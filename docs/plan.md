# Plan

Build Hostel in this order. Finish each milestone, show the evidence to the owner,
and wait for approval before the next one. From M1, each milestone ends with all E2E
tests passing in 10 separate runs of the whole suite in CI (the workflow "E2E
gate", started by the label `e2e-10` on the pull request), and in one local run on
macOS (`make test-e2e`). Before 2026-10-26 the gate was `make test-e2e-10` on the
owner's Mac.

## M0. Repository setup

- `go.mod` with module `github.com/csansoon/hostel`. Go version from
  `mise.toml`.
- `internal/version` with `var Version = "dev"`, set by
  `-ldflags "-X github.com/csansoon/hostel/internal/version.Version=<git describe>"`.
  `hostel --version` prints it. The server compares helpers by SHA-256, not by
  version (design, section 8).
- A `cmd/hostel` main that dispatches the modes and prints help. Commands that later
  milestones build print "not implemented yet" and exit 1; do not add other
  placeholders.
- A `Makefile` that works with GNU Make 3.81 (the macOS version). All Go builds use
  `CGO_ENABLED=0`. Targets:
  - `build`: the host binary in `dist/hostel`.
  - `dist`: `dist/hostel-<goos>-<goarch>` (`.exe` on Windows) for darwin, linux, and
    windows on amd64 and arm64, plus the pinned mise binaries (below).
  - `lint`: `test -z "$(gofmt -l .)"` (fails when a file needs formatting) and
    `go vet -tags e2e ./...`.
  - `test-unit`: `go test ./...` (the few allowed unit tests).
- **mise pin.** `MISE_VERSION = 2026.10.17` in the `Makefile`. Release assets on
  `https://github.com/jdx/mise/releases/download/v<version>/`:
  `mise-v<version>-<os>-<arch>[-musl][.exe]`, with `darwin` → `macos`,
  `amd64` → `x64`, `arm64` → `arm64`. Use the `-musl` builds for Linux (static, so
  they run on any distribution) and the plain `.exe` for Windows. Store them as
  `dist/mise-<goos>-<goarch>[.exe]`.
- **mise checksums.** Create `build/mise-checksums.txt` once from the release's
  `SHASUMS256.txt` (download over HTTPS), with only the six assets above, and commit
  it. `make dist` verifies each download against this file and fails on a mismatch.
  A new mise version updates the pin and this file in the same commit.
- Extend the existing `.gitignore` if needed.

Done when: `make build`, `make dist`, `make lint`, and `make test-unit` pass, and
`dist/hostel --version` prints the version.

## M1. Core with Docker rooms

Goal: the full lease flow on Linux rooms in Docker, with no cloud.

Build:

- Configuration (design, sections 3 and 9): providers with machines, platform
  directories, defaults, strict decoding, `admin config check`, `admin config
  schema`.
- Server: SQLite store and migrations, HTTP API (section 11), workers, scheduler
  (section 5), failures (section 6), gateway (section 13), port forwarding and the
  port process (section 14), reconcile, events, tokens.
- Providers: `static` and `external`.
- Helper: all commands in section 8 except `connect`, and the job supervisor
  (section 15); platform scripts selected by OS.
- Guest: all commands in section 10.
- `hostel server init`, `hostel server run`, and the `admin` commands.
- mise on rooms (section 9): upload, machine-level tools, `mise install` in the setup
  job, `mise env --json` in the job environment.
- Test infrastructure: `test/roomimage` (Debian with `sshd` and `git`, without
  mise and without the helper), `test/fakeprovider` (an `external` provider that runs
  `docker run`), `test/platforms/`, and the E2E harness (design, section 20:
  isolation, static rooms, time).
- `Makefile` targets `test-e2e` (`go test -tags e2e -count=1 -timeout 60m
  ./test/e2e/...`) and `test-e2e-10` (that command 10 times as separate runs; stops
  at the first failure).

E2E scenarios (each one through real binaries):

1. Acquire, sync, run, release. The slot directory no longer exists after cleanup.
2. Two guests, one slot: the second waits, sees its position, and gets the slot
   after the first releases.
3. Priority: a `high` lease passes a `normal` lease in the queue.
4. Aging: a `low` lease gets a slot before newer `normal` leases after the aging
   time.
5. Blocked machines: a lease for a full machine does not block a lease for another
   machine.
6. Exit code 69 at once for: an unknown platform, an unknown device, too many
   devices, and no machine with the required labels or enough tunnel ports.
7. `acquire --wait` ends before the lease is ready: exit code 75 and the "run the
   same command again" line; the same command continues the same lease.
8. Queued lease without polls: cancelled after `poll_timeout`.
9. Idle expiry after `idle_timeout`; an open `cmd` connection keeps the lease
   active; a `ports` connection does not; a long `run` that ends does not cause an
   immediate expiry.
10. `max_lease` ends an active lease.
11. Sync: ignored files are not sent; `include` files are sent; deleted local files
    are deleted on the room; files that a room command created stay; a second sync
    sends only changed files; a same-size edit within one second is sent; `diff`
    shows a room-side change; `overwrite` lists a room-side change to a synced file;
    type changes work (file `a` becomes directory `a/b`, and back).
12. Sync path safety: a symbolic link to a directory outside `work/`, followed by a
    file below that link name, does not write outside `work/`.
13. Jobs: a job survives a dropped connection; `attach` continues from the offset;
    `attach --wait` returns 75 while the job runs; a job that exits with 75 gives the
    "exited with code 75" line; a killed supervisor gives "lost".
14. Port forwarding:
    - a room command reaches a local HTTP server through a tunnel; the local server
      starts after the tunnel (port from `.env`); the tunnel reconnects after a drop;
    - the guest changes its local port (`.env` and `tunnel set --local-port`): the
      next connection reaches the new port without a restart;
    - `tunnel add` with a fixed `--room-port`, `tunnel set --room-port`, and `tunnel
      remove` work during a lease; new jobs see the new `HOSTEL_TUNNEL_<NAME>_PORT`;
      a fixed port that a room service uses fails with a clear message;
    - the gateway refuses a remote forward for a port that the lease did not
      register;
    - `hostel forward` reaches a server that a room job started;
    - a local server that listens only on `::1` is reachable through a tunnel;
    - setup commands can reach a guest service (the port process starts before
      setup);
    - an open forward or tunnel does not prevent idle expiry.
15. End of access: after release, the gateway refuses the lease key; open shells and
    the tunnel are closed; the next lease on the same slot can bind the same tunnel
    port. A key of another lease is refused. A direct SSH connection from the guest to
    the room container with the lease key is refused during and after the lease. A
    key that a guest adds to `authorized_keys` during a lease is removed by cleanup.
    An admin release closes an active guest shell at once.
16. Cleanup: a process that the lease detached (`setsid sleep 1000`) is stopped;
    `authorized_keys` is restored; the slot ports are free.
17. Server restart (`kill -9`) in each lease state (`queued`, `assigned`,
    `provisioning`, `preparing`, `ready`, `cleaning`): the lease continues correctly,
    and no second room is created (also with `kill -9` during the provider's
    `provision` call).
18. External provider: on-demand creation; a limit error keeps the lease queued and
    the retry after `limit_retry` works; 3 failed attempts fail the lease; warm
    reuse; `--fresh` gets a new room and evicts an idle warm room at the limit;
    destroy after `warm_running` (provider without stop).
19. Reconcile: a fake-provider room with the prefix that the database does not know is
    destroyed; a room without the prefix is not touched; a database room that the
    provider lost is marked destroyed and its `ready` lease fails with "room lost".
20. Failures: a `prepare` failure returns the lease to the queue and marks the room
    broken; a cleanup failure marks the room broken and no new lease uses it;
    `admin repair` makes a static room usable again.
21. Ownership: a guest token cannot release a lease of another token; an admin token
    can.
22. Platforms:
    - `linux` (an OS platform) works with no platform directory;
    - a device platform with scripts for `linux` and `macos` runs the right scripts on
      a `linux` machine and on an `external` machine with `os = "macos"`;
    - `--device` selects devices, the default is the first device, and
      `HOSTEL_DEVICES` and the device facts reach `hostel run`;
    - `hostel screenshot --device` returns the file that the script wrote;
    - a static room is ready for its platforms at server start; an on-demand room runs
      a platform `provision` only when a lease for that platform first uses it, and
      again after the platform scripts change.
23. Limits: a provider `max_rooms` limits rooms of two machines together; a provider
    `limits` entry limits a subset of machines; a machine `max_rooms` limits its own
    rooms; `--fresh` evicts an idle warm room at the machine limit; after a "limit
    reached" error with an idle Hostel room of that provider, the server evicts it
    and tries again.
24. Configuration errors stop the server with the file and line: an unknown key, a
    machine that lists an unknown platform or an OS platform, a static or external
    machine without `os`, an OS platform directory with devices, and a device
    platform without `prepare.<os>` for an OS that serves it.
25. Static host offline: at start, and also when it goes offline after start (the
    container stops during `preparing`), the room is `broken` ("unreachable"), a
    request for its platform is queued (not exit 69, not an attempt), and the room
    becomes usable by itself when the host is back.
26. Early cancel: a lease that is cancelled while its on-demand room is still in
    creation frees its slot without a helper call; the room becomes a warm room.
27. Restart with a waiting queue: the server is down longer than `poll_timeout`;
    after the restart, no queued lease is cancelled at once.
28. mise: a room without mise gets the Hostel mise binary; a repository `mise.toml`
    with an `[env]` value and one small tool makes the tool and the value available in
    `hostel run`; a second lease on the warm room does not download the tool again.
    The tool download needs internet; skip only this part when `HOSTEL_E2E_OFFLINE=1`.

Done when: `make test-e2e-10` passes on the owner's Mac (Docker through
OrbStack is installed).

