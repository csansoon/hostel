# M2 implementation plan

Status: planned, 2026-10-22. Milestone M2 in `plan.md`: the spare MacBook as a
static iOS room. This file gives the build order, the checks on the real machine,
and the risks. `design.md` stays the contract. When this plan adds a detail to the
contract, the same commit updates `design.md`.

## 1. Setup

- The Hostel server runs on the owner's Mac (`hostel server init` done on
  2026-10-22; `~/.config/hostel/server.toml`, `~/.local/share/hostel`). It listens
  on the default `127.0.0.1:7410` (API) and `127.0.0.1:7422` (gateway). Guests run
  on the same Mac, in ExampleApp worktrees.
- The server connects to the MacBook over the local network (`connect = false`).
  `hostel room connect` is M5.
- The owner prepares the MacBook with `docs/static-room-setup.md`.
- Machine configuration:

  ```toml
  [providers.home]
  kind = "static"

  [providers.home.machines.macbook]
  address = "hostel@<address>"
  os = "macos"
  platforms = ["ios"]
  labels = ["xcode-26.5", "desktop"]
  ```

- The platform directories are in this repository (`platforms/ios`,
  `platforms/macos`). The server reads them from `platforms_dir`; on the owner's
  Mac, `platforms_dir` points to the repository checkout.

## 2. Work that does not need the MacBook

1. `platforms/ios/`: `platform.toml` (devices `iphone-17-pro`, `ipad-pro-13`;
   `max_devices = 2`; `requires = ["xcode-26.5"]`) and the scripts:
   - `provision`: check `xcodebuild -version` and the iOS runtime; create one
     golden simulator for each device (name `hostel-golden-<device>`), boot it once
     so that the first-boot work is done, and shut it down.
   - `prepare`: for each lease device, make a clean simulator (method from the
     timing experiment, section 3), boot it, wait with `simctl bootstatus`, and
     write `SIMULATOR_UDID` and `SIMULATOR_UDIDS` to `device.env`.
   - `screenshot`: `xcrun simctl io <udid> screenshot <path>` for the selected
     device.
   - `reset`: shut down and delete (or erase) the lease's simulators. It finds them
     by a name that contains the slot, so a repeated reset works without
     `device.env`.
   - `health`: Xcode works, the runtime exists, the golden simulators exist, free
     disk is at least 20 GB, and no simulator of a slot without a lease is left.
   - All names have the prefix `hostel-`; the scripts act only on such simulators.
2. `platforms/macos/screenshot.macos.sh`: `screencapture -x <path>`. It fails with
   a clear message when the image is empty or the permission is missing.
3. The E2E platform `fakedevice` stays the test for the script contract. The
   `ios` scripts cannot run in Docker; `make lint` runs `shellcheck` on
   `platforms/` (new) and `hostel admin config check` on them.
4. A draft of the ExampleApp `hostel.toml` and of the `agent-guide` text (section 4).

## 3. Checks on the MacBook

Each check gives a short result in `design.md` (or a fix), with the date.

1. **Timing.** Compare `simctl clone` of a golden simulator + boot with
   `simctl erase` of a kept simulator + boot, 5 times each, for `iphone-17-pro` and
   `ipad-pro-13`. Select the faster method that gives a clean device (no app, no
   data, default settings). Record the numbers.
2. **`ps -E`.** Check that the helper finds the room user's processes by
   `HOSTEL_LEASE` in their environment (cleanup step 1, design section 17), also
   for processes in a new session (`setsid`, `nohup`, a detached `screen`).
3. **Build without `.git`.** Build ExampleApp in `work/` with the `setup` commands
   (`tuist install`, `tuist generate`) and `xcodebuild` for the simulator. ExampleApp's
   `Tuist.swift` finds the root without `.git`; check that no build step needs git
   (for example a build number from `git rev-list`).
4. **Screen Recording over SSH.** `screencapture` from an SSH session, with the
   permission for `sshd-keygen-wrapper`. If it does not work, find the correct
   entry or a different method (for example a launchd agent in the `hostel`
   session), and update the setup guide.
5. **Simulator window.** The Simulator app shows the booted device in the desktop
   session (needed for the desktop screenshot and for a person who watches). The
   device screenshot does not need it.

## 4. ExampleApp

`hostel.toml` proposal for the ExampleApp repository root (not committed to ExampleApp; the
owner adds it):

