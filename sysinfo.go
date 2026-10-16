package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Status is what one Mac reports about itself. Later phases ship this over
// the network so a scheduler can decide "does this job fit here?".
// Version is reported by every agent. Clients refuse nodes on a different
// version instead of discovering the mismatch as a 404 halfway through.
const Version = "0.4"

type Status struct {
	Version   string    `json:"version"`
	Hostname  string    `json:"hostname"`
	Arch      string    `json:"arch"`
	CPUs      int       `json:"cpus"`
	Load1     float64   `json:"load1"`     // 1-minute load average
	MemTotal  uint64    `json:"mem_total"` // bytes
	MemAvail  uint64    `json:"mem_avail"` // bytes we could realistically hand to a job
	MemWired  uint64    `json:"mem_wired"` // bytes the kernel will never give up
	MemCompr  uint64    `json:"mem_compr"` // bytes sitting in the memory compressor
	OnAC      bool      `json:"on_ac"`     // plugged in?
	Battery   int       `json:"battery"`   // percent, -1 if no battery
	Timestamp time.Time `json:"timestamp"`
}

func readStatus() (Status, error) {
	s := Status{Version: Version, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Timestamp: time.Now()}
	s.Hostname, _ = os.Hostname()

	total, err := sysctlUint("hw.memsize")
	if err != nil {
		return s, err
	}
	s.MemTotal = total

	if err := readVMStat(&s); err != nil {
		return s, err
	}
	if err := readLoad(&s); err != nil {
		return s, err
	}
	readPower(&s)
	return s, nil
}

func sysctlUint(key string) (uint64, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", key, err)
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// readVMStat parses `vm_stat`. macOS does not have a "free memory" number
// the way Linux does. It keeps "free" near zero on purpose and parks recently
// used pages as "inactive" so they can be reused instantly. So what a job can
// actually get is: free + inactive + speculative + purgeable.
func readVMStat(s *Status) error {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return fmt.Errorf("vm_stat: %w", err)
	}
	pages := map[string]uint64{}
	var pageSize uint64 = 16384
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "(page size of 16384 bytes)"
			if i := strings.Index(line, "page size of "); i >= 0 {
				fields := strings.Fields(line[i+len("page size of "):])
				pageSize, _ = strconv.ParseUint(fields[0], 10, 64)
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(val), "."), 10, 64)
		if err != nil {
			continue
		}
		pages[strings.TrimSpace(key)] = n
	}
	avail := pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"] + pages["Pages purgeable"]
	s.MemAvail = avail * pageSize
	s.MemWired = pages["Pages wired down"] * pageSize
	s.MemCompr = pages["Pages occupied by compressor"] * pageSize
	return nil
}

// readLoad parses `sysctl -n vm.loadavg`, which looks like "{ 2.34 2.50 2.60 }".
func readLoad(s *Status) error {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return fmt.Errorf("loadavg: %w", err)
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(fields) < 1 {
		return fmt.Errorf("loadavg: unexpected output %q", out)
	}
	s.Load1, err = strconv.ParseFloat(fields[0], 64)
	return err
}

// readPower parses `pmset -g batt`. Desktops (Mac mini) have no battery line;
// they are always on AC.
func readPower(s *Status) {
	s.Battery = -1
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		s.OnAC = true
		return
	}
	text := string(out)
	s.OnAC = strings.Contains(text, "AC Power")
	if i := strings.Index(text, "%"); i > 0 {
		j := strings.LastIndexAny(text[:i], "\t ")
		if j >= 0 {
			if pct, err := strconv.Atoi(text[j+1 : i]); err == nil {
				s.Battery = pct
			}
		}
	}
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

func (s Status) String() string {
	power := "battery"
	if s.OnAC {
		power = "AC"
	}
	bat := ""
	if s.Battery >= 0 {
		bat = fmt.Sprintf(" (%d%%)", s.Battery)
	}
	return fmt.Sprintf(
		"%s  %s  %d cpus  load %.2f\n  mem total %s  avail %s  wired %s  compressed %s\n  power %s%s",
		s.Hostname, s.Arch, s.CPUs, s.Load1,
		gb(s.MemTotal), gb(s.MemAvail), gb(s.MemWired), gb(s.MemCompr),
		power, bat,
	)
}