## M2. Spare MacBook as a static iOS room

Goal: an agent builds the ExampleApp iOS app on the MacBook and gets a screenshot.

Owner tasks (write a short guide in `docs/static-room-setup.md` first):

- Create a macOS user `hostel` on the MacBook, turn on Remote Login for it, and add
  the server's public key.
- Install Xcode 26.5 and the iOS 26.5 simulator runtime.
- Give the MacBook's address.

Build:

- `platforms/ios/` (`platform.toml` and the `.macos.sh` scripts) with the devices
  `iphone-17-pro` and `ipad-pro-13`. Measure `simctl clone` + boot against
  `simctl erase` + boot, and select the faster clean device method.
- `platforms/macos/screenshot.macos.sh` for the Mac desktop (`screencapture`;
  check the Screen Recording permission over SSH). The owner sets automatic login on the MacBook so that it has a
  desktop session (add this to the setup guide), and the machine gets the label
  `desktop`.
- ExampleApp `hostel.toml` proposal (do not commit it to ExampleApp; give it to the owner).
- Check that the ExampleApp iOS build works in `work/` without `.git` (design, section 12).
- Check that `ps -E` shows the environment of the room user's processes (cleanup,
  design section 17).
- `hostel agent-guide` text for ExampleApp agents.

Done when: from an ExampleApp worktree with a local API, `hostel acquire ios`, `hostel run`
of the iOS build, app launch, sign-in against the local API through the tunnel, and
`hostel screenshot` work, on an iPhone and on an iPad device. A second lease starts
on the warm room and has a clean simulator. A `macos` lease gives a desktop
screenshot.

