---
name: hostel-tools
description: Understand and change Hostel tools - the defaults (android-sdk, android-emulator, xcode, ios-simulator, macos-desktop, windows-desktop), mise tools (jdk:21, node:22), what each tool needs on the machine, options, and your own tools with their scripts. Use it when the user says "add iOS/Android to a machine", "what does the Mac need for the simulator", "Android emulator does not start", "add a phone or tablet", "make my own tool" (for example a tvOS simulator), or "change the install/start script".
---

# Tools

A tool is something that a machine gives to a lease. A guest asks for the tools
that it needs (`hostel acquire android-sdk jdk`), and the server finds a machine
that has all of them. There are two kinds:

| Kind | Examples | In a lease |
|---|---|---|
| Software | `android-sdk`, `xcode`, `jdk`, `node`, `dotnet` | Always usable on a machine that has it, also when the lease did not ask for it. Asking for it only selects such a machine. |
| Screen tool | `android-emulator`, `ios-simulator`, `macos-desktop`, `windows-desktop` | Starts only for a lease that asks for it. Hostel takes screenshots of it, has its driver ready, and cleans it up at the end. |

An **option** follows a colon. For software it is a version (`node:22`,
`xcode:26`): a request matches the whole version or its first parts (`xcode:26`
matches `26.1`). For a screen tool it is a phone or tablet
(`android-emulator:pixel-9`); without one, the lease gets the first option.

The OS is a property of the machine, not a tool. Guests do not ask for an OS.
The tools of a repository's mise files (node, pnpm, Python) are not machine
tools: they are set up for each lease on every machine (`hostel-use`).

```sh
hostel tools                  # for everyone: options, screen, machines, free rooms, waiting, wait
hostel tools android-sdk      # one tool: needs, environment, machines, its GUIDE.md
hostel config tools           # for admins: NAME, SCREEN, MACHINES, SOURCE
hostel config machines           # the machines with their tools
```

## Tools on a machine

A machine lists its tools in `tools` (`hostel-machines`):

```toml
tools = ["xcode", "ios-simulator", "android-emulator", "jdk:21", "macos-desktop"]
```

- An entry is `name` or `name:option`. A defined tool (below) needs no option;
  `android-emulator:pixel-9` gives only that option.
- **Any mise tool** is a tool without a definition. Give its version: `jdk:21`,
  `node:22`, `python:3.13`, `go:1.25` (`jdk` is the mise tool `java`). A machine
  can list several versions (`node:20`, `node:22`). The server installs them
  with mise when it provisions the machine.
- **Needs are added.** A machine that lists `android-emulator` also gets
  `android-sdk`, and with it `jdk` (the latest version, unless the machine
  lists one).
- Change the list: `hostel config machines set mac-mini tools=xcode,ios-simulator,jdk:21`.
- An unknown tool, a tool without a script for the machine's OS (for example
  `windows-desktop` on Linux), or a tool name with a typing error fails the
  check at once.

## The default tools

The binary has these tools built in.

| Tool | Kind | Options | Machine OS | What the machine needs |
|---|---|---|---|---|
| `android-sdk` | software; needs `jdk` | the SDK platform (36) | Linux or macOS | Nothing: `install` downloads the SDK into the cache (about 1 GB). Linux: the emulator's Ubuntu packages, or `sudo` without a password. Optional `env`: `HOSTEL_ANDROID_API` (36). |
| `android-emulator` | screen; needs `android-sdk` | `pixel-9`, `pixel-tablet` (max 2) | Linux or macOS | Linux: `/dev/kvm` usable by the machine user. The first install downloads the system image (about 4 GB; timeout 30 min). Optional `env`: `HOSTEL_ANDROID_TAG` (`google_apis`), `HOSTEL_ANDROID_PORT` (5554). |
| `xcode` | software | the Xcode version | macOS | Xcode, installed by a person (it installs nothing). Optional `env`: `DEVELOPER_DIR` to select one of several. |
| `ios-simulator` | screen; needs `xcode` | `iphone-17-pro`, `ipad-pro-13` (max 2) | macOS | An iOS Simulator runtime. Optional `env`: `HOSTEL_IOS_RUNTIME` (for example `26.5`). |
| `macos-desktop` | screen; whole machine | none | macOS | An automatic login of the machine user, an unlocked screen, Screen Recording for `/usr/libexec/sshd-keygen-wrapper`, and Automation Mode without a password (`sudo automationmodetool enable-automationmode-without-authentication`). |
| `windows-desktop` | screen; whole machine | none | Windows | The "Hostel desktop" task in a signed-in, unlocked desktop session. |

What the tools give to commands:

- `android-sdk`: `ANDROID_HOME`; `adb`, `sdkmanager`, and `emulator` on `PATH`.
- `android-emulator`: read-only emulators from a clean snapshot;
  `ANDROID_SERIAL` (first option) and `ANDROID_SERIALS`.
- `ios-simulator`: new, clean simulators for each lease; `SIMULATOR_UDID`
  (first option) and `SIMULATOR_UDIDS`.
- `macos-desktop`, `windows-desktop`: the lease takes the whole machine.

