---
name: hostel-use
description: Use a Hostel room from a repository - acquire a lease with the tools that you need, run builds and tests, sync files, take screenshots, open tunnels, and release. Use it when the user or agent says "build the Android/iOS app", "run the tests on Windows/Linux/macOS", "QA on the Android emulator", "take a screenshot of the simulator", "reach my local API from the room", "write hostel.toml", or "tell my agent to use Hostel".
---

# Use a room

The code stays on the local computer. Hostel sends it to the room, runs
commands there, and deletes the changes at the end of the lease. The computer
must be connected (`hostel-install`).

**`hostel agent-guide` is the source of truth.** Run it at the start of a task
and follow it. It also has the guide of each tool of your server. This skill
summarizes it.

## The rule

- **Ask for the tools that you need.** `hostel tools` lists them;
  `hostel tools <tool>` shows one with its guide.
- **Ask for a screen tool** (an emulator, a simulator, a desktop) **only when
  you must see or drive the app.** It takes time and memory. A build needs no
  screen.
- **All software of the machine is usable in the lease**, also the tools that
  you did not ask for. Asking for a tool only selects a machine that has it.
- The tools of the repository's mise files (node, pnpm, Python) are set up on
  every machine. Do not ask for them.
- **One lease for each worktree.** For work on two machines at the same time,
  use a second worktree (`git worktree add`).
- **When the lease is ready, read the machine facts** that `acquire` prints:
  the OS, the hardware, all tools with their environment names, and the
  screens that started. `hostel info` prints them again.

## Start

```sh
hostel agent-guide               # the rules and the guide of each tool
hostel tools                     # TOOL, OPTIONS, SCREEN, MACHINES, FREE, WAITING, EST. WAIT
hostel status                    # rooms, leases, the waiting line (plain text)
hostel acquire android-sdk jdk --task "build the Android app"
```

Examples:

```sh
# Android: build without an emulator, then QA on an emulator (the SDK comes with it)
hostel acquire android-sdk jdk --task "build"
hostel run -- ./gradlew assembleDebug
hostel acquire android-emulator:pixel-9 --task "QA settings"

# iOS: build with Xcode, then QA on a simulator (Xcode comes with it)
hostel acquire xcode --task "build"
hostel acquire ios-simulator:iphone-17-pro --task "QA login"

# Desktop apps (the lease takes the whole machine)
hostel acquire xcode macos-desktop --task "QA the Mac app"
hostel acquire dotnet windows-desktop --task "QA the editor"

# Any free machine
hostel acquire --task "API tests"
```

A tool is `name` or `name:option`. The option is a version (`node:22`
matches `22.11.0`) or a phone or tablet of a screen tool. Without tools on the
command line, `acquire` uses `tools` of `hostel.toml`, else any machine. A
second `acquire` with other tools in the same worktree fails: release first.
Run `acquire` in the repository. Flags:

| Flag | Meaning |
|---|---|
| `--task "..."` | Required: what the lease is for. Others see it. |
| `--priority low\|normal\|high` | The lane in the waiting line. Change it only when the user asks. |
| `--fresh` | A new on-demand room with empty caches. |
| `--wait 100s` | How long `acquire` waits before it returns. |
| `--background` | Do not wait in the waiting line: return at once, and a small background process keeps the place. A lease that is ready goes on to the sync and the setup as usual. |

## Wait in the waiting line and keep working

The place in the waiting line belongs to the lease, and the lease ID is in the
worktree (`$(git rev-parse --git-dir)/hostel/lease/lease.json`). Running
the same `acquire` command again continues that lease in the same place; it
never takes another agent's place. Use one worktree for each agent: agents in
the same worktree share one lease.

A waiting lease lives only while its owner polls it: after `poll_timeout`
(default 5 minutes) without `acquire`, the server ends it. `acquire` prints the
time. Two ways to keep the place while you do other work:

```sh
# 1. Return at once and let Hostel keep the place:
hostel acquire ios-simulator --task "QA the sign-in screen" --background
# ... other work ...
hostel acquire ios-simulator --task "QA the sign-in screen"   # continue; wait until ready

# 2. Or return quickly and run the command again before the timeout:
hostel acquire ios-simulator --task "QA the sign-in screen" --wait 1s
```

