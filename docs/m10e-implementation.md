# M10e implementation plan: tools

Status: approved (2026-11-05), phase 1. Milestone M10e in `plan.md`.
Owner decisions: `requirements.md`, section 3, 2026-11-05 ("tools").

## 1. Why

An agent built Android code for a real app with Hostel (report of 2026-11-05).
It worked, but it lost most of its time on one wrong choice: it built on
`linux` leases, which have no Android SDK, and not on `android` leases, which
always start an emulator. The cause is in the model, not in Android:

- **A platform has three meanings in one word:** the software that a lease gets
  (installed by the platform scripts, but declared nowhere), the resources of one
  lease (an `android` lease always gets an emulator: the first device is the
  default), and a queue with its machines.
- **The server never says what a lease contains.** The agent sees a name
  (`android`) and a description ("Android Emulator") and must guess. `ANDROID_HOME`
  exists only on `android` leases, also on a machine that has the SDK on disk.
- **Facts about machines are labels without meaning** (`xcode-26.5`, `desktop`,
  `kvm`). The Windows and Mac desktops hide behind the label `desktop`.

The fix is general: the server declares what each machine gives, the agent asks
for what it needs, and a lease starts only what the agent asked for.

## 2. The model

One new concept: the **tool**. The words platform, device, and labels leave the
vocabulary.

- **Tool:** something that a machine gives to a lease: software (`android-sdk`,
  `jdk`, `xcode`, `dotnet`, `node`, `docker`) or a screen (`android-emulator`,
  `ios-simulator`, `macos-desktop`, `windows-desktop`).
- **Option:** a version of a software tool, or the phone or tablet of an
  emulator or simulator, after a colon: `node:22`, `xcode:26`,
  `android-emulator:pixel-9`. An option is a filter: `node` matches any machine
  with any `node`, `node:22` only machines whose `node` matches `22`. A version
  option matches the whole version or its first parts (`xcode:26` matches `26.1`
  and `26.2`; `node:22` matches `22.11.0`, not `2.2`). The option of a screen
  tool matches its name exactly; without one, the lease gets the first option
  that the machine has.
- **Machine:** it lists the tools that it gives. The server installs them,
  checks them, and keeps the real options (versions, phones and tablets) of each room (the
  **inventory**).
- **Lease:** one slot of one machine that has all the requested tools. The
  software of the machine is always usable in the lease, also the tools that the
  lease did not ask for: asking for `android-sdk` only means "a machine that has
  it". Only screen tools start on request (they cost time and memory).
- **Screen tool:** a tool that the agent looks at and acts on. Hostel starts it
  for each lease, cleans it up at the end, takes screenshots of it, and has its
  driver ready (`agent-device`; `desktop` and `winapp` on Windows). A screen tool
  can take the whole machine (the desktops: there is one screen).
- **The OS is a property of the machine, not a tool.** Agents do not ask for
  an OS: tools work the same way on each OS that has them. Only the desktops
  (`macos-desktop`, `windows-desktop`) belong to one OS by nature. The lease
  shows the OS (section 6).

What stays: machines, rooms (one runtime instance of a machine), slots, leases,
providers, the waiting line and its lanes, sync, jobs, tunnels, cleanup. Tools
from the repository (`mise.toml`: node, pnpm, Python) stay as they are: set up
for each lease on any machine, and cached.

### The agent's view

```
$ hostel tools
TOOL               OPTIONS                      SCREEN  MACHINES                FREE  WAITING
android-sdk        36                                   home/macbook, ns/linux  2     0
android-emulator   pixel-9, pixel-tablet        yes     home/macbook, ns/linux  2     0
jdk                21                                   home/macbook, ns/linux  2     0
xcode              26.1                                 home/macbook            1     0
ios-simulator      iphone-17-pro, ipad-pro-13   yes     home/macbook            1     0
macos-desktop                                   yes     home/macbook            1     0
windows-desktop                                 yes     home/win                1     0
dotnet             9.0.100                              home/win                1     0
From the repository's mise files, on every machine: node, pnpm (and any mise tool).

$ hostel tools android-sdk
android-sdk 36: Android SDK (build tools, platform tools, emulator binaries)
  needs        jdk
  environment  ANDROID_HOME; adb, sdkmanager, emulator on PATH
  machines     home/macbook (36, macos), ns/linux (36, linux; on demand)
  use          Build with Gradle: hostel run -- ./gradlew assembleDebug
```

