# Context

This file gives a new contributor the background that is not in the code: why
Hostel exists, who uses it first, and what was tried before.

## Why Hostel exists

The owner develops **ExampleApp**, a monorepo with a TypeScript API and native iOS, Mac
Catalyst, and Android clients. A native Windows client is planned. Many coding
agents work on ExampleApp at the same time, each in its own git worktree on the owner's
Mac.

Today, these agents share one iOS Simulator, one Android emulator or USB device,
and one Postgres + SeaweedFS pair on the Mac. A local lock tool (`devlock`) makes
them take turns. This has three problems:

1. Agents wait for each other for the devices.
2. Cloud agents (Linux machines) cannot build or QA the iOS or Mac app at all.
3. There is no Windows machine.

Hostel moves builds and UI QA to remote machines. Each agent gets its own machine
(or slot) for a short time. The agent's code stays local, and the API under test
still runs locally on the agent's machine. A tunnel connects the remote app to it.

## First user: ExampleApp

Hostel must be generic, but ExampleApp is the first user: a private monorepo with a
TypeScript API and native iOS and Android clients. What it needs from Hostel:

- **The local API:** each worktree runs its own API on its own port; the app on
  the room reaches it through a tunnel (`hostel.toml`, `[tunnels.api]`).
- **iOS:** an Xcode build for a simulator (the `ios` platform, with a label for
  the Xcode version), install, launch, sign-in through the local API, and
  screenshots.
- **Android:** a Gradle build with JDK 21 and the Android SDK, and the emulator
  (the `android` platform).
- A `hostel.toml` for ExampleApp is a separate change in the ExampleApp repository. Propose it
  to the owner; do not change the ExampleApp repository from this project.

## History

### Prototype "berth" (2026-10-20, abandoned)

A first prototype in TypeScript stayed on the owner's Mac (never pushed). Its design:

- No central server. Each machine ran a broker (`harbor`) that the client called over
  SSH with one JSON request on stdin.
- The machine configured its own slots and devices.
- The client made a snapshot commit with a temporary git index and pushed it over SSH
  to the machine.

Why it was abandoned: an on-demand machine does not exist before a request, so it
cannot keep a queue. A central server can. The owner then asked for three pieces
(server, guests, rooms) and for a file push without git.

Lessons that still apply:

- A timing-based E2E test (idle expiry) was flaky: it failed in 1 or 2 of about 11
  full runs. Wait for conditions with a deadline, keep timing margins wide, and run
  the suite many times before you call it stable.
- The tunnel must not need the local port at lease start. The API often starts
  later. Read the port when a connection arrives.
- Do not copy code from the prototype. The topology and the language changed.

### Crabbox evaluation (2026-10-20)

Crabbox (crabbox.sh, `openclaw/crabbox`, Go, MIT) gives agents leases on remote
machines, with working-tree sync, run, SSH, screenshots, VNC, and a provider
contract over JSON. It was not selected because it has no queue or priority, no
slots, no reverse tunnel, and its Namespace providers are Linux only. Its external
provider contract inspired Hostel's `external` provider.

### Namespace spike (2026-10-20)

The spike proved that a Namespace macOS devbox can build and run ExampleApp iOS and take
screenshots, and that a Linux instance can run the Android emulator. See
`namespace.md` for times, limits, and traps.

## Decisions log

See `requirements.md`, section 3. Record new owner decisions there, with the date.