With `--background`, a detached `hostel keep-place` process polls the lease
until it is ready or ends, then stops. Run the same `acquire` command again
at least every `idle_timeout` (acquire prints it; default 10 minutes): it
continues the lease and takes the room when it is ready.
A ready lease lives for `idle_timeout` (default 10 minutes) without a `hostel`
command, and it holds the room meanwhile. `hostel release` ends the lease and
the process.

On Windows, an agent tool can run each command in a job object that kills all
its processes when the command ends and does not let them leave. Then the
background process ends with the command. `acquire` notices it at the next
run: it says how long the server keeps the place, and `--background` starts a
new process. In such a tool, run `acquire` again before the poll timeout.

Exit codes of `acquire`:

| Code | Meaning | Do |
|---|---|---|
| 0 | The lease is ready, files are synced, and setup ran. | Read the machine facts. Work. |
| 75 | Not finished (waiting or preparing). | Run the **same** command again. It continues the same lease. |
| 69 | No machine can serve the request (an unknown tool or option, or no machine with all the tools together). The message says why. | Do not retry. Change the tools, or tell the user. |
| 78 | This computer is not connected. | Do not connect yourself. Ask the user to run `hostel connect <invite code>`. |
| 1 | Error, for example the setup job failed (the lease stays ready). | Read the message; fix the code; `hostel sync --setup`. |

## Work

`run` and `attach` wait 100 seconds and return with code 75 while the job goes
on. For a long build, keep each command shorter than the time limit of your own
shell tool: start it with `hostel run --detach`, then run
`hostel attach <job> --wait 9m` until the job ends. A running job alone does not
keep the lease; a running `attach` does. Between two commands, `idle_timeout`
applies.

```sh
hostel info                                # the machine facts of the lease again
hostel run -- "make test"                 # one argument: runs with sh -c (PowerShell on Windows)
hostel run -- xcodebuild -version          # several arguments: no shell
hostel run --detach -- "npm run dev"       # start a job, print its ID
hostel jobs
hostel attach <job> --wait 9m              # the output until the job ends or the wait ends
hostel stop <job>
hostel sync                                # send local changes again (after each edit)
hostel sync --setup                        # run the repository setup again
hostel diff                                # changes on the room since the last sync
hostel cp ./fixture.json :tmp/fixture.json # local -> room work directory
hostel cp :build/app.log ./app.log         # room -> local
hostel screenshot -o screen.png            # --tool android-emulator:pixel-tablet for another screen
hostel ssh                                 # interactive shell (ssh -- <command> for one command)
hostel ssh-config                          # an ssh_config file for ssh -F
```

`run` and `attach` always print a last line on stderr. Read it:

- `job <id> exited with code N`: the command ended; N is also the exit code.
- `job <id> is still running` (exit code 75): run `hostel attach <id>`. Do
  **not** run `hostel run` again: that starts a second job.

`--json` prints the result as JSON on stdout. On Windows, a background process
ends with its job: use `hostel run --detach` for a server.

Tool facts: commands get the environment of the machine's tools (for example
`ANDROID_HOME`) and the facts of the screens that started (`ANDROID_SERIAL`,
`SIMULATOR_UDID`). The guide of each tool names them. To tap, type, and read a
screen, use the driver that its guide names: `agent-device` on the room
(`hostel run -- agent-device help`), and `desktop` and `winapp` for
`windows-desktop`. `hostel screenshot` needs `--tool` only when the lease has
more than one screen.

## Tunnels and forwards

A tunnel makes a local server reachable on the room at
`localhost:$HOSTEL_TUNNEL_<NAME>_PORT`. Declare it in `hostel.toml`, or change it
during a lease:

```sh
hostel tunnel list                                # each tunnel: confirmed, or what is missing
hostel tunnel add api --local-port-env PORT       # or --local-port 3000; [--room-port P]
hostel tunnel set api --local-port 3001
hostel tunnel remove api
```

