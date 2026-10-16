package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// runAgent is the long-lived process every Mac in the pool runs. In Phase 1
// it only answers "how are you doing?" over HTTP. Later phases add discovery
// and job execution to this same process.
func runAgent(addr string) error {
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		s, err := readStatus()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	http.HandleFunc("/run", handleRun)
	http.HandleFunc("/jobs", handleJobs)
	http.HandleFunc("/jobs/", handleJobs)
	log.Printf("agent listening on %s  (try: curl %s/status)", addr, addr)
	return http.ListenAndServe(addr, nil)
}
