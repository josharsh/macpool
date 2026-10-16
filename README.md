# macpool

Any Macs on the same network, used as one computer.

Every Mac has memory and CPU that sit idle most of the day. macpool is a small program
you run on each of them. The Macs find each other over the local network, and from any
one of them you can hand work to the whole group with a single command. There is no
server to set up, no accounts, no config file with IP addresses in it. If a Mac is on
the network and running macpool, it is part of the pool.

It is built for the kind of work most of us actually have: a few hundred or a few
thousand small, independent tasks. Run a script against every row in a CSV. Call an
LLM once per document. Run a test suite against forty repositories. Re-encode a folder
of videos. Each task fits comfortably on one Mac; there are just a lot of them, and one
Mac at a time is slow.

## What it looks like

Start the agent on every Mac that should take work:

```
macpool agent
```

See who is around:

```
$ macpool nodes
ADDR                 HOST               VER   LOAD    AVAIL   POWER
192.168.0.150:7777   studio.local       0.4   1.20  41.3 GB      AC
192.168.0.153:7777   riya-mbp.local     0.4   3.02   6.8 GB      AC
192.168.0.161:7777   mini-2.local       0.4   0.44  12.1 GB      AC
```

Run one command on whichever Mac has the most room:

```
$ macpool run "ffmpeg -i big.mov -vcodec h264 small.mp4"
→ 192.168.0.150:7777 (studio.local, avail 41.3 GB)
...
```

Or describe a job in a file and let the pool fan it out:

```yaml
# job.yml
runtime: python3
deps: [anthropic]
input: words.csv            # one task per row
command: python3 poem.py {row}
output: poems.csv           # each task's result lands back in its row
memory_gb: 0.5
concurrency: 5
secrets: [ANTHROPIC_API_KEY]
```

```
$ macpool run job.yml
bundle ./poems → 2 KB
  ✓ 192.168.0.150:7777 (studio.local)   runtime installed in 3.1s
  ✓ 192.168.0.161:7777 (mini-2.local)   runtime cached in 0.0s
  50/50 tasks
wrote poems.csv (+1 column: poem)
50 tasks, 0 failed, 14.2s, per node: 192.168.0.150:7777=27 192.168.0.161:7777=23
```

Your directory is shipped to each Mac, the Python environment is created once and
cached, the rows are split across the Macs that have room, and the output CSV comes back
with a new column. The script itself knows nothing about macpool. It reads one row,
prints one line of JSON.

## How it works, briefly

Each agent is a plain process that announces itself with mDNS (Bonjour, the same thing
that makes AirDrop and printers appear) and answers HTTP on one port. It reports what
macOS actually has to give: not "free memory", which macOS keeps near zero on purpose,
but free plus inactive plus purgeable pages, which is what a new process can really get.

The command-line tool is the scheduler. It asks every agent for its status, drops the ones
on battery or without enough memory, ships your files as a tarball, and sends tasks to the
agent with the fewest in flight. Output streams back over the same HTTP connection as it
happens, and the exit code arrives in an HTTP trailer when the task ends. If you Ctrl-C,
the task is killed on the remote Mac too, including anything it spawned.

Runtimes are cached by a hash of their dependency list, so the first job that needs
`anthropic` pays for `pip install` and every later one does not. That is the same idea
as a Docker layer, in about sixty lines.

## What it is not

It is not one big computer. Memory is not pooled across machines; a task has to fit on a
single Mac. Nothing here helps you run a model that is larger than your largest machine.

It does not run containers. Tasks run as ordinary processes on the remote Mac, inside a
job directory, with the declared runtime on PATH. That is deliberate: it keeps the GPU
and Metal available for local inference, and it keeps the whole thing small enough to
read in an afternoon. It also means the pool is for people who trust each other. A task
can do anything the agent's user can do.

Traffic between the CLI and agents is plain HTTP on your LAN. Secrets you declare in the
job file travel in a request body. Fine on a network you control, not fine on a café's
Wi-Fi. TLS is on the list.

mDNS does not cross routers. Macs on different subnets, or on Wi-Fi with client
isolation, will not see each other. An explicit `--join host:port` fallback is also on
the list.

## Status

Early and honest about it. Everything above works and is exercised end to end, and this
is the whole current feature set. There is no central record of jobs yet: the CLI is the
brain, so if your terminal dies mid-job, in-flight tasks are killed and completed rows are
not remembered. Retries, resuming a half-finished job, and a control plane that lets two
people submit at once without colliding are the next phase.

The project is also a deliberately hands-on way to learn platform engineering, which is
why there is a `docs/phase-N.md` for every step with the concepts it introduces and
exercises to do by hand. If you want to understand how a scheduler, a process supervisor,
and a tiny bit of a reverse proxy work from the inside, those are the place to start.

## Install

Requires Go 1.21 or newer and macOS. One binary, no runtime dependencies.

```
git clone https://github.com/josharsh/macpool
cd macpool
go build -o macpool .
./macpool agent            # on every Mac in the pool
./macpool nodes            # from any of them
```

Python 3 (ships with macOS) is needed on nodes for `runtime: python3`; Node for
`runtime: node`.

## Job file reference

| field | meaning |
|---|---|
| `name` | job name; defaults to the file name |
| `runtime` | `python3`, `node`, or `sh` (default) |
| `deps` | pip or npm packages to install into a cached environment |
| `input` | CSV file; one task per row. Omit to run `command` once |
| `command` | shell command. `{row}` is replaced with the row as JSON; also available as `$MACPOOL_ROW` |
| `output` | CSV file. The last stdout line of each task is parsed as a JSON object and merged into its row |
| `memory_gb` | a node must have at least this much available to be eligible |
| `concurrency` | maximum tasks in flight across the whole pool |
| `secrets` | environment variable names copied from your shell into each task |

## License

MIT
