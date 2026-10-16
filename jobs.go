package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------- agent side: job directories ----------

// jobMeta is what the agent remembers about an uploaded job between the
// upload and the exec calls: the env that activates its runtime.
type jobMeta struct {
	Env []string `json:"env"`
}

var safeName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// handleJobs serves:
//
//	POST /jobs             body = tar.gz bundle, headers X-Job-Name, X-Runtime, X-Deps
//	POST /jobs/{id}/exec   body = {"cmd": "...", "env": {...}}  → streamed output, trailer exit code
//	DELETE /jobs/{id}      remove the job directory
func handleJobs(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/jobs")
	if r.Method == http.MethodDelete {
		id := strings.TrimPrefix(rest, "/")
		if id == "" || safeName.MatchString(id) {
			http.Error(w, "bad job id", http.StatusBadRequest)
			return
		}
		os.RemoveAll(filepath.Join(poolRoot(), "jobs", id))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case rest == "" || rest == "/":
		uploadJob(w, r)
	case strings.HasSuffix(rest, "/exec"):
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/exec")
		execJob(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func uploadJob(w http.ResponseWriter, r *http.Request) {
	name := safeName.ReplaceAllString(r.Header.Get("X-Job-Name"), "-")
	if name == "" {
		name = "job"
	}
	var b [4]byte
	rand.Read(b[:])
	id := name + "-" + hex.EncodeToString(b[:])
	dir := filepath.Join(poolRoot(), "jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := untar(r.Body, dir); err != nil {
		os.RemoveAll(dir)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var deps []string
	if d := r.Header.Get("X-Deps"); d != "" {
		deps = strings.Split(d, ",")
	}
	env, status, err := prepareRuntime(r.Header.Get("X-Runtime"), deps)
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	meta, _ := json.Marshal(jobMeta{Env: env})
	os.WriteFile(filepath.Join(dir, ".macpool.json"), meta, 0o644)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "runtime": status})
}

func execJob(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || safeName.MatchString(id) {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(poolRoot(), "jobs", id)
	metaBytes, err := os.ReadFile(filepath.Join(dir, ".macpool.json"))
	if err != nil {
		http.Error(w, "unknown job "+id, http.StatusNotFound)
		return
	}
	var meta jobMeta
	json.Unmarshal(metaBytes, &meta)

	var req struct {
		Cmd string            `json:"cmd"`
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		http.Error(w, "body must be {\"cmd\": \"...\", \"env\": {...}}", http.StatusBadRequest)
		return
	}
	// Later entries win on duplicate keys, so the runtime's PATH beats the
	// agent's, and the task's env beats both. Secrets live only in this slice.
	env := append(os.Environ(), meta.Env...)
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, "MACPOOL_JOB_DIR="+dir)
	execStream(w, r, dir, req.Cmd, env)
}

func (j Job) depsHeader() string { return strings.Join(j.Deps, ",") }

// shellQuote wraps s in single quotes so the shell passes it through verbatim.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
