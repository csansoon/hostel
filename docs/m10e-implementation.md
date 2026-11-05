# M10e implementation plan: tools

Status: design for the owner's review (2026-11-05). Milestone M10e in `plan.md`.
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
  `jdk`, `xcode`, `dotnet`, `node`, `docker`), a screen (`android-emulator`,
  `ios-simulator`, `macos-desktop`, `windows-desktop`), or the OS itself
  (`linux`, `macos`, `windows`).
- **Option:** a version or a model of a tool, after a colon: `node:22`,
  `xcode:26`, `android-emulator:pixel-9`. An option is a filter: `node` matches
  any machine with any `node`, `node:22` only machines whose `node` matches `22`.
  A version option matches the whole version or its first parts (`xcode:26`
  matches `26.1` and `26.2`; `node:22` matches `22.11.0`, not `2.2`). A model
  option matches the model name exactly.
- **Machine:** it lists the tools that it gives. The server installs them,
  checks them, and keeps the real versions and models of each room (the
  **inventory**).
- **Lease:** a set of tools (with optional options) on one slot of one machine
  that has all of them. The server starts only what the lease asked for.
- **Screen tool:** a tool that the agent looks at and acts on. Hostel starts it
  for each lease, cleans it up at the end, takes screenshots of it, and has its
  driver ready (`agent-device`; `desktop` and `winapp` on Windows). A screen tool
  can take the whole machine (the desktops: there is one screen).
- **OS tool:** each machine has the tool of its OS automatically
  (`linux`, `macos`, or `windows`; the option is the OS version). So
  `hostel acquire linux` is a plain Linux machine, and `hostel acquire
  android-sdk macos` is the Android SDK on a Mac.

What stays: machines, rooms (one runtime instance of a machine), slots, leases,
providers, the waiting line and its lanes, sync, jobs, tunnels, cleanup. Tools
from the repository (`mise.toml`: node, pnpm, Python) stay as they are: set up
for each lease on any machine, and cached.

### The agent's view

```
$ hostel tools
TOOL               OPTIONS                      SCREEN  MACHINES                FREE  WAITING
linux              24.04                                ns/linux                1     0
macos              26.1                                 home/macbook            1     0
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
hostel acquire linux --task "API tests"
```

- `hostel.toml` declares the usual set of the repository (`tools = [...]`), so
  `hostel acquire --task "..."` is enough. Tools on the command line replace it.
- A request that no machine can serve fails at once, with the reason:
  "no machine has xcode and android-sdk together; machines with xcode:
  home/macbook".
- `hostel info` (in a lease) lists the lease's tools, their real options, and
  the names of the environment variables that they set.
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
  (`node:20`, `node:22`). For a screen tool, the option limits the models
  (`android-emulator:pixel-9`); without it, the machine gives all models of the
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
  OS, or a needed tool that the OS cannot have.

## 4. Tool definitions

A tool definition is a folder, as a platform folder is today:

```
tools/
  android-sdk/      tool.toml  install.linux.sh  install.macos.sh  check.linux.sh  check.macos.sh  GUIDE.md
  android-emulator/ tool.toml  install.*.sh  start.*.sh  stop.*.sh  screenshot.*.sh  GUIDE.md
  windows-desktop/  tool.toml  start.windows.ps1  stop.windows.ps1  screenshot.windows.ps1  GUIDE.md
```

`tool.toml`:

