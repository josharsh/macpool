package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Job is the job file. Every field answers a question the node can't guess:
// what to run, with which runtime, how big it is, what one unit of work is.
type Job struct {
	Name        string   `yaml:"name"`
	Runtime     string   `yaml:"runtime"` // python3 | node | sh (default sh)
	Deps        []string `yaml:"deps"`    // pip or npm packages
	Input       string   `yaml:"input"`   // CSV; one task per row
	Command     string   `yaml:"command"` // {row} is replaced by the row as JSON
	Output      string   `yaml:"output"`  // CSV; each task's last stdout line (JSON) merged into its row
	MemoryGB    float64  `yaml:"memory_gb"`
	Concurrency int      `yaml:"concurrency"`
	Secrets     []string `yaml:"secrets"` // env var names copied from your shell to the task
}

func loadJob(path string) (Job, error) {
	var j Job
	b, err := os.ReadFile(path)
	if err != nil {
		return j, err
	}
	if err := yaml.Unmarshal(b, &j); err != nil {
		return j, fmt.Errorf("%s: %w", path, err)
	}
	if j.Command == "" {
		return j, fmt.Errorf("%s: command is required", path)
	}
	if j.Name == "" {
		j.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if j.Concurrency <= 0 {
		j.Concurrency = 4
	}
	switch j.Runtime {
	case "", "sh", "python3", "node":
	default:
		return j, fmt.Errorf("%s: runtime %q: use python3, node, or sh", path, j.Runtime)
	}
	if j.Output != "" && j.Input == "" {
		return j, errors.New("output requires input")
	}
	return j, nil
}

func readCSV(path string) (header []string, rows []map[string]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, nil, fmt.Errorf("%s: need a header row and at least one data row", path)
	}
	header = recs[0]
	for _, rec := range recs[1:] {
		row := map[string]string{}
		for i, col := range header {
			if i < len(rec) {
				row[col] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return header, rows, nil
}

func writeCSV(path string, header []string, rows []map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		rec := make([]string, len(header))
		for i, col := range header {
			rec[i] = row[col]
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// Directories never worth shipping to a node.
var bundleSkip = map[string]bool{".git": true, ".venv": true, "node_modules": true, "__pycache__": true}

// tarDir packs a directory into a gzipped tar in memory. This is the whole
// "ship my code to the node" mechanism: the same format Docker uses for layers.
func tarDir(dir string, skipFiles ...string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if bundleSkip[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		for _, s := range skipFiles {
			if s != "" && rel == s {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil // symlinks, sockets: skip
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	tw.Close()
	gz.Close()
	return buf.Bytes(), err
}

// untar unpacks a bundle into dir. The ".." check matters: a malicious bundle
// could otherwise write to ~/.ssh/authorized_keys on the node.
func untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("bundle is not gzip: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("refusing path %q in bundle", hdr.Name)
		}
		target := filepath.Join(dir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}
