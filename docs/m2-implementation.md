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

(Filled in during M2.)