Each machine also gets `agent-device` (tap, type, read the screen) and, on
Windows, `winapp`. `mise.agent_device = false` (`hostel config settings set`)
turns this off.

The full Mac and Windows preparation (users, Remote Login, auto login, the
desktop task) is in `docs/static-machine-setup.md`.

## Your own tool

```sh
hostel config tools new tvos-sim ./tvos-sim --os macos --screen   # a template folder (--os linux|macos|windows)
# edit ./tvos-sim/tool.toml, GUIDE.md, and the scripts
hostel config tools push ./tvos-sim                    # check and save; the name is the folder name
hostel config machines set mac-mini tools=xcode,ios-simulator,tvos-sim   # let a machine give it
```

Change a default tool:

```sh
hostel config tools pull ios-simulator ./sim    # the current files into a new folder
hostel config tools push ./sim --name ios-simulator   # save your version (source: changed)
hostel config tools reset ios-simulator         # back to the version of this release
hostel config tools remove tvos-sim             # refused while a machine lists it
```

`push --name <n>` saves the folder under another name. A push applies at once.
A free machine whose tools changed installs again. A push sends only `tool.toml`,
`GUIDE.md`, and scripts.

### `tool.toml`

```toml
description = "tvOS Simulator"
needs = ["xcode"]                  # tools that this tool uses; a machine that lists it gets them too
screen = true                      # a screen tool: start, screenshot, and stop for each lease
options = ["apple-tv-4k"]          # screen tools: the first is the default
max_options = 1                    # options in one lease (default 1)
whole_machine = false              # true: the lease takes all rooms of the machine
env = { TVOS_HOME = "{cache}/tvos-sim" }   # {cache} is $HOSTEL_CACHE_DIR, {home} the machine user's home
path = ["{cache}/tvos-sim/bin"]
script_timeouts = { install = "20m", start = "5m" }   # defaults: install 20m, others 3m
```

- A software tool has no `options`: its option is the version that `check`
  prints.
- The `env` and `path` of a software tool apply to every lease on a machine
  that has it. Those of a screen tool apply when the lease started it.

### `GUIDE.md`

A short text for agents: what the tool gives, its environment variables, and
how to use it (build with it; start, see, and drive a screen). `hostel tools
<name>` and `hostel agent-guide` show it. Name every variable that `start`
writes.

### Scripts

File names: `<step>.<os>.sh` for `macos` and `linux`, `<step>.<os>.ps1` for
`windows`. The machine's OS selects the file. A misspelled script name is an
error. A tool is available on an OS when it has a script for that OS, or when
it has no scripts at all (a tool that only sets `env` and `path`).

| Step | When | Runs in |
|---|---|---|
| `install` | once for each machine, and again when the tool's files or the machine's options of a screen tool change (after the `install` of its needs) | `hostel/` |
| `check` | after install, after each cleanup, at repair, before a warm machine takes a lease: exit non-zero if the tool does not work; print the version (or several versions, or the options of a screen tool, comma-separated) on the last line | `hostel/` |
| `start` | screen tools only: each lease that asks for the tool, before it is ready, after the `start` of its needs; write the facts to `tool.env` | `rooms/<room>/` |
| `screenshot` | screen tools only: `hostel screenshot`; `$1` is the output PNG path, `$2` the option | `rooms/<room>/` |
| `stop` | screen tools only: at cleanup, in reverse order of `start`; stop and delete what `start` started | `rooms/<room>/` |

Rules:

- Every step is optional, except `start` for a screen tool on each OS that it
  is available on. A software tool must not have `start`, `screenshot`, or
  `stop`.
- Each step must be safe to run again.
- A non-zero exit code is a failure. The last 4 KB of output go to the event
  log (`hostel status <machine>`). A failed `start` stops the tools that already
  started for the lease.
- The helper runs `/bin/sh <script>` (no execute bit necessary) and stops its
  process group at the timeout.
- The machine's mise tools are installed before `install`, so scripts can use
  them. Keep downloads in `$HOSTEL_CACHE_DIR/<tool>`: it stays between leases.

Environment of the scripts: `HOSTEL_TOOL`, `HOSTEL_TOOL_ALL_OPTIONS` (the
options that the machine lists for the tool; all options in `tool.toml` when
the machine lists it without one), `HOSTEL_TOOLS` (all tools of the lease), `HOSTEL_ROOM`,
`HOSTEL_ROOM_DIR`, `HOSTEL_WORK_DIR`, `HOSTEL_CACHE_DIR`, `HOSTEL_LEASE_ID`, and
for `start`, `screenshot`, and `stop`: `HOSTEL_TOOL_OPTIONS` (the lease's
options of this tool, comma-separated) and `HOSTEL_EXCLUSIVE` (`1` for a
whole-machine lease). Also the `MISE_*` variables, the `env` and `path` of the
machine's software tools, the provider, machine, and room `env`, and `tool.env`
(for `screenshot` and `stop`). Use machine `env` to vary a script between
machines.

`tool.env` lines (`NAME=value`) go into the environment of every lease command.

## Check

```sh
hostel config tools
hostel admin config check     # on the server machine: the effective tools of each machine, with needs
hostel status mac-mini        # the inventory, and the events with install and check output
```
