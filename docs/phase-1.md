# Phase 1 — A Mac that can describe itself

**Goal:** one binary, `macpool`, that answers "how are you doing?" for the machine
it runs on, both locally and over HTTP. Nothing distributed yet. Every later phase
builds on this process.

## What got built

```
macpool/
  main.go      CLI: `macpool agent`, `macpool status [host:port]`
  sysinfo.go   readStatus(): memory, load, power, cpus  ← the lesson is here
  agent.go     HTTP server with GET /status
```

Build and run:

```
go build -o macpool .
./macpool status                 # read this Mac directly
./macpool agent                  # start the daemon, listens on :7777
./macpool status localhost:7777  # ask the daemon instead
curl localhost:7777/status       # raw JSON
```

## The concepts, in the order you'll hit them

### 1. "Free memory" is a lie on macOS

Run `vm_stat`. On an 18 GB machine you will see "Pages free" around 4000 pages,
which is 64 MB. That is not because you're out of memory. macOS deliberately keeps
free memory near zero and parks recently used pages as **inactive**, so they can be
handed back to whoever touched them last without a disk read. Unused RAM is wasted RAM.

So the number a scheduler cares about, "how much could I give a new job right now",
is not `free`. It is:

```
avail = free + inactive + speculative + purgeable
```

The three buckets you cannot touch:

- **wired**: the kernel's own pages. Never paged out, never given up.
- **active**: pages a running process touched recently.
- **compressor**: pages macOS squeezed instead of swapping. On your machine this was
  6.8 GB, which means you were already under memory pressure when you ran it.

`sysinfo.go` computes `MemAvail` from exactly those four buckets. Page counts are in
pages, not bytes, so everything is multiplied by the page size the first line of
`vm_stat` reports (16384 on Apple silicon, 4096 on Intel).

### 2. Where the numbers come from

Three OS tools, all shelled out with `os/exec`:

- `sysctl -n hw.memsize` — total RAM in bytes. `sysctl` is the kernel's key-value store.
- `sysctl -n vm.loadavg` — load average, the number of runnable processes averaged over
  1, 5, 15 minutes. Load 5.3 on 11 CPUs means half the cores are busy.
- `pmset -g batt` — power source and battery percent. A Mac mini prints no battery line.

Shelling out is the honest v1. The "proper" way is the `host_statistics64` Mach call via
cgo, which is what Activity Monitor uses. Same numbers, more code. We'll switch only if
exec cost becomes a problem (it won't at one call per few seconds).

### 3. The agent is a process that never exits

`runAgent` starts an HTTP server and blocks forever. That is what a daemon is: a process
with no terminal, no user, that sits in a loop answering requests. `launchd` is how macOS
keeps such things alive across reboots; we'll add that when the agent is worth keeping alive.

### 4. Why HTTP and JSON for a local tool

Because in Phase 2 the thing calling `/status` will be another Mac. Starting with a
network boundary on day one, even when both ends are localhost, means the design never
has a "works only locally" assumption to unlearn.

## Do these by hand before Phase 2

1. Run `vm_stat 1` (refreshes every second) in one terminal. In another, open a big app
   or run `python3 -c "x = bytearray(2*1024**3); input()"`. Watch which buckets move:
   free → active, and if you keep going, compressor grows. Quit the app, watch pages
   land in **inactive**, not free.
2. Compare `./macpool status` with Activity Monitor's Memory tab. Our `avail` should be
   close to total minus "Memory Used". Explain any gap to yourself using the buckets above.
3. Unplug the charger and run `./macpool status`. `power` flips to `battery`. In Phase 3
   the scheduler will refuse to place jobs on a node in that state.
4. Start the agent, then run `lsof -i :7777`. That is the process-to-port mapping; a
   reverse proxy is nothing but a program that sits on one port and forwards to others.

## What Phase 2 adds

Discovery. Each agent announces itself on the local network with mDNS (Bonjour, the thing
that makes AirDrop and printers show up). `macpool nodes` lists every Mac running an agent
with its `avail` memory. That is the moment it stops being one machine.