## M3. Namespace devbox (macOS)

Answer first, by experiment: the questions for M3 in `namespace.md`.

Build:

- `namespace` provider with `devbox` machines: provision, list (by name prefix),
  access, stop, resume, destroy, limit errors.
- The `macos` desktop screenshot on a devbox (`screencapture` or
  `devbox desktop screenshot`). Desktop UI control and Mac Catalyst builds are
  future work (owner decision, 2026-10-23).
- Warm policy, deadlines, reconcile against the real account.
- Opt-in smoke test (`HOSTEL_SMOKE_NAMESPACE=1`).

Safety: before each real test, run `devbox list -o json`. The plan allows one
devbox. If the owner has a devbox, stop and ask. Destroy everything that the test
created. Never touch other devboxes.

Done when: the M2 flow works on a devbox, and after `kill -9` of the server and a
restart, no orphan devbox remains.

## M4. Linux and Android

Answer first: the questions for M4 in `namespace.md`, and the time of a clean
Android emulator snapshot boot.

Build:

- `instance` machines in the `namespace` provider (or a Linux `devbox` machine, if
  KVM works there and it is simpler).
- `platforms/android/` with `.linux.sh` and `.macos.sh` scripts and a clean emulator
  for each lease. Test it on a Linux machine and on the MacBook
  (`mac-mini`). Namespace macOS devboxes do not get `android` (owner
  decision, 2026-10-27).
