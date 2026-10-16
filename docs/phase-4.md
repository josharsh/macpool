# Phase 4 — The job file, and the poem experiment

**Goal:** `macpool run job.yml`. Your directory is shipped to every eligible Mac, each one
prepares the runtime once, then the input CSV fans out into one task per row across the
pool with a concurrency cap, and the results come back as a new column. The poem
experiment runs end to end.

## What got built

```
job.go       Job struct + YAML loading, CSV read/write, tarDir/untar (the bundle)
runtime.go   prepareRuntime(): cached venv / node_modules per (runtime, deps), flock'd
jobs.go      agent: POST /jobs (upload+setup), POST /jobs/{id}/exec, DELETE /jobs/{id}
fanout.go    client: eligibility → parallel setup → worker pool → merge CSV → summary
run.go       execStream() now shared by /run and /jobs/{id}/exec; postStream() client helper
sysinfo.go   Version const; CLI refuses agents on another version
examples/    poems/ (the real thing, needs ANTHROPIC_API_KEY)   shout/ (keyless smoke test)
```

Run the smoke test first, it needs no key:

```
./macpool run examples/shout/job.yml
bundle .../examples/shout → 1 KB
  ✓ 192.168.0.150:7791 (Harshs-MacBook-Pro.local) runtime installed in 1.6s
  ✓ 192.168.0.150:7792 (Harshs-MacBook-Pro.local) runtime cached in 1.6s      ← waited on the flock, then found it ready
  8/8 tasks
wrote .../examples/shout/shouted.csv (+2 column(s): ran_on, shout)
8 tasks, 0 failed, 5.0s, per node: 192.168.0.150:7791=4 192.168.0.150:7792=4
```

Then the real one:

```
export ANTHROPIC_API_KEY=sk-ant-...
./macpool run examples/poems/job.yml
cat examples/poems/poems.csv
```

## The job file, field by field, and what each one made us build

```yaml
name: poems
runtime: python3          → prepareRuntime(): a venv on the node, cached by hash
deps: [anthropic]         → pip install, once per node per unique deps list
input: words.csv          → readCSV → one task per row
command: python3 poem.py {row}   → {row} becomes the row as shell-quoted JSON
output: poems.csv         → last stdout line of each task, parsed as JSON, merged into the row
memory_gb: 0.5            → eligibility predicate: node.MemAvail >= 0.5 GB
concurrency: 5            → size of the worker pool
secrets: [ANTHROPIC_API_KEY]  → read from YOUR shell, sent per task, never written on the node
```

Contract for the script: print anything you like to stderr, print **one JSON object as the
last line of stdout**. Keys that aren't already columns become new columns. That's the entire
protocol between the pool and your code, and it's why `poem.py` has no import of anything
macpool-related.

## The concepts

### 1. Shipping code: the bundle is a tarball

`tarDir` walks the job's directory and writes a gzipped tar into memory, skipping `.git`,
`.venv`, `node_modules`. The agent unpacks it into `~/.macpool/jobs/<name>-<rand>/` and runs
the command there. This is exactly how a Docker build context, a Heroku slug, and a Lambda
zip work. Note the `..` check in `untar`: without it a bundle could write to
`~/.ssh/authorized_keys` on the node. Every unpacker in history has had this bug once.

### 2. Runtimes are content-addressed

`prepareRuntime` hashes `(runtime, deps)` and keeps the venv at
`~/.macpool/envs/python3-<hash>/`. Same inputs, same hash, reuse. Change one dep, new hash,
new venv, old one stays for jobs still using it. This is the idea behind Docker layers, Nix,
and every build cache: identify an artifact by what produced it, not by a name someone chose.
The first run above paid 1.6s for `python3 -m venv`; the second paid nothing.

### 3. The bug we hit: two processes, one directory

Two agents on this Mac received the same job at the same moment and both started building
the same venv. One got `File exists`, the other saw its half-built directory deleted under it.
An in-process mutex can't help; they are different processes. The fix is `flock(2)`, an
advisory lock the kernel holds on an open file. The second process blocks until the first
releases, and if the first crashes the kernel drops the lock. `lockDir` is 12 lines.
With one agent per Mac you'd never see this. With an agent restarting mid-install, you would.

### 4. Eligibility, then placement

Scheduling now has two stages. **Eligibility** happens once per job: on AC, same version,
at least `memory_gb` available. **Placement** happens per task: `leastBusy()` picks the
worker with the fewest in-flight tasks, tie-broken by free memory. The summary line shows
the split, 4 and 4 above. With one slow node you'll see it drift, because a node that
finishes faster asks for more work. That's work-stealing in its simplest form.

### 5. Secrets

Your shell has the key. The node must never persist it. So: the client reads it from `os.Environ`,
sends it in the exec request body, the agent puts it in the child's environment slice and
nowhere else. Known weakness, stated plainly: the request travels over plain HTTP on your LAN.
Anyone on the Wi-Fi with Wireshark can read it. TLS between CLI and agents is on the list,
not in this phase.

### 6. Version skew bit us again, so it's fixed

Your old agents on :7777 and :7782 answered 404 to the new endpoints. Now every status
carries a `version`, the CLI refuses mismatches with a message that says what to do, and
`nodes` shows a VER column. Kubernetes does the same with a skew policy. The lesson:
anything with more than one node has multiple versions in flight at once, always.

### 7. What is still wrong

- **No retries.** A rate-limited API call fails the row. Phase 5 adds per-task retry and,
  more importantly, memory of what finished so a second `run` only does the failed rows.
- **The client is the brain.** If your terminal dies, in-flight tasks are killed and nothing
  records which rows completed. Phase 5 moves that record into a control plane.
- **Plain HTTP.** See section 5.
- **Setup is synchronous.** A 90-second `pip install` looks like a hang. Streaming setup logs
  is a small change we'll make when a real dep list makes it annoying.

## Do these by hand before Phase 5

1. Run `shout` with `concurrency: 1`, then `8`. Compare the seconds. Each task sleeps 0.5s,
   so the math should be obvious; check that it is.
2. `ls ~/.macpool/envs/` and `cat ~/.macpool/envs/*/pyvenv.cfg`. Add `deps: [requests]` to
   shout's job.yml, run again, `ls` again. Two envs now. Remove the dep, run: which one is used?
3. Edit `shout.py` so row 3 does `sys.exit(2)`. Run. Read the failure line, open `shouted.csv`,
   see the blank cell, and find the job dir kept on the node for inspection.
4. `curl -X POST localhost:7791/jobs --data-binary @<(cd examples/shout && tar czf - .) -H 'X-Job-Name: manual'`
   then `ls ~/.macpool/jobs/`. You've just done what the CLI does, by hand.
5. Run the poem job with your key. Then run it again and watch `runtime cached`.
6. `sudo tcpdump -i lo0 -A port 7791 | grep ANTHROPIC` while running poems against :7791.
   That's section 5 made visible. It should bother you.

## What Phase 5 adds

A control plane. One process remembers every job and every task's result in SQLite, hands
out placements so two CLIs can't collide, retries failed tasks, and lets `macpool run` resume
a job that was half done when your laptop closed. The Temporal idea, in its smallest form.
