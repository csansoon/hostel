---
name: hostel-platforms
description: Understand and change Hostel platforms - the defaults (ios, android, macos, windows, linux), what each needs on the machine, devices and labels, and your own platforms with their scripts. Use it when the user says "add iOS/Android to a machine", "what does the Mac need for iOS", "Android emulator does not start", "add a device", "make my own platform" (for example tvOS), or "change the provision/prepare script".
---

# Platforms

A platform is what a guest asks for (`hostel acquire <platform>`). There are two
kinds:

| Kind | Names | Which machines serve it |
|---|---|---|
| OS platform | `linux`, `macos`, `windows` | Every machine with that OS, automatically. Never list it in `platforms`. |
| Device platform | `ios`, `android`, or your own | Machines that list it in `platforms`. |

Tools such as Node, Java, or pnpm are not platforms: the repository's mise
files declare them (`hostel-use`), or the machine's `tools` key (`hostel-rooms`).

```sh
hostel platforms              # for everyone: free slots, queue, wait, devices
hostel config platforms       # for admins: NAME, KIND, MACHINES, SOURCE
```

## The default platforms

The binary has `ios`, `android`, `macos`, and `windows` built in. `linux` is an OS
platform without scripts.

| Platform | Devices | Machine OS | What the machine needs |
|---|---|---|---|
| `ios` | `iphone-17-pro`, `ipad-pro-13` (max 2) | macOS | Xcode and an iOS Simulator runtime, installed by a person. The platform requires the label `xcode-26.5`. Optional machine `env`: `HOSTEL_IOS_RUNTIME` (for example `26.5`), `DEVELOPER_DIR`. |
| `android` | `pixel-9`, `pixel-tablet` (max 2) | Linux or macOS | Java 17 or later: set `tools = { java = "21" }` on the machine. Linux: `/dev/kvm` usable by the room user (give the machine a label such as `kvm`), and the emulator's packages or `sudo` without a password. Provision downloads about 5 GB (timeout 30 min). Optional `env`: `HOSTEL_ANDROID_API` (36), `HOSTEL_ANDROID_TAG` (`google_apis`), `HOSTEL_ANDROID_PORT` (5554). |
| `macos` | none | macOS | Headless builds: nothing. Desktop apps: the label `desktop`, an automatic login of the room user, an unlocked screen, Screen Recording for `/usr/libexec/sshd-keygen-wrapper`, and Automation Mode without a password (`sudo automationmodetool enable-automationmode-without-authentication`). |
| `windows` | none | Windows | OpenSSH Server. Desktop apps: the label `desktop` and the "Hostel desktop" task in a signed-in, unlocked desktop session. |
| `linux` | none | Linux | An SSH server. |

What the platforms give to commands:

- `ios`: new, clean simulators for each lease; `SIMULATOR_UDID` (first device)
  and `SIMULATOR_UDIDS`.
- `android`: read-only emulators from a clean snapshot; `ANDROID_SERIAL`,
  `ANDROID_SERIALS`, `ANDROID_HOME`.
- A lease that requires `desktop` is exclusive: it takes the whole machine.

Each room also gets `agent-device` (tap, type, read the screen) and, on
Windows, `winapp`. `mise.agent_device = false` (`hostel config settings set`)
turns this off.

The full Mac and Windows preparation (users, Remote Login, auto login, the
desktop task) is in `docs/static-room-setup.md`.

## Devices and labels

- `--device <d>` selects devices of the platform (repeat it, up to
  `max_devices`). Without it, the repository's `devices` or the platform's
  first device apply.
- Labels are free words on machines (`labels = [...]`). A machine is a
  candidate only if it has every required label: the platform's `requires`,
  the repository's `requires` in `hostel.toml`, and each `--require`.
- Name labels after what the machine has: `xcode-26.5`, `desktop`, `kvm`.
- An unknown platform or device, or a label that no machine has, fails at once
  with exit code 69.

## Your own platform

```sh
hostel config platforms new tvos ./tvos --os macos     # a template folder (--os linux|macos|windows)
# edit ./tvos/platform.toml and the scripts
hostel config platforms push ./tvos                    # check and save; the name is the folder name
hostel config rooms set mac-mini platforms=ios,tvos     # let a machine serve it
```

Change a default platform:

```sh
hostel config platforms pull ios ./ios      # the current files into a new folder
hostel config platforms push ./ios          # save your version (source: changed)
hostel config platforms reset ios           # back to the version of this release
hostel config platforms remove tvos         # refused while a machine lists it
```

`push --name <n>` saves the folder under another name. A push applies at once.
A free room whose platform changed provisions again. A push sends only
`platform.toml`, `README.md`, and scripts.

### `platform.toml`

```toml
description = "tvOS Simulator"
devices = ["apple-tv-4k"]          # the first is the default (device platforms only)
max_devices = 1                    # default 1
requires = ["xcode-26.5"]          # labels that each machine must have
exclusive = false                  # true: the lease takes the whole room
script_timeouts = { provision = "20m" }   # defaults: provision 20m, others 3m
```

An OS platform folder (`linux`, `macos`, `windows`) can have `description`,
`requires`, `exclusive`, and `script_timeouts`, but not `devices`.

### Scripts

File names: `<step>.<os>.sh` for `macos` and `linux`, `<step>.<os>.ps1` for
`windows`. The room's OS selects the file. A misspelled script name is an
error.

| Step | When | Runs in |
|---|---|---|
| `provision` | once for each room, and again when the scripts change: install and check what the platform needs | `hostel/` |
| `prepare` | each lease, before it is ready: start the devices, write their facts to `device.env` | `slots/<slot>/` |
| `screenshot` | `hostel screenshot`: `$1` is the output PNG path, `$2` the device | `slots/<slot>/` |
| `reset` | at cleanup: stop and delete what `prepare` started | `slots/<slot>/` |
| `health` | after cleanup, at repair, before a warm room takes a lease: exit non-zero if the room cannot serve the platform | `hostel/` |

Rules:

- Every step is optional, except `prepare` for a device platform on each OS of
  the machines that list it.
- Each step must be safe to run again.
- A non-zero exit code is a failure. Provision or prepare failures make the
  room Broken and send the lease back to the queue. The last 4 KB of output go
  to the event log (`hostel status <room>`).
- The helper runs `/bin/sh <script>` (no execute bit necessary) and stops its
  process group at the timeout.
- Machine-level `tools` are installed before `provision`, so scripts can use
  them.

Environment of the scripts: `HOSTEL_PLATFORM`, `HOSTEL_PLATFORM_DEVICES` (all
devices of the platform), `HOSTEL_SLOT`, `HOSTEL_SLOT_DIR`, `HOSTEL_WORK_DIR`,
`HOSTEL_CACHE_DIR` (kept between leases), `HOSTEL_LEASE_ID`, and for `prepare`,
`screenshot`, and `reset`: `HOSTEL_DEVICES` (the lease's devices,
comma-separated) and `HOSTEL_EXCLUSIVE` (`1` for a whole-room lease). Also the
`MISE_*` variables, the provider, machine, and slot `env`, and `device.env` (for
`screenshot` and `reset`). Use machine `env` to vary a script between machines.

`device.env` lines (`NAME=value`) go into the environment of every lease
command. Document the names that your platform writes.

## Check

```sh
hostel config platforms
hostel status mac-mini       # the events show provision output and errors
```