```toml
# The guest reads PORT from the local .env for each connection, so .env is not
# sent to the room (it has secrets).
[tunnels.api]
local_port_env = "PORT"

[platforms.ios]
devices = ["iphone-17-pro"]
setup = ["cd apps/ios && tuist install && tuist generate --no-open"]
# Debug builds read the API URL from TUIST_DEBUG_API_BASE_URL at generate time.
env = { TUIST_DEBUG_API_BASE_URL = "http://localhost:{tunnels.api.port}" }
```

- ExampleApp's `.mise.toml` pins `node`, `tuist`, `pnpm`, and `java`. The setup job runs
  `mise install`, so the room also gets Java, which iOS does not need. The cost is
  one download; the mise cache keeps it.
- Build command for agents (ExampleApp `apps/ios/QA.md`, section 3):

  ```sh
  hostel run -- "cd apps/ios && xcodebuild -workspace exampleapp.xcworkspace -scheme exampleapp \
    -configuration Debug -destination id=\$SIMULATOR_UDID build"
  ```

  Then `xcrun simctl install` and `xcrun simctl launch` (bundle ID
  `com.example.app`). DerivedData stays in its default place; the `work` path of a slot
  is the same for each lease, so the warm build reuses it.
- `hostel agent-guide` has a short generic part for devices and tunnels. The
  ExampleApp-specific commands go to ExampleApp's `AGENTS.md` (proposal for the owner).

## 5. Acceptance (the "done when" of M2)

From an ExampleApp worktree on the owner's Mac, with the local API running:

1. `hostel acquire ios --device iphone-17-pro`; `hostel run` of the iOS build; app
   install and launch; sign-in against the local API through the tunnel;
   `hostel screenshot`. The same with `--device ipad-pro-13`.
2. A second lease starts on the warm room, and its simulator is clean (the app from
   the first lease is not installed).
3. `hostel acquire macos --require desktop` and `hostel screenshot` give a desktop
   screenshot.
4. The evidence: the command output and the screenshots.

## 6. Code changes and tests

Code changes come from the checks. Each changed behavior gets an E2E scenario
when Docker can show it, and the 10-run gate runs at the end of M2. The checks on
the MacBook are manual; their results go into this file (section 8).

## 7. Risks

| Risk | Plan |
|---|---|
| `screencapture` over SSH fails even with the permission. | A launchd agent in the `hostel` session takes the screenshot; the script asks it. |
| Simulator boot over SSH needs the desktop session. | The setup has automatic login; if a boot fails without it, `health` reports it. |
| An Xcode prompt (license, first launch, developer tools) stops a script. | The setup guide covers them; scripts have timeouts; `health` checks `xcodebuild -version`. |
| Building ExampleApp takes long. The default `max_lease` (`2h`) and the `run` wait need checks. | DerivedData in the cache; measure the first and the warm build. |
| ExampleApp must not build with `CODE_SIGNING_ALLOWED=NO` (its QA notes: the Keychain fails). A simulator build can need a signing identity for its entitlements. | Check the first build. If it needs an Apple account in Xcode for the `hostel` user, ask the owner. |
| The MacBook sleeps or loses the network. | `pmset` in the setup; the room becomes unreachable, and the server reports it. |

## 8. Results

- 2026-10-22, `ps -E` (check 2), on the owner's Mac (macOS 27.2): `ps -E` shows
  the environment of a Python process, but not of `/bin/sleep` or `/bin/zsh`
  (Apple's own binaries). Cleanup did not find a guest's leftover `sh` or `sleep`.
  Fix: cleanup also finds the processes in the lease's sessions (design, section
  17). `TestCleanup` has two leftover processes without `HOSTEL_LEASE`; it fails
  on the old code (2 processes stay) and passes now. Check again on the MacBook
  (macOS 26).
- 2026-10-22, the room Mac: MacBook, macOS 26.6.2, arm64, 8 cores, 8 GB memory,
  Xcode 26.5 (17F42), iOS 26.5 runtime (23F77). The server on the owner's Mac
  reaches it by its `.local` name over IPv6; IPv4 between the two Wi-Fi address
  ranges does not work on this network.
- 2026-10-22, setup guide: the `echo` with a line break inside quotes wrote one
  bad line to `100-hostel.conf` when it was pasted, and sshd closed each
  connection. The guide now uses `printf` and runs `sshd -t`.