- A Linux Docker image or provision step with the Android SDK, if the boot time
  needs it.

Done when: an agent runs the ExampleApp Android app on a remote emulator against its local
API (`10.0.2.2` through the tunnel) and gets a screenshot.

## M5. VPS deployment

Answer first: headless `devbox` and `nsc` login on Linux; how cloud agents install a
binary from a private repository.

Build:

- TLS for the API (Caddy or `autocert`).
- Network rule for public static rooms (design, section 13): firewall that allows
  SSH only from the server, or `connect = true` with inbound SSH closed.
- `hostel room connect` for static rooms behind NAT, as a launchd and systemd
  service.
- Release builds (for example GoReleaser) and an install method.
- `docs/deploy-vps.md`.

Done when: a guest on the owner's Mac uses the home MacBook through a server on
an exe.dev VM (owner decision, 2026-10-27; a cloud agent is an optional extra
check).

## M6. Windows and several slots

- Windows on a static Windows host, the owner's Cloud PC `win-pc` (owner
  decision, 2026-10-29): verify SSH, write the `.windows.ps1` scripts, and test
  job objects and cleanup. `docs/m6-implementation.md` has the plan.
- Several slots per room: an OS user for each slot on static rooms, port and device
  separation for each slot, and E2E scenarios with two leases on one room. An
  exclusive lease (platform `exclusive`, or the `desktop` label) takes all slots of a
  room with two slots.

Done when: a guest on the owner's Mac uses `win-pc` through the exe.dev server
(sync, a .NET build, a detached job, a tunnel; no lease process remains after the
release), and the E2E suite has scenarios with two leases on one room
(`m6-implementation.md`, section 4).

## M7. Desktop testing (macOS, then Windows)

Owner decision, 2026-10-29. An agent builds, installs, starts, and removes a
desktop app, takes screenshots, and clicks and types in it, as on iOS and Android.
Not in scope: the repository's own UI test suites, signed builds.

- macOS first, on the home MacBook (a `desktop` machine): the `macos` platform with
  the label `desktop` gets device-like control (agent-device supports macOS: an
  accessibility snapshot, refs, and actions), install and cleanup of `.app`
  bundles, and a clean desktop after each lease.
