# M1 implementation plan

Status: proposal, 2026-10-21. Milestone M1 in `plan.md`: the full lease flow on
Linux rooms in Docker, with no cloud. This file gives the build order, the package
contents, the technical decisions inside the design, and the risks. `design.md`
stays the contract. When this plan adds a detail to the contract, the same commit
updates `design.md`.

## 1. Approach

- Build vertical slices. The first slice is one lease on one static Docker room,
  from `acquire` to cleanup (E2E scenario 1). Each later phase adds behavior and
  its E2E scenarios.
- Each phase ends with `make lint`, `make test-unit`, and `make test-e2e` passing,
  and with one or more local commits.
- `make test-e2e-10` runs at the end of M1 (phase 9). Earlier phases run the
  suite 3 times in a row to find flaky tests early.

## 2. Dependencies

| Module | Version | Use |
|---|---|---|
| `golang.org/x/crypto` | v0.58.0 | SSH server (gateway), SSH client (server to room, guest to gateway) |
| `modernc.org/sqlite` | v1.60.1 | server state |
| `github.com/pelletier/go-toml/v2` | v2.4.3 | configuration (`DisallowUnknownFields` gives line and column) |

No CLI framework. A small flag helper over the standard `flag` package allows
flags after positional arguments (`hostel acquire ios --task x`) and stops at `--`.

## 3. Technical decisions inside the design

These decisions add details to the design. Each one goes into `design.md` in
the commit that implements it.

1. **mise upload with gzip** (owner approved, 2026-10-21). The server compresses
   the mise binary with gzip while it sends it. The helper decompresses it, checks
   the SHA-256, and replaces the file with a rename. The Linux binary goes from 151
   MB to about 52 MB.
2. **Helper `install` command.** The helper table has no command to receive
   files from the server. Add `install <item>` (server only). Items: `mise`
   (gzip stream), `mise-config` (bytes), `platform <name>` (a `tar` stream; it
   replaces `hostel/platforms/<name>/`). The helper itself is uploaded before it
   exists, with `sh -c 'cat > tmp && chmod 755 tmp && mv tmp bin/hostel'`, and is
   then checked with `info`.
3. **Probe** also prints `$HOME`. All room paths in helper calls are absolute.
   `MISE_TRUSTED_CONFIG_PATHS` and the other `MISE_*` paths are absolute.
4. **Listeners with port 0.** `listen` and `gateway_listen` accept port 0. The
   server writes the real addresses to `<data_dir>/server.json` after it starts. If
   `gateway_public` is not set, the server uses the real gateway address. The E2E
   harness reads this file, so it never needs to reserve a port for the server
   (no race between parallel tests).
