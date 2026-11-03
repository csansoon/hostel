---
name: hostel-use
description: Use a Hostel room from a repository - acquire a lease, run builds and tests, sync files, take screenshots, open tunnels, and release. Use it when the user or agent says "build on iOS", "run the tests on Windows/Linux/macOS", "QA on the Android emulator", "take a screenshot of the simulator", "reach my local API from the room", "write hostel.toml", or "tell my agent to use Hostel".
---

# Use a room

The code stays on the local computer. Hostel sends it to the room, runs
commands there, and deletes the changes at the end of the lease. The computer
must be connected (`hostel-install`).

**`hostel agent-guide` is the source of truth.** Run it at the start of a task
and follow it. This skill summarizes it.

## Start

```sh
hostel platforms                 # PLATFORM, KIND, DEVICES, FREE, QUEUE, EST. WAIT
hostel status                    # rooms, leases, queues (plain text)
hostel acquire ios --task "QA the sign-in screen on iPhone"
```

One lease per platform per worktree. Run `acquire` in the repository. Flags:

| Flag | Meaning |
|---|---|
| `--task "..."` | Required: what the lease is for. Others see it. |
| `--device d` | A device of the platform; repeat for several (`hostel platforms` lists them). |
| `--require label` | A machine label, for example `desktop`; repeat for several. |
| `--priority low\|normal\|high` | The queue lane. Change it only when the user asks. |
| `--fresh` | A new on-demand room with empty caches. |
| `--wait 100s` | How long `acquire` waits before it returns. |

Exit codes of `acquire`:

| Code | Meaning | Do |
|---|---|---|
| 0 | The lease is ready, files are synced, and setup ran. | Work. |
| 75 | Not finished (queued or preparing). | Run the **same** command again. It continues the same lease. |
| 69 | No machine can serve the request (unknown platform, device, or label). | Do not retry. Tell the user. |
| 78 | This computer is not connected. | Do not connect yourself. Ask the user to run `hostel connect <invite code>`. |
| 1 | Error, for example the setup job failed (the lease stays ready). | Read the message; fix the code; `hostel sync --setup`. |

## Work

```sh
hostel run -- "make test"                 # one argument: runs with sh -c (PowerShell on Windows)
hostel run -- xcodebuild -version          # several arguments: no shell
hostel run --detach -- "npm run dev"       # start a job, print its ID
hostel jobs
hostel attach <job>
hostel stop <job>
hostel sync                                # send local changes again (after each edit)
hostel sync --setup                        # run the repository setup again
hostel diff                                # changes on the room since the last sync
hostel cp ./fixture.json :tmp/fixture.json # local -> room work directory
hostel cp :build/app.log ./app.log         # room -> local
hostel screenshot -o screen.png            # --device d for another device
hostel ssh                                 # interactive shell (ssh -- <command> for one command)
hostel ssh-config                          # an ssh_config file for ssh -F
```

`run` and `attach` always print a last line on stderr. Read it:

- `job <id> exited with code N`: the command ended; N is also the exit code.
- `job <id> is still running` (exit code 75): run `hostel attach <id>`. Do
  **not** run `hostel run` again: that starts a second job.

`--json` prints the result as JSON on stdout. `--platform <p>` selects the lease
when the worktree has several. On Windows, a background process ends with its
job: use `hostel run --detach` for a server.

Devices: commands get the device facts in their environment. iOS:
`SIMULATOR_UDID`, `SIMULATOR_UDIDS`. Android: `ANDROID_SERIAL`,
`ANDROID_SERIALS`, `ANDROID_HOME`. To tap, type, and read the screen, use
`agent-device` on the room (`hostel run -- agent-device help`). Desktop apps:
`hostel acquire macos --require desktop` or `hostel acquire windows --require
desktop`; the agent guide shows the commands (`open`, `desktop`, `winapp`).

## Tunnels and forwards

A tunnel makes a local server reachable on the room at
`localhost:$HOSTEL_TUNNEL_<NAME>_PORT`. Declare it in `hostel.toml`, or change it
during a lease:

```sh
hostel tunnel list
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
include = [".env"]               # extra files that git ignores (glob patterns)
# mise = false                   # skip the repository mise install

[tunnels.api]                    # room -> this computer
local_port_env = "PORT"          # or local_port = 3000 (exactly one)
# room_port = 4000               # fixed room port (1024-65535); default: from the slot's pool

[platforms.ios]
devices = ["iphone-17-pro"]      # default devices for this repository
# requires = ["xcode-26.5"]      # labels that this repository needs
setup = ["cd apps/ios && make deps"]
env = { API_URL = "http://localhost:{tunnels.api.port}" }

[platforms.android]
env = { API_URL = "http://10.0.2.2:{tunnels.api.port}" }
```

- Files: tracked and untracked files that git does not ignore, plus `include`.
- Setup: `mise install` in the work directory (the repository's `mise.toml`,
  `.tool-versions`, `.nvmrc`, ...), then each `setup` command in order. The
  first failure stops it.
- `{tunnels.<name>.port}` becomes the tunnel's room port.
- Do not put `desktop` in `requires`: every lease would take the whole machine.
- Check the file: `hostel admin config check --repo hostel.toml`.

## The queue

Each platform has one queue. Order: leases that an admin placed, then the lane
(`high`, `normal`, `low`), then the request time. An old `low` lease stays behind
a new `normal` one. Admins move leases and change lanes in the app (Lobby).
`hostel status ios` prints the queue of a platform.

## End

```sh
hostel release
```

A lease also ends after 10 minutes without a `hostel` command (`idle_timeout`),
and at `max_lease` (default 2 hours). Changes on the room are temporary: make
durable changes in local files. `hostel release <lease-id>` ends another lease
of yours (admins: any lease).

## Status

- `hostel status` prints plain text: rooms with their leases (user, task, time,
  lease ID), and the queues. `hostel status <room>` shows details and events;
  `hostel status <platform>` shows its queue. Agents use this.
- People run `hostel` alone for the Hostel app (Lobby, Rooms, Front desk).

## Instructions for the agent's file

Put this paragraph into the repository's `AGENTS.md` or `CLAUDE.md`:

```markdown
## Remote machines (Hostel)

For builds, tests, or UI QA on another platform (iOS, Android, macOS, Windows,
Linux), use Hostel. First run `hostel agent-guide` and follow it. Get a lease
with `hostel acquire <platform> --task "<what you do>"`; if it exits with 75, run
the same command again; with 69, stop and tell me; with 78, ask me to connect
this computer. Run commands with `hostel run -- "<command>"` and read its last
stderr line; run `hostel sync` after each local edit. Run `hostel release` when
you are done. Do not change `--priority` unless I ask.
```
