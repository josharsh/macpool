package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3918.
Pages active:                            244757.
Pages inactive:                          231278.
Pages speculative:                        12783.
Pages throttled:                              0.
Pages wired down:                        237371.
Pages purgeable:                              4.
"Translation faults":                 123456789.
Pages occupied by compressor:            426227.
`

func TestParseVMStat(t *testing.T) {
	avail, wired, compr := parseVMStat(vmStatSample)
	const page = 16384
	if want := uint64(3918+231278+12783+4) * page; avail != want {
		t.Errorf("avail = %d, want %d (free+inactive+speculative+purgeable)", avail, want)
	}
	if want := uint64(237371) * page; wired != want {
		t.Errorf("wired = %d, want %d", wired, want)
	}
	if want := uint64(426227) * page; compr != want {
		t.Errorf("compressed = %d, want %d", compr, want)
	}
}

func TestLastJSONLine(t *testing.T) {
	out := "working...\nstill working\n{\"w1\":\"harbor\",\"poem\":\"line one\\nline two\",\"n\":3}\n"
	got := lastJSONLine(out)
	if got["w1"] != "harbor" || got["poem"] != "line one\nline two" || got["n"] != "3" {
		t.Errorf("got %v", got)
	}
	if lastJSONLine("no json here\n") != nil {
		t.Error("plain text should not parse")
	}
	if lastJSONLine("{\"a\":1}\ntrailing log line\n") != nil {
		t.Error("only the LAST line counts")
	}
}

func TestShellQuote(t *testing.T) {
	in := `{"w1":"it's","w2":"a \"test\""}`
	want := `'{"w1":"it'\''s","w2":"a \"test\""}'`
	if got := shellQuote(in); got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "job.yml"), []byte("command: true\n"), 0o644)
	os.MkdirAll(filepath.Join(src, "lib"), 0o755)
	os.WriteFile(filepath.Join(src, "lib", "x.py"), []byte("print(1)\n"), 0o755)
	os.MkdirAll(filepath.Join(src, "node_modules", "big"), 0o755)
	os.WriteFile(filepath.Join(src, "node_modules", "big", "a.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(src, "out.csv"), []byte("old"), 0o644)

	bundle, err := tarDir(src, "out.csv")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := untar(bytes.NewReader(bundle), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "lib", "x.py")); string(b) != "print(1)\n" {
		t.Errorf("lib/x.py = %q", b)
	}
	if info, err := os.Stat(filepath.Join(dst, "lib", "x.py")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("executable bit lost: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); !os.IsNotExist(err) {
		t.Error("node_modules should be skipped")
	}
	if _, err := os.Stat(filepath.Join(dst, "out.csv")); !os.IsNotExist(err) {
		t.Error("the job's output file should be skipped")
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	// A bundle whose entry escapes the target directory.
	var buf bytes.Buffer
	writeTarEntry(&buf, "../../evil.txt", "pwned")
	dst := t.TempDir()
	if err := untar(&buf, dst); err == nil {
		t.Fatal("expected an error for a ../ path")
	}
	if _, err := os.Stat(filepath.Join(dst, "..", "..", "evil.txt")); !os.IsNotExist(err) {
		t.Error("file was written outside the target directory")
	}
}

func TestLoadJobValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	if _, err := loadJob(write("a.yml", "runtime: python3\n")); err == nil {
		t.Error("missing command must fail")
	}
	if _, err := loadJob(write("b.yml", "command: true\nruntime: ruby\n")); err == nil {
		t.Error("unknown runtime must fail")
	}
	if _, err := loadJob(write("c.yml", "command: true\noutput: x.csv\n")); err == nil {
		t.Error("output without input must fail")
	}
	j, err := loadJob(write("poems.yml", "command: python3 p.py {row}\ninput: in.csv\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "poems" || j.Concurrency != 4 {
		t.Errorf("defaults: name=%q concurrency=%d", j.Name, j.Concurrency)
	}
}

func TestPick(t *testing.T) {
	gbs := func(n float64) uint64 { return uint64(n * (1 << 30)) }
	nodes := []Node{
		{Addr: "a", Status: &Status{OnAC: true, MemAvail: gbs(4)}},
		{Addr: "b", Status: &Status{OnAC: false, MemAvail: gbs(40)}}, // most memory, but on battery
		{Addr: "c", Status: &Status{OnAC: true, MemAvail: gbs(12)}},
		{Addr: "d", Err: os.ErrDeadlineExceeded},
	}
	best, err := pick(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if best.Addr != "c" {
		t.Errorf("picked %s, want c (most memory among reachable nodes on AC)", best.Addr)
	}
	if _, err := pick(nodes[1:2]); err == nil {
		t.Error("a battery-only pool must be refused")
	}
}