A forward is the other direction (a room port on this computer):

```sh
hostel forward 8080 --local-port 18080
hostel forward list
hostel forward stop 8080
```

## `hostel.toml` in the repository

```toml
tools = ["android-sdk", "jdk:21"]  # what "hostel acquire" asks for when no tools are given
include = [".env"]                 # extra files that git ignores (glob patterns)
setup = ["./gradlew --version"]    # after mise install, in every lease
env = { APP_ENV = "test" }         # in every lease
# mise = false                     # skip the repository mise install

[tunnels.api]                      # room -> this computer
local_port_env = "PORT"            # or local_port = 3000 (exactly one)
# room_port = 4000                 # fixed room port (1024-65535); default: from the slot's pool

[tool.android-emulator]            # only when the lease has this tool
env = { APP_API_BASE_URL = "http://10.0.2.2:{tunnels.api.port}" }

[tool.ios-simulator]
setup = ["tuist install && tuist generate --no-open"]
env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }
```

- Files: tracked and untracked files that git does not ignore, plus `include`.
- Setup: `mise install` in the work directory (the repository's `mise.toml`,
  `.tool-versions`, `.nvmrc`, ...), then each top-level `setup` command, then
  the `setup` of each `[tool.<name>]` of the lease. The first failure stops it.
- A `[tool.<name>]` applies when the lease has the tool: you asked for it, or a
  tool that you asked for needs it (`[tool.android-sdk]` applies to a lease for
  `android-emulator`).
- The `env` of a `[tool.<name>]` replaces keys of the top-level `env`.
- Tools on the command line replace `tools`.
- `{tunnels.<name>.port}` becomes the tunnel's room port.
- Do not put a desktop tool in `tools` unless every lease needs it: each such
  lease takes the whole machine.
- Check the file: `hostel admin config check --repo hostel.toml`.

## The waiting line

There is one waiting line for all tools. Order: leases that an admin placed,
then the lane (`high`, `normal`, `low`), then the request time. An old `low`
lease stays behind a new `normal` one. Each waiting lease takes the first free
machine that has its tools, so a lease for one machine does not block a lease
for another. `acquire` prints "waiting behind N leases". Admins move leases and
change lanes in the app (Lobby). `hostel status <tool>` prints the waiting
leases that asked for a tool.

## End

```sh
hostel release
```

A lease also ends after 10 minutes without a `hostel` command (`idle_timeout`),
and at `max_lease` (default 2 hours). Changes on the room are temporary: make
durable changes in local files. Cleanup does not delete the caches: mise tools,
`$HOSTEL_CACHE_DIR`, `~/.gradle`, and other files in the home directory of the
room stay, and the next lease can read them, also of another user. Never write a
secret there. `hostel release <lease-id>` ends another lease
of yours (admins: any lease).

If a command prints `run "hostel update"`, tell the user. Do not update
`hostel` yourself: other agents on the computer use the same binary.

## Status

- `hostel status` prints plain text: rooms with their leases (user, task,
  tools, time, lease ID), and the waiting line. `hostel status <room>` shows
  details, the inventory, and events; `hostel status <tool>` shows the waiting
  leases for a tool. Agents use this.
- People run `hostel` alone for the Hostel app (Lobby, Rooms, Front desk).

## Instructions for the agent's file

Put this paragraph into the repository's `AGENTS.md` or `CLAUDE.md`:

```markdown
## Remote machines (Hostel)

For builds, tests, or UI QA on another machine (Android, iOS, macOS, Windows,
Linux), use Hostel. First run `hostel agent-guide` and follow it. Ask only for
the tools that you need, for example `hostel acquire android-sdk jdk --task
"<what you do>"`; ask for a screen tool (`android-emulator`, `ios-simulator`)
only when you must see the app. If acquire exits with 75, run the same command
again; with 69, change the tools or tell me; with 78, ask me to connect this
computer. Run commands with `hostel run -- "<command>"` and read its last
stderr line; run `hostel sync` after each local edit. Run `hostel release` when
you are done. Do not change `--priority` unless I ask.
```
