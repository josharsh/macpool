package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// worker is one node that has our bundle unpacked and its runtime ready.
type worker struct {
	node     Node
	jobID    string
	inflight int
	done     int
}

type execRequest struct {
	Cmd string            `json:"cmd"`
	Env map[string]string `json:"env"`
}

// runJobFile is the client side of a job: bundle the directory, set it up on
// every eligible node, then either run the command once or fan it out over
// the input CSV with a concurrency cap. Returns the process exit code.
func runJobFile(path string, wait time.Duration) (int, error) {
	start := time.Now()
	job, err := loadJob(path)
	if err != nil {
		return 1, err
	}
	abs, _ := filepath.Abs(path)
	dir := filepath.Dir(abs)

	var header []string
	var rows []map[string]string
	if job.Input != "" {
		if header, rows, err = readCSV(filepath.Join(dir, job.Input)); err != nil {
			return 1, err
		}
	}
	secrets := map[string]string{}
	for _, k := range job.Secrets {
		v, ok := os.LookupEnv(k)
		if !ok {
			return 1, fmt.Errorf("secret %s is not set in your shell", k)
		}
		secrets[k] = v
	}
	bundle, err := tarDir(dir, job.Output)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "bundle %s → %.0f KB\n", dir, float64(len(bundle))/1024)

	// --- find nodes that fit ---
	nodes, err := discover(wait)
	if err != nil {
		return 1, err
	}
	need := uint64(job.MemoryGB * float64(1<<30))
	var eligible []Node
	for i := range nodes {
		nodes[i].Status, nodes[i].Err = fetchStatus(nodes[i].Addr)
		n := nodes[i]
		switch {
		case n.Err != nil:
			fmt.Fprintf(os.Stderr, "  - %s: %v\n", n.Addr, n.Err)
		case !n.Status.OnAC:
			fmt.Fprintf(os.Stderr, "  - %s (%s): on battery\n", n.Addr, n.Status.Hostname)
		case n.Status.MemAvail < need:
			fmt.Fprintf(os.Stderr, "  - %s (%s): only %s available\n", n.Addr, n.Status.Hostname, gb(n.Status.MemAvail))
		default:
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		return 1, fmt.Errorf("no eligible node among %d found: need on AC with ≥ %s available", len(nodes), gb(need))
	}

	// --- upload + runtime setup on every eligible node, in parallel ---
	var mu sync.Mutex
	var workers []*worker
	var wg sync.WaitGroup
	for _, n := range eligible {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			t := time.Now()
			id, status, err := uploadBundle(n.Addr, job, bundle)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s (%s): %v\n", n.Addr, n.Status.Hostname, err)
				return
			}
			fmt.Fprintf(os.Stderr, "  ✓ %s (%s) runtime %s in %.1fs\n", n.Addr, n.Status.Hostname, status, time.Since(t).Seconds())
			workers = append(workers, &worker{node: n, jobID: id})
		}(n)
	}
	wg.Wait()
	if len(workers) == 0 {
		return 1, errors.New("setup failed on every node")
	}

	// --- single command: stream it live, like `macpool run "cmd"` ---
	if job.Input == "" {
		w := workers[0]
		env := map[string]string{}
		for k, v := range secrets {
			env[k] = v
		}
		fmt.Fprintf(os.Stderr, "→ %s (%s)\n", w.node.Addr, w.node.Status.Hostname)
		return postStream(execURL(w), execRequest{Cmd: job.Command, Env: env}, os.Stdout)
	}

	// --- fan out: one task per row ---
	results := make([]map[string]string, len(rows))
	fails := make([]string, len(rows))
	done := 0
	leastBusy := func() *worker {
		mu.Lock()
		defer mu.Unlock()
		best := workers[0]
		for _, w := range workers[1:] {
			if w.inflight < best.inflight ||
				(w.inflight == best.inflight && w.node.Status.MemAvail > best.node.Status.MemAvail) {
				best = w
			}
		}
		best.inflight++
		return best
	}
	runTask := func(i int) {
		rowJSON, _ := json.Marshal(rows[i])
		env := map[string]string{"MACPOOL_ROW": string(rowJSON), "MACPOOL_TASK": fmt.Sprint(i)}
		for k, v := range secrets {
			env[k] = v
		}
		cmd := strings.ReplaceAll(job.Command, "{row}", shellQuote(string(rowJSON)))
		w := leastBusy()
		var out bytes.Buffer
		code, err := postStream(execURL(w), execRequest{Cmd: cmd, Env: env}, &out)

		mu.Lock()
		defer mu.Unlock()
		w.inflight--
		w.done++
		done++
		switch {
		case err != nil:
			fails[i] = err.Error()
		case code != 0:
			fails[i] = fmt.Sprintf("exit %d: %s", code, lastLines(out.String(), 3))
		default:
			if results[i] = lastJSONLine(out.String()); results[i] == nil {
				fails[i] = "no JSON object on the last line of stdout"
			}
		}
		fmt.Fprintf(os.Stderr, "\r  %d/%d tasks", done, len(rows))
	}
	tasks := make(chan int)
	conc := min(job.Concurrency, len(rows))
	for c := 0; c < conc; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range tasks {
				runTask(i)
			}
		}()
	}
	for i := range rows {
		tasks <- i
	}
	close(tasks)
	wg.Wait()
	fmt.Fprintln(os.Stderr)

	// --- merge results back into the CSV, in original row order ---
	if job.Output != "" {
		have := map[string]bool{}
		for _, h := range header {
			have[h] = true
		}
		var newCols []string
		for _, r := range results {
			for k := range r {
				if !have[k] {
					have[k] = true
					newCols = append(newCols, k)
				}
			}
		}
		sort.Strings(newCols)
		merged := make([]map[string]string, len(rows))
		for i, row := range rows {
			m := map[string]string{}
			for k, v := range row {
				m[k] = v
			}
			for k, v := range results[i] {
				m[k] = v
			}
			merged[i] = m
		}
		outPath := filepath.Join(dir, job.Output)
		if err := writeCSV(outPath, append(header, newCols...), merged); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (+%d column(s): %s)\n", outPath, len(newCols), strings.Join(newCols, ", "))
	}

	// --- summary ---
	nfail := 0
	for i, f := range fails {
		if f != "" {
			nfail++
			fmt.Fprintf(os.Stderr, "  row %d failed: %s\n", i+1, f)
		}
	}
	var parts []string
	for _, w := range workers {
		parts = append(parts, fmt.Sprintf("%s=%d", w.node.Addr, w.done))
	}
	fmt.Fprintf(os.Stderr, "%d tasks, %d failed, %.1fs, per node: %s\n", len(rows), nfail, time.Since(start).Seconds(), strings.Join(parts, " "))
	if nfail > 0 {
		fmt.Fprintf(os.Stderr, "job dirs kept on the nodes for inspection (~/.macpool/jobs/%s-*)\n", job.Name)
		return 1, nil
	}
	for _, w := range workers {
		req, _ := http.NewRequest(http.MethodDelete, "http://"+w.node.Addr+"/jobs/"+w.jobID, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return 0, nil
}

func execURL(w *worker) string { return "http://" + w.node.Addr + "/jobs/" + w.jobID + "/exec" }

func uploadBundle(addr string, job Job, bundle []byte) (id, status string, err error) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/jobs", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Job-Name", job.Name)
	req.Header.Set("X-Runtime", job.Runtime)
	req.Header.Set("X-Deps", job.depsHeader())
	resp, err := http.DefaultClient.Do(req) // no timeout: pip install can be slow
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New(strings.TrimSpace(buf.String()))
	}
	var r struct{ ID, Runtime string }
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		return "", "", err
	}
	return r.ID, r.Runtime, nil
}

// lastJSONLine parses the last non-empty stdout line as a JSON object of
// strings. That line is the task's result; everything above it is logging.
func lastJSONLine(out string) map[string]string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var raw map[string]any
	if json.Unmarshal([]byte(lines[len(lines)-1]), &raw) != nil {
		return nil
	}
	m := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			m[k] = s
		} else {
			b, _ := json.Marshal(v)
			m[k] = string(b)
		}
	}
	return m
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