```
hostel acquire android-sdk jdk --task "build"                 # no emulator
hostel acquire android-emulator:pixel-9 --task "QA settings"  # the SDK comes with it (needs)
hostel acquire ios-simulator:iphone-17-pro --task "QA login"  # xcode comes with it
hostel acquire dotnet windows-desktop --task "QA the editor"
hostel acquire xcode macos-desktop --task "QA the Mac app"
hostel acquire --task "API tests"                             # any free machine
```

- `hostel.toml` declares the usual set of the repository (`tools = [...]`), so
  `hostel acquire --task "..."` is enough. Tools on the command line replace it.
- **A request that no machine can serve fails at once** (owner decision), with
  the reason: "no machine has xcode and android-sdk together; machines with
  xcode: home/macbook". The check uses all configured machines and their
  inventories. When machines can serve it but all of them are offline, the lease
  waits as today, and `acquire` says so.
- **When the lease is ready,** `acquire` prints the facts of its machine: the
  OS, its version and architecture, the hardware, all tools of the machine with
  their options and environment names (requested or not), and the screen tools
  that started for this lease. `hostel info` shows them again:

  ```
  lease l-4f2a is ready on ns/linux
    machine  linux 24.04 (amd64), 8 cores, 16 GB
    tools    android-sdk 36 (ANDROID_HOME; adb on PATH) · jdk 21 · docker 27.3
    started  —   (no screen tools; add android-emulator to get one)
  ```