5. **Port process control socket on Windows.** Go supports Unix sockets on
   Windows 10 and later. Use a Unix socket on Windows too, in
   `%LOCALAPPDATA%\hostel\`, instead of a named pipe. This needs no extra
   dependency. Windows guests are not tested in M1, but the code compiles.
6. **Windows rooms.** The helper compiles for Windows. Job supervision and
   cleanup on Windows return the error "Windows rooms are not supported yet
   (milestone M6)". This is the minimum for `make dist`, not a placeholder for M1
   behavior.
7. **Server key table.** Add `port_process_offline` (default `10m`) to the
   server keys table in section 9 (it is used in section 14 but not listed).
   Done in phase 1, with: `rooms.home`, `leases.tunnels`, duration and path rules,
   script file names, and the repository platform `requires`.
8. **SQLite.** WAL mode, `busy_timeout = 5000`, `foreign_keys = on`, one open
   connection (`SetMaxOpenConns(1)`). All writes are short compare-and-set
   statements, so one connection is enough and there is no `SQLITE_BUSY`.
   Migrations are numbered SQL files, embedded, recorded in `schema_version`.
9. **Long polls.** An in-memory broadcast for each lease wakes waiting
   `GET /v1/leases/{id}` calls when the revision changes. The database stays the
   source of truth.

## 4. Server internals

### Workers

- One **loop** runs at start, after each event, and every `intervals.scheduler`.
  It runs the scheduler pass, then makes sure that each lease and room in a
  transitional state has exactly one worker goroutine.
- A **lease worker** drives `assigned → provisioning → preparing → ready` and
  `cleaning → final`. A **room worker** drives `creating → provisioning →
  running`, `stopping`, `resuming`, and `destroying`.
- Each step: read the row, do the I/O with a deadline, write the result with a
  compare-and-set. If the compare-and-set fails, the worker stops; the loop starts
  a new worker for the new state if necessary. Each step is safe to repeat after
  `kill -9`.
- **Timers** (idle expiry, `max_lease`, `poll_timeout`, warm stop and destroy,
  `limit_retry`) are checks in the loop, not goroutines.
- **Reconcile** runs at start and every `intervals.reconcile`.

### Scheduler (`internal/scheduler`)

A pure function: input = queued leases, rooms, slots, machines, limits, full
flags, and the time. Output = assignments (lease → slot), room actions (create,
resume, evict), and the new queue positions. Unit tests cover: effective priority
and aging, order, blocked machines, the five options, `--fresh`, exclusive leases,
and the three kinds of limits.

### Room operations (`internal/roomops`)

- SSH client with the server key. It pins the host key at the first connection
  (`rooms.ssh_host_key`). A different key makes the room `broken` ("host key
  changed").
- `Probe`, `Upload` (hash compare, then `install`), `RunScript`, `Begin`,
  `Cleanup`, `Health`. Each call has a deadline from the configuration.
- A dial or handshake failure returns a typed `ErrUnreachable`, so the worker can
  apply the "unreachable" rules of section 6.

### Providers

- `static`: `Access` dials `address`. `Provision`, `Stop`, and the others are not
  called.
- `external`: runs the command with the JSON contract and the configured
  timeouts. It maps the `limit` and `not_found` errors to typed errors.

### Gateway (`internal/gateway`)

- SSH server with the gateway host key. Public key authentication for the user
  `<lease-id>.<cmd|ports>`. The key must be the lease key, and the lease must be
  `ready`.
- For each guest connection: one upstream SSH connection to the room.
- Sessions: `exec` and `shell` are wrapped in the helper (`room exec
  --command-b64`, `room shell`). Requests `pty-req`, `env`, `window-change`,
  `signal`, `exit-status`, and `exit-signal` are relayed.
- `direct-tcpip` only to `127.0.0.1` on the room. `tcpip-forward` only on `ports`
  connections and only for registered room ports. The gateway relays
  `forwarded-tcpip` channels back to the guest.
- A registry of open connections for each lease. The lease worker closes them when
  the lease leaves `ready`. `cmd` connections update `last_activity_at` when they
  open and close.

## 5. Helper (`internal/room`)

All commands of section 8 except `connect`, plus `install` (section 3 above) and
the hidden `job supervise`. Details:

- **Job supervisor.** `setsid` on Linux and macOS. The command runs in its own
  process group. `supervisor.json` records the PIDs and the start times (Linux:
  field 22 of `/proc/<pid>/stat`; macOS: `ps -o lstart=`).
- **Cleanup step 1.** Linux: read `/proc/<pid>/environ` of the processes of the
  room user. macOS: `ps -E` (verified in M2). SIGTERM, wait up to 10 seconds,
  SIGKILL.
- **Sync.** `internal/syncfs` has the manifest, the plan, and the path-safe
  writes. Unit tests are allowed for the plan (`AGENTS.md`).
- **Environment.** The six sources of section 10, in order. `mise env --json`
  runs with a deadline.

## 6. Guest (`internal/guest`)

- Configuration: `~/.config/hostel/config.toml`, `HOSTEL_SERVER`, `HOSTEL_TOKEN`.
- Local state in `$(git rev-parse --git-dir)/hostel/<platform>/`, written with
  a temporary file and a rename.
- `acquire` steps 1 to 9 of section 10. Each step records its result. `--wait`
  covers the whole command.
- SSH client to the gateway with the lease key and the pinned gateway host key.
  It reconnects for `attach`.
- **Port process** (`hostel ports`, hidden). Started detached (`setsid`). One
  `ports` connection to the gateway. Control socket with one JSON request and one
  JSON response for each connection.
- All commands and exit codes of section 10, including `--json` for `run` and
  `attach`.

## 7. Test infrastructure

- **`test/roomimage`.** `debian:13-slim` with `openssh-server`, `git`, `ca-certificates`,
  and a user `hostel`. No helper, no mise. sshd allows only public key
  authentication. The server key goes into `authorized_keys` at container start
  (an environment variable read by the entry point).
- **`test/fakeprovider`.** A Go program that implements the external contract with
  `docker run` (label `hostel-e2e=<run id>`, name = room name, an explicit port
  `127.0.0.1:<free port>:22`). A lock file for each name makes `provision`
  idempotent under concurrent calls. Environment variables control the delay, a
  limit, and failures.
- **`test/platforms/`.** A device platform `fakedevice` with scripts for `linux`
  and `macos`. The scripts write `device.env` and a fake PNG, and record each call
  in a file, so tests can check which scripts ran.
- **Harness (`test/e2e`).**
  - `TestMain` builds the host binary and the Linux helpers for the Docker
    architecture once, and checks that the mise binaries exist in `dist/`
    (`make test-e2e` runs `make dist` first).
  - It removes Docker objects with a `hostel-e2e` label that are older than 2 hours,
    and `hostel ports` processes of earlier runs.
  - Each test: a run ID, a temporary home directory, a temporary git repository, its
    own server (`server_id` random), and helpers to run guest commands and to wait
    for a condition with a deadline.
  - Each test removes the containers, networks, and volumes with its own label,
    also when it fails.
  - Server process control: start, stop, `kill -9`, restart.
- **Docker architecture.** OrbStack on the owner's Mac runs `linux/arm64`
  containers. The harness asks Docker for the architecture.

## 8. Phases

| Phase | Content | E2E scenarios (`plan.md`, M1) |
|---|---|---|
| 1 | `internal/protocol` (states and priorities; API and helper types come with their first use), `internal/config` (server, platform, repository, guest; defaults; strict decoding; key paths in errors; JSON Schema), `admin config check|schema`, `internal/store` with migrations, `internal/scheduler` with unit tests | — |
| 2 | Test infrastructure (section 7). `hostel server init` and `server run` with configuration check only | 24 |
| 3 | `roomops`, `static` provider, helper `info`, `install`, `begin`, `run-script`, `cleanup`; server workers for static rooms; HTTP API; tokens; events; `hostel status` | — (room readiness is checked inside scenario 1) |
| 4 | Gateway (`cmd` only); helper `exec`, `shell`, `set-env`, `sync-plan`, `sync-apply`, `diff`, `tar-in`, `tar-out`, jobs; guest `acquire`, `sync`, `run`, `attach`, `jobs`, `stop`, `cp`, `diff`, `ssh`, `ssh-config`, `release`; mise setup job | 1, 11, 12, 13, 16 |
| 5 | Queue and lease lifecycle: positions, priority and aging, blocked machines, request checks, `--wait`, poll timeout, idle and `max_lease` expiry, ownership, platforms and devices, `screenshot`, `platforms`, `agent-guide` | 2, 3, 4, 5, 6, 7, 8, 9, 10, 21, 22 |
| 6 | Port process, `ports` gateway purpose, tunnels, forwards, `PUT /v1/leases/{id}/tunnels`, end of access | 14, 15 |
| 7 | `external` provider, on-demand rooms, warm policy, eviction, limits, reconcile, failures, repair, offline static host, early cancel, restarts | 17, 18, 19, 20, 23, 25, 26, 27 |
| 8 | mise on rooms: machine-level tools, repository tools, cache between leases | 28 |
| 9 | Stability: `make test-e2e-10`; fix flaky tests; update `design.md` and `README.md` | all |

## 9. Risks

| Risk | Control |
|---|---|
| The E2E suite is slow (about 50 rooms in each run, each with a 150 MB mise upload). | Parallel tests; gzip upload; measure the suite time in phase 4 and report it. |
| Flaky timing tests (prototype lesson). | Wait for conditions with deadlines; wide margins; run the suite 3 times at each phase end. |
| `kill -9` during `provision` creates a second container. | A lock file for each name in the fake provider; scenario 17 checks the container count. |
| Interaction of SSH remote forwards with OpenSSH on the room (`localhost` bind for both loopback addresses). | Scenario 14 tests `127.0.0.1` and `::1`. Containers have an IPv6 loopback (checked). |
| Scenario 28 needs internet and a GitHub download. | Skip that part with `HOSTEL_E2E_OFFLINE=1`; use a small tool. |
| Docker resources on the shared Mac (8 CPU, 8 GB). | Limit test parallelism (`-parallel 4`) if the suite overloads the machine. Never touch containers that the tests did not create. |
