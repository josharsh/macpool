package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// ---------- agent side: actually running a command ----------

type runRequest struct {
	Cmd string `json:"cmd"`
}

// handleRun executes one shell command and streams its output back as the
// HTTP response body while it runs. The exit code travels in an HTTP trailer,
// a header sent *after* the body, because we don't know it until the end.
func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		http.Error(w, "body must be {\"cmd\": \"...\"}", http.StatusBadRequest)
		return
	}
	execStream(w, r, "", req.Cmd, nil)
}

// execStream runs `sh -c command` in dir with env (nil = inherit) and streams
// its output into the response. Shared by /run and /jobs/{id}/exec.
func execStream(w http.ResponseWriter, r *http.Request, dir, command string, env []string) {
	// CommandContext ties the process to the request: if the client goes away,
	// ctx is cancelled and Go kills the process for us.
	cmd := exec.CommandContext(r.Context(), "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	// `sh -c` forks children. Killing sh alone would orphan them. Putting the
	// whole thing in its own process group lets us kill the group (negative pid).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Trailer", "X-Exit-Code")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// Both stdout and stderr go to the response. flushWriter pushes every
	// chunk to the network immediately instead of buffering until the end.
	out := &flushWriter{w: w, f: flusher}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode() // process ran and failed
	default:
		code = -1 // couldn't start, or killed
		fmt.Fprintf(out, "macpool: %v\n", err)
	}
	w.Header().Set("X-Exit-Code", strconv.Itoa(code))
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// ---------- client side: choosing a node and submitting ----------

// pick is the whole scheduler for now: filter, then score.
// Filter: reachable and on AC power (a laptop on battery is about to leave).
// Score: most available memory wins.
func pick(nodes []Node) (Node, error) {
	var best Node
	for _, n := range nodes {
		if n.Err != nil || !n.Status.OnAC {
			continue
		}
		if best.Status == nil || n.Status.MemAvail > best.Status.MemAvail {
			best = n
		}
	}
	if best.Status == nil {
		return Node{}, errors.New("no eligible node: need a reachable agent on AC power")
	}
	return best, nil
}

// submit sends the command to one agent and copies its output to our
// stdout as it arrives. Returns the job's exit code.
func submit(addr, command string) (int, error) {
	return postStream("http://"+addr+"/run", runRequest{Cmd: command}, os.Stdout)
}

// postStream POSTs a JSON body, copies the streamed response into out, and
// returns the exit code from the trailer.
func postStream(url string, body any, out io.Writer) (int, error) {
	b, _ := json.Marshal(body)
	// No client timeout: a job may legitimately run for an hour.
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return -1, fmt.Errorf("%s: %s", url, bytes.TrimSpace(msg))
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return -1, err
	}
	// Trailers are only populated once the body has been read to EOF.
	code, err := strconv.Atoi(resp.Trailer.Get("X-Exit-Code"))
	if err != nil {
		return -1, errors.New("agent did not report an exit code (connection dropped?)")
	}
	return code, nil
}
