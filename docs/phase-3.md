# Phase 3 — The first job on the pool

**Goal:** `macpool run "<command>"` picks the best Mac, runs the command there, streams its
output to your terminal as it happens, and exits with the command's exit code. One job,
one node, no retries, no durability. That comes later.

## What got built

```
run.go     handleRun()   agent: POST /run → exec the command, stream output, trailer = exit code
           pick()        client: the scheduler (filter + score), 15 lines
           submit()      client: POST, copy body to stdout, read trailer
main.go    `macpool run [-node host:port] "cmd"`
agent.go   registers /run
```

Try it (restart your agents first, they're running old binaries, see Gotcha):

```
./macpool run "hostname; echo hi; exit 3"; echo "exit=$?"
→ 192.168.0.150:7782 (Harshs-MacBook-Pro.local, avail 3.4 GB)     ← stderr
Harshs-MacBook-Pro.local                                          ← stdout, from the node
hi
exit=3

./macpool run 'for i in 1 2 3; do echo tick $i; sleep 1; done'    # lines arrive one per second
./macpool run "sleep 100"   then Ctrl-C                            # sleep dies on the node too
```

**Gotcha:** every agent you started before this phase answers `404 page not found` to `/run`
because that binary doesn't have the handler. Rebuild, then restart each one:
`kill $(lsof -t -i :7777 -i :7782); go build -o macpool . && ./macpool agent`.
This is the "rolling upgrade" problem in miniature: the network has nodes on different
versions, and the client has no idea. Real systems put a version in the status response
and refuse to schedule onto mismatches. We'll add that when it bites a second time.

## The concepts

### 1. What "run a process" actually is

`exec.Command("/bin/sh", "-c", cmd)` does two syscalls: **fork** clones the agent process,
and **exec** replaces the clone's memory with `/bin/sh`. The shell then forks again for each
command in your string. So `sleep 100` is agent → sh → sleep, three processes. `ps -o pid,ppid,command`
on the node while a job runs shows the tree.

### 2. Why `sh -c` and what it costs

Passing the string to a shell gives you pipes, `&&`, globs, env vars, for free. The cost: it is
arbitrary code execution on the node. For a pool of Macs owned by one team that's acceptable
and it is what CI runners do. Phase 4's job spec will narrow this to a declared runtime; the
shell stays as the escape hatch.

### 3. Streaming through HTTP

HTTP normally sends a body with a known length. We don't know the length, the job is still
running. Two mechanisms make it work:

- **Chunked transfer encoding.** Go switches to it automatically when you write without a
  Content-Length. Each `Write` becomes a chunk the client can read immediately.
- **Flush.** Go's response writer buffers 4 KB before sending. `flushWriter` calls `Flush()`
  after every write so `echo tick 1` reaches your terminal before `sleep 1` finishes. Remove
  the flush and all three ticks arrive together after 3 seconds. Try it.

### 4. The exit code arrives in a trailer

Headers go before the body, but the exit code only exists after the body. HTTP has a feature
almost nobody uses for exactly this: **trailers**, headers sent after the last chunk. The agent
declares `Trailer: X-Exit-Code` up front and sets the value at the end. On the client,
`resp.Trailer` is empty until you've read the body to EOF, which is why `submit` copies the body
first and reads the trailer second. gRPC uses trailers for status codes for the same reason.

### 5. Killing the job when the client leaves

You Ctrl-C the CLI. The TCP connection drops. Go cancels `r.Context()`. `exec.CommandContext`
sees that and calls `cmd.Cancel`. Ours kills `-pid`: the negative pid means "the whole process
group". Without `Setpgid: true` and the negative pid, we'd kill `sh` and leave `sleep` orphaned,
running for 100 seconds on someone else's Mac. Test 3 in the verification below proves zero
orphans. This single detail is the difference between a toy and something you'd run on a
coworker's machine.

### 6. The scheduler is a filter and a score

`pick()`: drop nodes that are unreachable or on battery, keep the one with the most available
memory. That's it. Every scheduler, Kubernetes included, is this shape: predicates that say
"not here", then a priority that ranks the rest. Ours will grow a "does the job's declared
memory fit" predicate in Phase 4, and a "fewest running jobs" tie-break once nodes report that.

### 7. What's wrong with it, on purpose

- **Race.** Two people run `macpool run` at the same second. Both discover, both see the same
  node has the most room, both pick it. Nothing coordinates. Fixing this needs one brain that
  hands out placements, which is the control plane in Phase 5.
- **No record.** If your terminal dies, the job is killed and nothing remembers it ran. Phase 5.
- **Discovery every time.** Two seconds of mDNS wait per command. Fine now, annoying at 50 jobs.

## Do these by hand before Phase 4

1. `./macpool run "sleep 60"` then, on the node, `ps -o pid,ppid,pgid,command | grep -E 'sleep|sh -c|macpool'`.
   Note that sh and sleep share a `pgid` and it is not the agent's. Ctrl-C the CLI, run `ps` again.
2. Comment out the `fw.f.Flush()` line, rebuild, rerun the tick loop. Watch streaming break.
   Put it back.
3. `./macpool run "exit 42"; echo $?` and `./macpool run "nonexistent-cmd"; echo $?`. Explain
   the two different codes.
4. Unplug your laptop, run `./macpool run hostname` with only that laptop's agent up. Read the error.
5. Run `curl -N -X POST localhost:7781/run -d '{"cmd":"date; sleep 2; date"}' -D -` and find
   `Transfer-Encoding: chunked` and `Trailer: X-Exit-Code` in the headers yourself.

## What Phase 4 adds

The job file. `macpool run job.yml` with a runtime, deps, memory requirement, and an input CSV
that fans out into one task per row. The poem experiment, end to end.