```toml
description = "Android emulator: one clean emulator for each lease"
needs       = ["android-sdk"]
screen      = true
models      = ["pixel-9", "pixel-tablet"]   # the options of a screen tool
max_models  = 2                             # models in one lease (default 1)
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

- **Environment of a tool:** `tool.toml` can set fixed values for every lease
  of a machine that has the tool, also when the lease did not ask for it
  (`env = { ANDROID_HOME = "{cache}/android-sdk" }`, `path = ["{cache}/android-sdk/platform-tools"]`).
  This is general: each tool declares its environment and `PATH`, and the lease
  gets the environment of the tools that it asked for and their needs.
- **`GUIDE.md`:** a short text for agents: how to build with the tool, how to
  see and drive a screen. `hostel tools <name>` and `hostel agent-guide` show it.
  The Android, iOS, macOS, and Windows sections of the agent guide of today move
  into the `GUIDE.md` files of their tools.
- **Script environment:** as today (`HOSTEL_SLOT`, `HOSTEL_CACHE_DIR`, …), with
  `HOSTEL_TOOL`, `HOSTEL_TOOL_OPTIONS` (the lease's models of this tool), and
  `HOSTEL_TOOLS` (all tools of the lease) instead of `HOSTEL_PLATFORM` and
  `HOSTEL_DEVICES`.
- **Caches:** an install script keeps its files in `$HOSTEL_CACHE_DIR/<tool>`
  (kept between leases on a room, as the SDK is today).

## 5. Scheduling

The order, the lanes, admin moves, blocking, slots, on-demand rooms, and limits
stay. Changes:

- **Candidate machine:** a machine is a candidate when it lists every requested
  tool and its inventory matches every option. Before the first install of an
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
  block a lease for linux.
- **Estimated wait:** the leases before it in the order that share a candidate
  machine × the median lease time on its candidate machines ÷ their slots, plus
  the creation time when a new room is necessary. `acquire` prints "waiting
  behind N leases".
- **Models:** a lease that names no model of a screen tool gets the first model
  that the machine has (the tool's `models` order).

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
  `hostel screenshot [--tool <tool[:model]>]` (the only screen of the lease when
  there is one); `hostel info` lists the tools. `--device` and `--require` go.
- `hostel.toml`:

  ```toml
  tools = ["android-sdk", "jdk"]              # the default set of this repository

  [with.android-emulator]                     # applies when the lease has this tool
  env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }

  [with.ios-simulator]
  setup = ["tuist install && tuist generate --no-open"]
  ```

  `[with.<tool>]` replaces `[platforms.<name>]`. The words "with" and "tools"
  are different on purpose: `tools` is a list, `with` gives settings.
- **The minimum guest version** goes up to the release of M10e: an older guest
  gets HTTP 426 and "run hostel update" (M10d), not a strange error.

## 7. The app

- The Lobby lists the tools (screens first), each with its free machines and
  its waiting leases. A tool page shows its options, machines, guide summary,
  and the waiting leases that asked for it, with the same actions as today.
- A machine page shows the inventory (tools, options, check state).
- The street on the main menu does not change.

## 8. Migration

The server migrates by itself at the first start of the M10e release:

- **Configuration** (the stored document of M9; a new revision with the author
  "migration"): `platforms = ["android"]` → `tools = ["android-sdk",
  "android-emulator", ...]`; `ios` → `xcode`, `ios-simulator`; the label
  `desktop` on a macOS machine → `macos-desktop`, on Windows →
  `windows-desktop`; the machine-level mise map → the list form. Other labels
  have no meaning in the new model: the migration lists them in the event log,
  removes them, and `hostel status` shows a warning until an admin looks at it.
- **Platform revisions** (M9): the defaults become the default tools. An admin's
  own platform becomes an own tool with the same scripts (`provision` →
  `install`, `prepare` → `start`, `reset` → `stop`, `health` → `check`); the
  migration writes a warning when a script needs a manual change (for example a
  check that prints no version).
- **Leases:** waiting and ready leases at the update keep working: the server
  maps their platform to its tools.
- **Repositories:** `hostel admin config check --repo` explains how to change
  `[platforms.x]` to `tools` and `[with.x]`. A `hostel.toml` with the old form
  still works for one release, with a warning on `acquire`.

## 9. Build order (phases; each ends with review, tests, a commit)

1. **Decisions and docs.** This file, `requirements.md` (vocabulary, decision),
   `plan.md`, and `design.md` (sections 3, 5, 8, 9, 10, 11).
2. **Tool definitions and configuration.** `tools/` with the default tools (the
   platform scripts split by tool), the configuration form, needs, checks,
   `hostel config tools`, and the migration of the configuration and the
   platform revisions.
3. **Rooms.** Install, check, and the inventory for each room; start and stop
   for each lease in the order of needs; the tool environment; screenshots by
   tool.
4. **Scheduling and API.** Candidates by tools and options, one waiting line,
   the estimate, `GET /v1/tools`, `POST /v1/leases` with tools, the minimum
   guest version.
5. **Guest.** `hostel tools`, `acquire` with tools, `hostel.toml` (`tools`,
   `with`), `info`, `screenshot --tool`, the agent guide from the `GUIDE.md`
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
  `acquire fake-screen:model-b` starts it (and `fake-sdk`, its need) in order,
  and cleanup stops it in reverse order; the environment of the tools.
- `TestToolOptions`: a version filter and a prefix match; `node` (any) gets a
  free machine while `node:22` waits for the one machine with 22.
- `TestToolWaitingLine`: one global order; a lease for a busy tool does not
  block a lease for another machine; "waiting behind N".
- `TestToolMigration`: a server with an M10d configuration and platform
  revisions starts on M10e; the configuration, the platforms, and a waiting
  lease are migrated.
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

- **A large change of the core.** Mitigation: the phases above, the migration
  test, and old leases that keep working at the update.
- **Several tools in one lease.** Their `start` steps run in the order of needs
  and `stop` in reverse order; a failed `start` stops the ones that already
  started, then the lease fails as a `prepare` failure does today.
- **Two heavy screens on one machine** (two emulators on an 8 GB Mac). The slots
  of the machine limit it, as today; the admin sets the slots.
- **Admins' own platforms** need a manual check after the migration (the
  warnings in section 8).