- Windows after that: the owner selects the machine (a desktop session that stays
  drawn; not the owner's session on `win-pc`). agent-device has no Windows
  support; Hostel needs a bridge in the desktop session (UI Automation, input,
  screenshots), as ExampleApp's QA bridge does.

Done when: an agent on the owner's Mac builds a small macOS app on the MacBook,
starts it, reads its accessibility snapshot, clicks and types in it, and takes a
screenshot; after the release, the app is gone and the desktop is clean. The
Windows part gets its own done-when when the owner selects the machine.

## M8. Users and connection

Owner decisions, 2026-10-30; design: `multiplayer.md`, sections 3 to 6 and 9.

- Users with a unique name and a role (`admin`, `member`); tokens belong to a user
  and a computer. Existing guest and admin tokens become users.
- `hostel server init` asks for the server name, the public addresses, and the
  owner's name, and prints the first admin invite code.
- HTTPS on the API with the server's own certificate (TLS mode `self-signed`) or
  none behind a proxy (`off`); the gateway reachable over the same address
  (`/v1/gateway`).
- Invite codes; `hostel connect <code>` with the pinned key; `hostel disconnect`;
  a client without a connection accepts only `connect`.
- `hostel config users` (`invite`, `code`, `set`, `remove`), with the rule of the
  last admin.
- The user on leases, queue entries, events, and in `hostel status`.
- Queues: lanes `high`, `normal`, `low`, first in, first out; no aging. Admin
  actions: release any lease, move a queued lease, change its lane; the user's
  client prints who did it.

Done when: the owner invites a coworker; the coworker connects with the code to the
exe.dev server and to a test server that has only an IP address (TLS
`self-signed`); `hostel status` shows the leases of both users; the owner ends the
coworker's lease with `hostel release <lease>`, and the coworker's client says so.
(Moving queue entries has its API and E2E test in M8 and its command in M10.)

## M9. Remote configuration

Design: `multiplayer.md`, section 7.

- The configuration (providers, machines, settings) in the database with
  revisions; the import from `server.toml`; changes apply without a restart.
- Platforms in the database: the defaults (`ios`, `android`, `macos`, `windows`)
  embedded in the binary; own platforms; `config platforms new|pull|push|reset|remove`.
- `hostel config`, `config edit`, `config apply`, `config history`,
  `config rooms`, `config providers`, `config settings`.
- `hostel machine join <code>` (macOS, Linux, Windows) and `hostel machine
  connect` (the former `hostel room connect`); provider login through the client.

Done when: from the owner's computer, without a shell on the server, the owner
adds a new machine with one command on it, adds a platform of their own and a
lease for it runs on that machine, changes the machine's labels, and removes it.

## M10. Status view

Design: `multiplayer.md`, section 8.

- `hostel status` (aliases `ls`, `rooms`): interactive and inline in a terminal,
  plain text otherwise; updates on change (long poll on a status revision).
- Room and queue details; `hostel status <room|platform>` as plain text; the
  lease's jobs from the room; hardware from the probe.
- Admin actions in the view, with a confirmation.

Done when: the owner sees a coworker's lease, opens its details with its running
job, moves a queue entry, and ends a lease from the view; the same command in a
pipe prints plain text.

## M10b. Hostel app

Design: `m10b-implementation.md` (owner decisions of 2026-11-03).

- `hostel` without arguments in a terminal opens the full-screen app (Bubble
  Tea): Check in when the computer is not connected; else the menu Lobby
  (platforms and queues), Rooms (machines), Front desk (server, users, invites,
  add a machine, check out). Details and admin actions in each.
- The agent commands stay non-interactive; `hostel status` prints only plain
  text (the inline view of M10 goes away) with the next commands for agents.
- New server data: the branch of a lease; the live CPU and memory use of a
  room.

Done when: on `hostel.example.com`, the owner checks in a new computer from the app,
sees a coworker's lease in Rooms with its branch and running job and the live
use of the machine, moves an entry in a Lobby queue, ends a lease, invites a
user, and checks out; `hostel status` in a pipe prints the plain text.

## M10c. Public release

Design: `m10c-implementation.md` (owner decisions of 2026-11-03).

- MIT license; a README for people (what Hostel is, the one-command install, a
  short start); skills for agents in `skills/` (install, server setup, rooms,
  platforms, users, use, troubleshooting).
- `install.sh` (macOS, Linux) and `install.ps1` (Windows) from GitHub Releases,
  with checksums; the release workflow on a `v*` tag.
- The personal and internal details out of the files and the history.

Done when: the repository is public; `v0.1.0` is released; on a clean machine,
the one-line install works, and an agent that reads only the README and the
skills sets up a server, adds a Linux machine with SSH as a room, connects, and
runs a lease.

## M11. Hardening against backdoors (later; not required for version 1)

Goal: a guest cannot keep access to a room after its lease, even with a backdoor
that it installed during the lease. Design section 18, "Backdoors", lists the
measures.

- One OS user for each lease on macOS and Linux rooms; delete it at cleanup.
- Read-only shared caches; a writable cache for each lease.
- Optional egress firewall on rooms.
- A machine option to destroy on-demand rooms after each lease.
- Cleanup audit of persistence locations.

E2E scenarios: a reverse-shell process without `HOSTEL_LEASE`, a crontab entry, a
user LaunchAgent or systemd user unit, and a changed cache binary. None of them
survives cleanup or reaches the next lease.
