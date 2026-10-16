package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "agent":
		fs := flag.NewFlagSet("agent", flag.ExitOnError)
		addr := fs.String("addr", ":7777", "address to listen on")
		fs.Parse(os.Args[2:])
		_, portStr, _ := strings.Cut(*addr, ":")
		port, err := strconv.Atoi(portStr)
		if err != nil {
			fatal(fmt.Errorf("bad -addr %q: need host:port", *addr))
		}
		stop, err := announce(port)
		if err != nil {
			fatal(err)
		}
		defer stop()
		if err := runAgent(*addr); err != nil {
			fatal(err)
		}
	case "nodes":
		fs := flag.NewFlagSet("nodes", flag.ExitOnError)
		wait := fs.Duration("wait", 2*time.Second, "how long to listen for agents")
		fs.Parse(os.Args[2:])
		nodes, err := discover(*wait)
		if err != nil {
			fatal(err)
		}
		if len(nodes) == 0 {
			fmt.Println("no agents found (is `macpool agent` running on this network?)")
			return
		}
		for i := range nodes {
			nodes[i].Status, nodes[i].Err = fetchStatus(nodes[i].Addr)
		}
		printNodes(nodes)
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		wait := fs.Duration("wait", 2*time.Second, "how long to listen for agents")
		nodeAddr := fs.String("node", "", "skip scheduling, run on this host:port")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			fatal(fmt.Errorf("usage: macpool run [-node host:port] \"<command>\""))
		}
		command := fs.Arg(0)
		if strings.HasSuffix(command, ".yml") || strings.HasSuffix(command, ".yaml") {
			code, err := runJobFile(command, *wait)
			if err != nil {
				fatal(err)
			}
			os.Exit(code)
		}

		addr := *nodeAddr
		if addr == "" {
			nodes, err := discover(*wait)
			if err != nil {
				fatal(err)
			}
			for i := range nodes {
				nodes[i].Status, nodes[i].Err = fetchStatus(nodes[i].Addr)
			}
			best, err := pick(nodes)
			if err != nil {
				fatal(err)
			}
			addr = best.Addr
			// stderr, so stdout stays clean for piping the job's real output
			fmt.Fprintf(os.Stderr, "→ %s (%s, avail %s)\n", best.Addr, best.Status.Hostname, gb(best.Status.MemAvail))
		}
		code, err := submit(addr, command)
		if err != nil {
			fatal(err)
		}
		os.Exit(code)
	case "status":
		// `macpool status` reads this machine directly.
		// `macpool status host:7777` asks a running agent instead.
		if len(os.Args) > 2 {
			s, err := fetchStatus(os.Args[2])
			if err != nil {
				fatal(err)
			}
			fmt.Println(s)
			return
		}
		s, err := readStatus()
		if err != nil {
			fatal(err)
		}
		fmt.Println(s)
	default:
		usage()
	}
}

func fetchStatus(addr string) (*Status, error) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	if s.Version != Version {
		v := "v" + s.Version
		if s.Version == "" {
			v = "an older version"
		}
		return &s, fmt.Errorf("agent is %s, this CLI is v%s: restart the agent with the new binary", v, Version)
	}
	return &s, nil
}

func printNodes(nodes []Node) {
	fmt.Printf("%-22s %-26s %5s %6s %8s %8s\n", "ADDR", "HOST", "VER", "LOAD", "AVAIL", "POWER")
	for _, n := range nodes {
		if n.Err != nil {
			fmt.Printf("%-22s %-26s %s\n", n.Addr, "?", n.Err.Error())
			continue
		}
		s := n.Status
		power := "battery"
		if s.OnAC {
			power = "AC"
		}
		fmt.Printf("%-22s %-26s %5s %6.2f %8s %8s\n", n.Addr, s.Hostname, s.Version, s.Load1, gb(s.MemAvail), power)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  macpool agent [-addr :7777]   run the node agent (announces itself on the LAN)\n  macpool nodes [-wait 2s]      list every agent on the LAN\n  macpool run [-node h:p] \"cmd\"  run a shell command on the best node, stream output\n  macpool run job.yml           run a job file: bundle dir, fan out over input CSV\n  macpool status [host:port]    show this Mac, or ask a remote agent")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
