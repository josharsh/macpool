# Phase 2 — Macs that find each other

**Goal:** `macpool nodes` lists every Mac on the Wi-Fi that is running `macpool agent`,
with its available memory. No server, no config, no IP addresses typed anywhere.

## What got built

```
discovery.go   announce(port)      agent says "I exist" on the LAN
               discover(wait)      CLI listens for `wait` and collects addresses
main.go        `macpool nodes`     discover → fetch /status from each → print table
               `macpool agent`     now calls announce() before listening
```

Try it with two agents on one machine (ports must differ):

```
./macpool agent -addr :7781     # terminal 1
./macpool agent -addr :7782     # terminal 2
./macpool nodes                 # terminal 3
```

```
ADDR                   HOST                       LOAD    AVAIL  POWER
192.168.0.150:7782     Harshs-MacBook-Pro.local   2.95   4.0 GB     AC
192.168.0.150:7781     Harshs-MacBook-Pro.local   2.95   4.0 GB     AC
```

Ctrl-C one agent, run `nodes` again, it's gone. On a second Mac on the same Wi-Fi,
`./macpool agent` with the default port, and it shows up in the same table.

**Gotcha you already hit:** the agent you started in Phase 1 is still running on :7777
with the *old* binary, which doesn't announce itself. Ctrl-C it and start it again, or
`kill $(lsof -t -i :7777)`.

## The concepts

### 1. Multicast: talking to everyone without knowing anyone

Normal networking (unicast) needs a destination address. Discovery is the chicken-and-egg
problem: you want addresses, but you need an address to ask. Multicast solves it. Every
machine on the LAN that cares joins the group `224.0.0.251`, and a packet sent to that
address on UDP port 5353 is delivered to all of them. That protocol is **mDNS** (multicast
DNS). Apple calls it Bonjour. It is how AirDrop, AirPlay, printers, and `Harshs-MacBook-Pro.local`
all work.

Two message types matter:

- **Announce** (`zeroconf.Register`): "service `_macpool._tcp` instance `Harshs-MacBook-Pro-7781`
  is at 192.168.0.150 port 7781." Sent when the agent starts, and re-sent on request.
- **Query** (`resolver.Browse`): "anyone offering `_macpool._tcp`?" Every agent replies.

`_macpool._tcp` follows the DNS-SD naming convention `_<service>._<transport>`. Run
`dns-sd -B _http._tcp` and `dns-sd -B _airplay._tcp` to see what else is on your network.

### 2. Discovery is not health

`discover()` returns addresses, nothing else. An mDNS record says "someone announced this",
not "it is alive and has room". That's why `nodes` makes a second trip, a plain HTTP GET to
`/status`, for every address. Phase 3's scheduler will do the same: discover to find
candidates, then ask each one before placing a job. Keep the two ideas separate in your head;
systems that conflate "registered" with "healthy" are how you end up sending traffic to dead
machines.

### 3. Why `wait` exists

There is no "end of list" in multicast. You ask, replies trickle in over a few hundred
milliseconds, and you stop listening whenever you decide. `-wait 2s` is that decision.
Too short and you miss slow nodes; too long and the CLI feels sluggish. Every discovery
system has this knob somewhere.

### 4. Where this breaks, so you aren't surprised later

- **Multicast does not cross routers.** Two Macs on different subnets or VLANs will never see
  each other. Office Wi-Fi with "client isolation" turned on blocks it entirely. When that
  happens we add a `--join 192.168.0.150:7777` fallback: tell one node about another by hand.
  Every real system (Consul, Kubernetes) ends up with both: auto-discovery where it works,
  explicit seeds where it doesn't.
- **Stale records.** If an agent is killed with `kill -9` it never sends its goodbye packet.
  Other machines may keep the record until its TTL (default 120s) expires. `nodes` would show
  it as `unreachable`, which is correct and why step 2 exists.
- **Rosetta note.** The Go toolchain on this Mac is amd64 under Rosetta. Works, but an arm64
  Go would be cleaner. Not worth fixing now.

### 5. One dependency

`github.com/grandcat/zeroconf` does the mDNS packet encoding. We could write it ourselves
(it's DNS packets over UDP multicast, ~600 lines), and it would be a good exercise once the
rest of the system exists. Not now.

## Do these by hand before Phase 3

1. Start one agent, then run `dns-sd -B _macpool._tcp local` in another terminal. That is
   Apple's own browser seeing your service. Start a second agent and watch it appear live.
2. `sudo tcpdump -i en0 -n udp port 5353` while you run `./macpool nodes`. You will see your
   query go out to 224.0.0.251 and each agent's answer come back. That's the whole protocol.
3. Kill an agent with `kill -9` instead of Ctrl-C, then `./macpool nodes` immediately. Does it
   show as unreachable or disappear? Why?
4. If you have a second Mac: run the agent there, `nodes` from here. If they don't see each
   other, that's section 4 above. Tell me, and `--join` moves up the schedule.

## What Phase 3 adds

`macpool run "python3 -c 'print(42)'"` picks the node with the most available memory that is
on AC power, sends the command over HTTP, runs it with `os/exec`, and streams stdout back to
your terminal. The first job on the pool.