- `hostel agent-guide` explains the general rule ("ask for the tools you need;
  ask for a screen only when you need to see the app") and adds the usage text
  of each tool that this server has (section 4). An agent never reads about a
  tool that the server does not have.

## 3. Server configuration

Machines list tools; the `platforms` and `labels` keys go.

```toml
[providers.home]
kind = "static"

[providers.home.machines.macbook]
host = "macbook.local"
tools = ["xcode", "ios-simulator", "android-sdk", "android-emulator", "jdk:21", "macos-desktop"]
slots = 1

[providers.home.machines.win]
tools = ["windows-desktop", "dotnet:9"]

[providers.ns]
kind = "namespace"

[providers.ns.machines.linux]
spec = { product = "instance", image = "..." }
tools = ["android-sdk", "android-emulator", "jdk:21", "docker"]
max_rooms = 1
```

- **A tool entry** is `name` or `name:option`. For a mise tool, the option is the
  version to install (`jdk:21`, `node:22`); a machine can list several versions
  (`node:20`, `node:22`). For a screen tool, the option limits its options
  (`android-emulator:pixel-9`); without it, the machine gives all options of the
  tool.
- **Any mise tool is a tool** without a definition: `node:22`, `python:3.13`,
  `jdk:21`, `go:1.25`. The server installs it with mise at provisioning, as the
  machine-level `tools` of today (that key keeps its name; its form changes from
  a map to the list above).
- **Tools with a definition** (section 4) come with Hostel (`android-sdk`,
  `android-emulator`, `xcode`, `ios-simulator`, `macos-desktop`,
  `windows-desktop`) or from an admin (`hostel config tools new|pull|push|reset|
  remove`, the commands of `hostel config platforms` today).
- **Needs are added:** a machine that lists `android-emulator` gets
  `android-sdk` too; `hostel admin config check` says so.
- `hostel admin config check` fails when a machine lists a tool that has no
  definition and is not a mise tool, a tool without a script for the machine's
  OS (for example `windows-desktop` on Linux), or a needed tool that the OS
  cannot have. It also fails, with a clear message, on the keys of the old form
  (`platforms`, `labels`, `requires`): there is no compatibility with the old
  configuration (owner decision).

## 4. Tool definitions

A tool definition is a folder, as a platform folder is today:

```
tools/
  android-sdk/      tool.toml  install.linux.sh  install.macos.sh  check.linux.sh  check.macos.sh  GUIDE.md
  android-emulator/ tool.toml  install.*.sh  start.*.sh  stop.*.sh  screenshot.*.sh  GUIDE.md
  windows-desktop/  tool.toml  start.windows.ps1  screenshot.windows.ps1  GUIDE.md
```

`tool.toml`:

```toml
description = "Android emulator: one clean emulator for each lease"
needs       = ["android-sdk"]
screen      = true
options     = ["pixel-9", "pixel-tablet"]   # the options of a screen tool; the first is the default
max_options = 2                             # options in one lease (default 1)
whole_machine = false                       # true: the lease takes all slots
script_timeouts = { install = "30m", start = "5m" }
```

Steps (scripts, one file for each step and OS, as today):

| Step | When | Was |
|---|---|---|
| `install` | once for each room, and again when the tool's files change | `provision` |
| `check` | after install, after cleanup, at repair; prints the version (the option) on its last line, and fails when the tool does not work | `health` |
| `start` | for each lease that asked for the tool, before `ready`, in the order of needs; writes facts to `tool.env` (`ANDROID_SERIAL`, `SIMULATOR_UDID`) | `prepare` |
| `screenshot` | `hostel screenshot`; screen tools only | `screenshot` |
| `stop` | cleanup of a lease that started the tool, in reverse order | `reset` |

- **Environment of a tool:** `tool.toml` sets fixed values
  (`env = { ANDROID_HOME = "{cache}/android-sdk" }`,
  `path = ["{cache}/android-sdk/platform-tools"]`). This is general: each tool
  declares its environment and `PATH`. Every lease on a machine gets the
  environment of all software tools of that machine, and the `tool.env` facts of
  the screen tools that started for it.
- **`GUIDE.md`:** a short text for agents: how to build with the tool, how to
  see and drive a screen. `hostel tools <name>` and `hostel agent-guide` show it.
  The Android, iOS, macOS, and Windows sections of the agent guide of today move
  into the `GUIDE.md` files of their tools.
- **Script environment:** as today (`HOSTEL_SLOT`, `HOSTEL_CACHE_DIR`, …), with
  `HOSTEL_TOOL`, `HOSTEL_TOOL_OPTIONS` (the lease's options of this tool), and
  `HOSTEL_TOOLS` (all tools of the lease) instead of `HOSTEL_PLATFORM` and
  `HOSTEL_DEVICES`.
- **Caches:** an install script keeps its files in `$HOSTEL_CACHE_DIR/<tool>`
  (kept between leases on a room, as the SDK is today).

## 5. Scheduling

The order, the lanes, admin moves, blocking, slots, on-demand rooms, and limits
stay. Changes:

- **Candidate machine:** a machine is a candidate when it lists every requested
  tool and its inventory matches every option. A lease with no tools has every
  machine as a candidate. Before the first install of an
  on-demand room, the configured tool list is used (the inventory comes after
  the install).
- **Ready for the lease:** the room has installed and checked every requested
  tool and its needs. A free slot on a room that still needs an install comes
  after a ready one (option 2 of today).
- **Whole machine:** a lease is exclusive when one of its tools has
  `whole_machine = true` (today: `exclusive`, or the label `desktop`).
- **One waiting line:** the queue is no longer one list for each platform. The
  order is global; each waiting lease takes the first free candidate machine.
  Blocking stays per candidate machine, so a lease that waits for xcode does not
  block a lease that another machine can serve.
- **Estimated wait:** the leases before it in the order that share a candidate
  machine × the median lease time on its candidate machines ÷ their slots, plus
  the creation time when a new room is necessary. `acquire` prints "waiting
  behind N leases".
- **Options of a screen tool:** a lease that names none gets the first option
  that the machine has (the tool's `options` order).

## 6. API and guest

- `GET /v1/tools` replaces `GET /v1/platforms`: each tool with its description,
  options (from the inventories), screen flag, needs, environment names, guide,
  machines, free slots, and waiting leases.
- `POST /v1/leases` takes `tools: ["android-sdk", "android-emulator:pixel-9"]`
  instead of `platform` and `devices`. The lease, the status, and the events
  show the requested tools and the tools that the lease got (with their real
  options).
- The room status shows the inventory of each room (tools, options, check
  state).
- Guest: `hostel tools [name] [--json]`; `hostel acquire <tool[:option]>...`;
  `hostel screenshot [--tool <tool[:option]>]` (the only screen of the lease when
  there is one); `hostel info` shows the machine facts. `--device` and
  `--require` go.
- `hostel.toml` (owner decision; the old form fails with a clear message):

  ```toml
  tools = ["android-sdk", "jdk:21"]      # what "hostel acquire" asks for when no tools are given
  include = [".env"]                     # extra files that git ignores
  setup = ["./gradlew --version"]        # after mise install, in every lease
  env = { APP_ENV = "test" }             # in every lease

  [tunnels.api]
  local_port_env = "PORT"

  [tool.android-emulator]                # only when the lease has this tool
  env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }

  [tool.ios-simulator]
  setup = ["tuist install && tuist generate --no-open"]
  ```

  The settings for every lease are on the top level. `[tool.<name>]` holds only
  what changes when the lease has that tool, also as a need of a requested tool
  (its `setup` runs after the top-level `setup`, its `env` replaces keys of the
  top-level `env`). Tools on the command line replace `tools`.
- **The minimum guest version** goes up to the release of M10e: an older guest
  gets HTTP 426 and "run hostel update" (M10d), not a strange error.

## 7. The app

- The Lobby lists the tools (screens first), each with its free machines and
  its waiting leases. A tool page shows its options, machines, guide summary,
  and the waiting leases that asked for it, with the same actions as today.
- A machine page shows the inventory (tools, options, check state).
- The street on the main menu does not change.

## 8. No migration

There is no compatibility with the old configuration, the old `hostel.toml`,
or the platform revisions (owner decision: nobody depends on them). A server of
the M10e release whose stored configuration has the old keys starts with an
empty configuration (no providers, no machines) and logs a warning;
`hostel status` and the app show it, and the admin writes the new
configuration from a client (`hostel config edit`). So the update never makes
the server unreachable. Machine tools are names without a colon (the colon
starts an option); mise backend names such as `npm:x` stay in the repository's
mise files. `install.sh --server` no longer copies the tools (phase 5): the
server uses the tools in its binary.
The stored platform revisions are dropped; the default tools come with the
binary. Leases that are open at the update end with the reason "the server was
updated to tools; acquire again". Update a server with no open lease: the cleanup
of a lease from before M10e has no tool data, so it cannot stop an old emulator
or simulator, and its slot can stay broken until an admin repairs the room
(owner decision: no code for the old leases). The minimum guest version goes up to the M10e
release (M10d: HTTP 426 and "run hostel update").

## 9. Build order (phases; each ends with review, tests, a commit)

1. **Decisions and docs.** This file, `requirements.md` (vocabulary, decision),
   `plan.md`, and `design.md` (sections 3, 5, 8, 9, 10, 11).
2. **Tool definitions and configuration.** `tools/` with the default tools (the
   platform scripts split by tool), the configuration form, needs, checks, and
   `hostel config tools`.
3. **Rooms.** Install, check, and the inventory for each room; start and stop
   for each lease in the order of needs; the tool environment; screenshots by
   tool.
4. **Scheduling and API.** Candidates by tools and options, one waiting line,
   the estimate, `GET /v1/tools`, `POST /v1/leases` with tools, the minimum
   guest version.
5. **Guest.** `hostel tools`, `acquire` with tools, `hostel.toml` (`tools`,
   `[tool.<name>]`), `info`, `screenshot --tool`, the agent guide from the `GUIDE.md`
   files, the skills, the README.
6. **App.** The Lobby by tools, the tool page, the inventory on the machine
   page, the demo data.
7. Review, gates, done-when.

## 10. Tests

E2E, with the fake tools of the test harness (the `fakedevice` platform of
today becomes the tools `fake-sdk` and `fake-screen`):

- `TestTools`: `hostel tools` lists the inventory and the options; a request
  that no machine can serve fails at once with the reason; `--json`.
- `TestToolLease`: `acquire fake-sdk` does not start `fake-screen`;
  `acquire fake-screen:option-b` starts it (and `fake-sdk`, its need) in order,
  and cleanup stops it in reverse order; the environment of the tools.
- `TestToolOptions`: a version filter and a prefix match; `node` (any) gets a
  free machine while `node:22` waits for the one machine with 22.
- `TestToolWaitingLine`: one global order; a lease for a busy tool does not
  block a lease for another machine; "waiting behind N".
- `TestToolConfig`: `hostel admin config check` explains the old keys; a tool
  without a script for a machine's OS, and a missing need.
- `TestLeaseFacts`: `acquire` and `hostel info` show the OS, the hardware, and
  all tools of the machine, also the ones that the lease did not ask for.
- The existing tests change from platforms to tools (acquire, desktop, app,
  smoke tests on the real Mac, Windows, and Namespace machines).

**Done when:** a new agent in the same app repository repeats the Android task
of the report (Gradle builds, emulator QA with screenshots) with only the
README, the skills, and `hostel agent-guide`: it builds with
`acquire android-sdk jdk` without an emulator, does QA with
`android-emulator:pixel-9`, and its report has none of the problems 1, 2, and 7
of the 2026-11-05 report. The iOS and Windows desktop smoke tests pass with
tools.

## 11. Not in this milestone

General improvements from the same report that are not about the model; a
later milestone:

- `hostel attach --wait` and `hostel run --wait`, `attach --last`, shorter job
  IDs.
- Faster `hostel cp` (compression), and a copy between two leases.
- Tool caches that survive on-demand rooms (needs provider support, for example
  Namespace cache volumes).
- `acquire --no-sync`, and tunnels on the command line.
- `hostel record` for screen tools.

## 12. Risks

- **A large change of the core.** Mitigation: the phases above, and a test for
  each part of the model.
- **Several tools in one lease.** Their `start` steps run in the order of needs
  and `stop` in reverse order; a failed `start` stops the ones that already
  started and the one that failed (it can leave processes; `stop` scripts are
  idempotent), then the lease fails as a `prepare` failure does today.
- **Two heavy screens on one machine** (two emulators on an 8 GB Mac). The slots
  of the machine limit it, as today; the admin sets the slots.
- **The production server** needs a new configuration at the update (section
  8).

## 13. State at the end of M10e (2026-11-06)

Done. v0.3.0 is released and runs on the production server.

- **Gate:** 10 of 10 local E2E runs on `6790f67` (owner decision: local, because
  a billing lock stops GitHub Actions). The release files are built and signed
  locally for the same reason.
- **Real machines:** the Windows smoke tests pass. On the production server, real
  leases of `ios-simulator`, `android-emulator`, `macos-desktop` (`ns/mac`), and
  `windows-desktop` work, with screenshots.
- **Done-when:** a new agent built, tested, and did QA of the Android app of a
  real repository in 20 minutes with only the docs. It chose `android-sdk
  jdk:21` for the build (no emulator) and `android-emulator:pixel-9` for QA. It
  had none of the problems 1, 2, and 7 of the 2026-11-05 report.
- **Not tested:** the Namespace smoke tests (the Namespace login of the test Mac may not
  create machines); `ns/linux` with tools.

Follow-ups from the done-when report, done in M10f (`plan.md`):

- A tunnel with an unset local port shows `confirmed` and does not work.
- `hostel status <machine>` fails for an on-demand machine without a room, and
  `hostel tools <tool>` does not show the CPU and memory of a machine.
- `android-sdk` reports 36, but builds install more SDK platforms; the facts of
  a lease do not show them, or the Java of the repository's mise file.
- `~/.gradle` stays between leases (it is a cache, section 9); the docs must say
  so, because other guests can read it.
- The guide should say that a long build needs `run --detach` and
  `attach --wait`.
