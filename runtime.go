package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// poolRoot is where an agent keeps job directories and cached runtimes.
func poolRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".macpool")
}

// lockDir takes an exclusive advisory lock on dir+".lock" so that only one
// setup of this environment runs at a time, even across processes (two agents
// on one Mac, or an agent restarting mid-install). flock is a kernel primitive:
// the lock dies with the process, so a crash can never leave it held.
// Returns the unlock function.
func lockDir(dir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// prepareRuntime makes sure the node has an environment with these deps and
// returns the env vars that put it on PATH. Environments are cached by a hash
// of (runtime, deps), the same idea as a Docker layer: identical inputs,
// reuse the output. First job pays for pip install; every later job is free.
func prepareRuntime(runtime string, deps []string) (env []string, status string, err error) {
	if runtime == "" || runtime == "sh" {
		return nil, "none", nil
	}
	sum := sha256.Sum256([]byte(runtime + "\x00" + strings.Join(deps, "\x00")))
	dir := filepath.Join(poolRoot(), "envs", runtime+"-"+hex.EncodeToString(sum[:6]))
	marker := filepath.Join(dir, ".ready")

	switch runtime {
	case "python3":
		env = []string{"VIRTUAL_ENV=" + dir, "PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH")}
	case "node":
		mods := filepath.Join(dir, "node_modules")
		env = []string{"NODE_PATH=" + mods, "PATH=" + filepath.Join(mods, ".bin") + ":" + os.Getenv("PATH")}
	}

	unlock, err := lockDir(dir)
	if err != nil {
		return nil, "", err
	}
	defer unlock()
	if _, err := os.Stat(marker); err == nil {
		return env, "cached", nil
	}
	os.RemoveAll(dir) // half-finished attempt from a crash

	var out []byte
	switch runtime {
	case "python3":
		out, err = exec.Command("python3", "-m", "venv", dir).CombinedOutput()
		if err == nil && len(deps) > 0 {
			args := append([]string{"-m", "pip", "install", "-q", "--disable-pip-version-check"}, deps...)
			out, err = exec.Command(filepath.Join(dir, "bin", "python3"), args...).CombinedOutput()
		}
	case "node":
		err = os.MkdirAll(dir, 0o755)
		if err == nil && len(deps) > 0 {
			args := append([]string{"install", "--prefix", dir, "--silent", "--no-audit", "--no-fund"}, deps...)
			out, err = exec.Command("npm", args...).CombinedOutput()
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, "", fmt.Errorf("setting up %s with %v: %w\n%s", runtime, deps, err, strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return nil, "", err
	}
	return env, "installed", nil
}