- 2026-10-22, `ios` provision at server start: 72 s (two golden simulators,
  each booted once).
- 2026-10-22, timing (check 1), 5 times each, seconds:

  | Device | clone | boot after clone | total | erase | boot after erase | total |
  |---|---|---|---|---|---|---|
  | iphone-17-pro | 2.2–2.7 | 9.6–10.3 | 12.2–12.6 | 0.2–0.3 | 28.5–30.6 | 28.8–30.8 |
  | ipad-pro-13 | 2.6–2.8 | 9.8–10.1 | 12.5–12.9 | 0.2–0.3 | 28.9–35.3 | 29.2–35.5 |

  A clone of the golden simulator (booted once at provision) boots about 2.4
  times faster than an erased device. `prepare` keeps the clone method.
- 2026-10-22, `ps -E` (check 2) on macOS 26.6.2: `ps -E` shows `HOSTEL_LEASE`
  for none of `sleep`, `sh`, `/usr/bin/python3`, and `/usr/bin/perl`. With a
  real lease, cleanup found and stopped all 4 leftover processes (two with
  `HOSTEL_LEASE` removed, one from `hostel ssh`, one `nc` on a port) by their
  sessions. The slot simulators were deleted and the port was free.
- 2026-10-22, Screen Recording (check 4): without the permission,
  `screencapture` over SSH fails ("could not create image from display"). With
  the permission for `/usr/libexec/sshd-keygen-wrapper` in the `hostel` session,
  it gives the full desktop with windows. `hostel screenshot` of a `macos`
  lease with `--require desktop` works.
- 2026-10-22, simulator window (check 5): `simctl boot` does not open the
  Simulator app. `open -a Simulator` from a lease job shows the booted lease
  device on the desktop. The device screenshot (`simctl io`) works without it.
- 2026-10-22, ExampleApp on the room (check 3): `tuist install` and `tuist generate`
  work in `work/` without `.git` (71 s, the first time). The setup first failed
  with `tuist: command not found`: the job `PATH` comes from `mise env` at the
  job start, before the setup's `mise install`. Fix: the setup adds
  `mise bin-paths` to `PATH` after `mise install` (design, section 10);
  `TestMise` has a setup that uses a tool from the same setup. The first
  `xcodebuild` (Debug, simulator) took 2 min 45 s; the build in the next lease
  took 47 s (DerivedData reused, the same `work` path). The app is signed ad hoc
  with the simulator entitlements (`keychain-access-groups` included), without
  an Apple account. The first `mise install` of ExampleApp's tools took 225 s on the
  room's network (Java, 200 MB, at about 50 kB/s); they are cached for later
  leases.
- 2026-10-22, acceptance item 2: a second lease was ready in 47 s, and ExampleApp was
  not installed on its simulator.
- 2026-10-22, tunnel: from the room, `localhost:7500` reaches the owner's local
  ExampleApp API; `POST /auth/magic-link` returns 200. ExampleApp's debug build signs in by
  itself as `user@dev.local` against a localhost API, and that account was then
  in the worktree's own database (only reachable through the tunnel).
- 2026-10-23, UI input: T3 Code drives simulators with `agent-device`
  (Callstack, npm, 0.21.25). Owner decision: each room gets it by default
  (design, section 9, "mise on rooms"). Over SSH on the room it works: the first
  `open` built its XCTest runner and took 39 s; later leases took 17–19 s
  (runner cache). `click` and `fill` work (ExampleApp's notification sheet, the
  sidebar, text in the chat field).
- 2026-10-23, cleanup with agent-device: its iOS runner (`xcodebuild
  test-without-building`) runs in a new session without a visible environment,
  and it survived cleanup until its simulator was gone (about 2.5 min). Fix:
  cleanup also takes the descendants of lease processes and records their
  sessions (design, section 17). `TestCleanup` has a daemon that starts a child
  in a new session; it fails on the old code (1 process stays) and passes now.
  On the room, two leases with `agent-device` left 0 processes.
- 2026-10-23, acceptance item 1 on `ipad-pro-13`: the build took 54–65 s (the
  DerivedData of the iPhone builds was reused), and the app signed in as
  `user@dev.local` through the tunnel (agent-device read the account in the
  sidebar). With the iPhone run above, item 1 is done.
- 2026-10-23, memory: the 8 GB MacBook ran the builds, one simulator, and
  agent-device without failures. I did not measure two devices in one lease.
