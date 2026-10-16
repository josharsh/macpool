package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/grandcat/zeroconf"
)

// serviceType is the mDNS service name every agent announces under.
// Convention is _<name>._<transport>, same as _http._tcp or _airplay._tcp.
const serviceType = "_macpool._tcp"

// announce registers this agent on the local network via mDNS (Bonjour).
// No central server: the agent multicasts "I exist, I'm on this port" to
// 224.0.0.251:5353 and answers anyone who asks for _macpool._tcp.
// The returned function withdraws the record; call it on shutdown.
func announce(port int) (func(), error) {
	host, _ := os.Hostname()
	// Instance name must be unique on the network. host+port lets two agents
	// on one Mac (for testing) coexist.
	instance := fmt.Sprintf("%s-%d", host, port)
	srv, err := zeroconf.Register(instance, serviceType, "local.", port, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("mdns register: %w", err)
	}
	return srv.Shutdown, nil
}

// Node is one discovered agent: where it is, and what it last reported.
type Node struct {
	Addr   string // host:port of the agent's HTTP server
	Status *Status
	Err    error
}

// discover browses the network for `wait` and returns every agent that
// answered. Discovery only tells you an address exists; it says nothing
// about whether the agent is healthy or has room. That is what /status is for.
func discover(wait time.Duration) ([]Node, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mdns resolver: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := resolver.Browse(ctx, serviceType, "local.", entries); err != nil {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}

	seen := map[string]bool{}
	var nodes []Node
	for e := range entries { // channel closes when ctx times out
		if len(e.AddrIPv4) == 0 {
			continue
		}
		addr := net.JoinHostPort(e.AddrIPv4[0].String(), strconv.Itoa(e.Port))
		if seen[addr] {
			continue
		}
		seen[addr] = true
		nodes = append(nodes, Node{Addr: addr})
	}
	return nodes, nil
}
