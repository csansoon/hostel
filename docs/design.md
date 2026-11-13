# Hostel design

Status: ready for implementation. Read `requirements.md` first. This document uses
its vocabulary and requirement IDs (R1, N1, ...). `plan.md` gives the build order.

## 1. Overview

```
                  HTTP + JSON (control path)
  guest ──────────────────────────────────────► hostel server ───► providers
    │                                              │  (static hosts, Namespace,
    │   SSH to the server gateway                  │   external command)
    └──────────────────────────────────────────────┤
                       (data path)                 ▼
                                                 machine (rooms, helper, tools)  
```

- **Control path.** The guest lists tools, creates a lease, waits, and releases
  it. The messages are small JSON documents over HTTP.
- **Data path.** The guest uses SSH to send files, run commands, and keep a reverse
  tunnel. All guest SSH traffic goes to the **gateway**, an SSH server inside the
  Hostel server.

Why all guest SSH goes to the gateway:

- Namespace machines are reachable only through the Namespace SSH proxy, which needs a
  Namespace login. Guests must not have that login.
- A static machine behind NAT (the home MacBook) cannot accept connections from a VPS
  or from cloud agents. It connects out to the gateway (section 13).
- When a lease leaves `ready`, the gateway closes all its connections at once.
- The gateway sees open sessions, so the server can measure guest activity.

**Access is always temporary and only through Hostel.** The gateway terminates the
guest's SSH connection and opens its own SSH connection to the machine with the
server's credentials. The guest never holds a credential that the machine accepts: the
lease key is valid only at the gateway, and only while the lease is `ready`. When
Hostel ends a lease (release, expiry, or an admin decision), access ends at once and
cannot come back without a new lease. This is true for every provider, including a
static VPS (section 13).

**Trust model.** Guests are the owner's own agents. Hostel protects against
accidents (state that goes from one lease to the next, forgotten machines, leaked
secrets in logs). Version 1 does not protect a machine against a hostile guest
(section 18, "Backdoors"). In version 1 the guest and the server use the same OS user
on a machine.

## 2. Binary and code layout

One Go binary, `hostel`, for darwin, linux, and windows on amd64 and arm64.

| Mode | Commands | Runs on |
|---|---|---|
| Guest | `hostel connect`, `hostel tools`, `hostel acquire`, `hostel run`, ... | agent machines (a user's computer) |
| Config | `hostel config users ...` | any connected computer; changes need an admin (M8; `multiplayer.md`) |
| Admin | `hostel admin ...` | machine tokens, machine control; `config check|schema` run locally |
| Server | `hostel server init`, `hostel server invite`, `hostel server run`, `hostel server update` | the server machine (`update --remote`: an admin's computer) |
| Helper | `hostel helper ...` (hidden from help) | machines; the server uploads it |

Machines need no permanent daemon. Exception: a static machine behind NAT runs
`hostel machine connect` (before M9: `hostel helper connect`) as a service (launchd, systemd, or a Windows service).

Distribution. A tag `v*` makes a GitHub release (`make release`): the binary for
each OS and architecture, the pinned `mise` binaries, `SHA256SUMS`, and (from
v0.2.0) `SHA256SUMS.sig`. `install.sh` installs the binary for the machine, or
with `--server DIR` also the helper binaries; it checks each file against
`SHA256SUMS`. The default tools are in the binary (M10e): there is no copy of
them on the server machine.
`docs/deploy-vps.md` shows a server on a VPS.

### Releases and updates (M10d)

- **Signature.** The release workflow signs `"hostel-release <tag>\n" +
  SHA256SUMS` with Ed25519 (`cmd/signrelease`, the repository secret
  `HOSTEL_RELEASE_KEY`); `SHA256SUMS.sig` is the signature in base64. The
  public key is in the binary (`internal/update`). The tag in the signed text
  stops an old release that is served under a new tag. The install scripts
  check `SHA256SUMS` only (the first install trusts HTTPS).
- **Source.** `https://github.com/csansoon/hostel/releases`: the latest tag is
  the redirect of `<base>/latest`; the files are `<base>/download/<tag>/<file>`.
  `HOSTEL_UPDATE_URL` and `HOSTEL_UPDATE_KEY` replace the base and the key (a
  mirror, the E2E tests). Deadlines: 30 s for the tag and the small files, 10
  min for a binary.
- **Versions.** A release is `vX.Y.Z`; a `git describe` build
  (`vX.Y.Z-N-gHASH[-dirty]`) is after `vX.Y.Z`; other builds (`dev`) are
  unknown: they get no notice, nothing refuses them, and an update replaces
  them. An update refuses a release that is not newer.
- **The CLI.** `hostel update [--check] [--version v]` downloads the binary,
  checks its checksum and that it prints the release version, and renames it
  over the running binary (on Windows the running file moves to
  `hostel.exe.old` first, and each start removes an old one). Exit codes: 0, 1,
  69 (the source cannot be reached).
- **Versions between guest and server.** Each guest request has the header
  `X-Hostel-Version`, and each server answer too. The server refuses a known
  guest version older than its minimum guest version (`v0.4.0`) with HTTP 426
  (`update_required`); a request without the header passes. When the server
  is newer, a guest command prints one line on stderr, at most once in 24 hours
  (`~/.config/hostel/update-notice`).
- **The server.** `update.Server` downloads and checks each file of
  `helper_dir` that differs from the release (`hostel-*`, `mise-*`) into new
  temporary files next to it, and only then renames them into place, then the
  server binary (a
  copy of its new helper file, checked with `--version`); it checks first that
  the server's user can write both directories. The default tool definitions are in
  the binary. `hostel server update` on the server machine does it for the
  binary that the service ran at its last start (`executable` in
  `data_dir/server.json`), not for the binary that runs the command, and prints
  the restart command. `POST /v1/server/update` (admin, section 11) does it in
  the server; then the server stops as at a signal and starts the new binary in
  the same process (`exec`, macOS and Linux), with the same arguments and
  environment. Leases and jobs continue (a job runs under its supervisor on the
  machine); a guest stream through the gateway (`hostel run`, `hostel ssh`) ends
  at the restart, and `hostel attach` continues a job. A signal during the
  update stops it and the server, without the new binary. Machines get the new
  helper before their next lease (section 8).
- **Machines that joined.** `hostel machine connect` sends
  `SSH-2.0-hostel-<version>` as its SSH client version; the status has it as
  `connect_version` of the machine while it is connected (`v0.1.0` for an
  older service, which sends no version). `hostel machine
  update` (as an administrator on the machine) downloads the server's binary
  for the machine (`/v1/download`, with the TLS of the join), checks that it
  runs, replaces the binary in the service directory, and restarts the
  service. The service does not replace itself: it cannot write its directory.
- **The app** reads the latest tag at start and every 6 hours
  (`HOSTEL_NO_UPDATE_CHECK=1` turns it off; errors are silent): Update in the
  main menu, Update the server in Front desk (admins), and "Out of date" on
  machines whose connect service is older than the server.

Dependencies. Use pure Go only, so that cross-compilation needs no C toolchain:

- `golang.org/x/crypto/ssh` (gateway, guest SSH, server-to-machine SSH).
- `modernc.org/sqlite` (server state).
- `github.com/pelletier/go-toml/v2` (configuration, strict decoding).
- The standard library for HTTP, `log/slog`, `archive/tar`, and `os/exec`.

Add another dependency only when it removes a large amount of code.

Suggested layout:

```
cmd/hostel/            main: mode dispatch
internal/protocol/     API and helper request and response types
internal/config/       server, tool, repository, and guest configuration; defaults; schema
internal/server/       HTTP API, workers
internal/scheduler/    queue order and room selection (pure functions)
internal/store/        SQLite schema, migrations, compare-and-set updates
internal/gateway/      SSH gateway
internal/provider/     Provider interface; static/, namespace/, external/
internal/machineops/   server-side operations on machines (probe, upload, scripts, cleanup)
internal/guest/        CLI commands, local state, sync client, port process
internal/helper/       helper commands
internal/syncfs/       manifests, sync plans, safe file writes
internal/update/       releases: source, signature, versions, updates (M10d)
cmd/signrelease/       signs SHA256SUMS in the release workflow
tools/                 the default tool definitions (embedded in the binary; section 8)
test/e2e/              E2E tests (build tag e2e)
test/fakeprovider/     external provider for tests; creates Docker machines
test/machineimage/     Dockerfile for test machines
test/tools/            tool definitions for tests
```

## 3. Configuration model

The owner configures two things: **providers** (with their machines or templates)
and the **tools** of each machine or template (owner decision, 2026-11-05; `m10e-implementation.md`).
The guest sees only tools.

### Providers and machines

- A **provider** is a source of machines, with its own settings: kind, login,
  account limits, and defaults for its machines.
- A `static` provider is a named group of real hosts: each entry under
  `machines` is one machine. Its `label` is the name that people see, for example
  "Office" or "Lab".
- An on-demand provider (`namespace`, `external`) has **templates**. A template
  gives up to its `max_machines` machines at the same time. The provider creates
  them when a lease needs one.
- A machine or template declares its OS (the `os` key, or from its `spec` for
  Namespace), the tools it gives, and its rooms.
- Its settings inherit from the provider and can override them. For `env`, its keys
  are merged into the provider's keys and replace keys with the same name. Its
  `tools` list replaces the provider's list.
- In the code, `config.Template` describes both forms: a static entry gives exactly
  one machine.

### Tools

A **tool** is something that a machine gives to a lease:

- **Software:** `android-sdk`, `jdk`, `xcode`, `dotnet`, `node`, `docker`. It is
  installed on the machine and is always usable in each lease on it, also when
  the lease did not ask for it.
- **Screen tools:** `android-emulator`, `ios-simulator`, `macos-desktop`,
  `windows-desktop`. The agent looks at them and acts on them. Hostel starts a
  screen tool only for a lease that asks for it, takes screenshots of it, has its
  driver ready, and cleans it up at the end. A screen tool can take the whole
  machine (the desktops: there is one screen).

An **option** is a version of a software tool, or the phone or tablet of an
emulator or a simulator: `node:22`, `xcode:26`, `android-emulator:pixel-9`. In a
request, an option is a filter: `node` matches any machine with any `node`,
`node:22` only a machine whose `node` matches `22`. A version option matches the
whole version or its first parts (`xcode:26` matches `26.1`; `node:22` matches
`22.11.0`, not `2.2`). The option of a screen tool matches its name exactly.

There are two kinds of tool:

| Kind | Examples | Definition |
|---|---|---|
| **mise tool** | `node:22`, `jdk:21`, `python:3.13`, `go:1.25` | None: any tool that mise can install. The server installs it with mise at machine provisioning. The option is the version to install; a machine or template must have it (`node` alone is an unknown tool, so a typing error such as `andriod-sdk` fails the check). A `needs` entry can name a mise tool without a version: the machine then gets the latest version, unless it lists one. `jdk` is the mise tool `java`. |
| **Defined tool** | `android-sdk`, `android-emulator`, `xcode`, `ios-simulator`, `macos-desktop`, `windows-desktop` | A directory with `tool.toml`, scripts for each OS, and `GUIDE.md` (section 8). Hostel comes with these defaults; an admin can change them and add own tools. |

The OS is a property of the machine, not a tool. Agents do not ask for an OS:
tools work the same way on each OS that has them. Only a desktop tool belongs to
one OS. A lease shows the OS of its machine (section 10).

Tools of the repository (its `mise.toml`: node, pnpm, Python) are not machine
tools. They are set up for each lease on any machine (section 9, "mise on
machines").

There are no platforms, devices, or labels. The configuration check fails on the
old keys (`platforms`, `labels`, `requires`) with a message that names the new
form.

## 4. Data model

The configuration (section 9) defines providers, machines, templates, and their tools. The
database stores runtime state.

| Table | Main columns |
|---|---|
| `machines` | id, name, provider, template, provider_ref, state, os, arch, home, shell, ssh_host_key, helper_sha256, provider_deadline, warm_until, drained, broken_reason, created_at, state_changed_at |
| `machine_tools` | machine_id, tool, scripts_sha256, state (`installing`, `ready`, `failed`), option (the version or the options that `check` reported), env (the tool's environment names), checked_at |
| `rooms` | machine_id, name, state, lease_id, cleanup_data, home |
| `leases` | id, token_id, manual_position, began_at, state, revision, position, tools (the request: `["android-emulator:pixel-9"]`), started (the screen tools that started, with their options), task, priority, tunnels, fresh, public_key, guest_host, repo, worktree, provider, template, machine_id, room_name, attempts, reason, created_at, last_poll_at, last_activity_at, ready_at, expires_at, ended_at, end_state, end_reason |
| `lease_tunnels` | lease_id, name, machine_port |
| `users` | id, name (unique, not case-sensitive), role (`admin` or `member`), created_at, removed_at (M8) |
| `tokens` | id, name, role (`user` or `machine`), template (`<provider>/<machine>`, for `machine` tokens), sha256, created_at, revoked_at, user_id, computer |
| `invites` | id, kind (`user` or `computer`), secret_sha256, role, name, user_id, created_by, created_at, expires_at, used_at (M8) |
| `events` | id, time, lease_id, machine_id, kind, message, user_id (who caused it; 0: the server) |

`machines.home` is the machine user's home directory from the probe. `leases.end_state`
is the final state that a lease in `cleaning` goes to. `leases.tunnels`
holds the requested tunnels (`[{"name", "machine_port"}]`) until the server assigns
the machine ports in `lease_tunnels`. Times are Unix milliseconds.

`machine_tools` is the **inventory** of a machine: each of its tools, the version
of the tool's scripts that installed it, its state, and the real option that
`check` reported (a version, or the phones and tablets of a screen tool). Jobs are
not in the database. They exist only on
machines (section 15).

**Transactions.** Each state change is a compare-and-set in one short SQLite
transaction (`UPDATE ... WHERE id = ? AND state = ?`). Never hold a transaction
during network I/O: read the state, do the I/O, then write the result with a
compare-and-set. Each worker step is idempotent, so a worker can repeat it after a
restart.

`leases.started` holds the screen tools whose `start` ran (written before the
script runs, because a failed start can leave processes that cleanup must stop), each with its options
and, after its `start` succeeded, the names of its `tool.env` facts. "Waiting behind N" is not a column: the server keeps the
result of the last scheduler pass in memory (a change of it also changes the
position, so the revision rule below holds).

**Revision.** A change of a lease's state, reason, or queue position increments
`revision`. The scheduler stores `position` in the lease row and writes it only when
it changes. The estimated wait is computed at each response and does not change the
revision. Long polls use the revision (section 11).

### Lease states

```
queued ─► assigned ─► provisioning ─► preparing ─► ready ─► cleaning ─► final
   │          │             │              │          │
   │          └─────────────┴──────────────┴──────────┴──► cleaning (release, cancel, expiry, error)
   └──► final (cancelled before a room was assigned)

assigned, provisioning, preparing ──► queued (machine failure; section 6)
```

| State | Meaning |
|---|---|
| `queued` | Waits for a room. |
| `assigned` | A room is reserved for this lease. |
| `provisioning` | The machine is in creation or resume, or a tool's `install` script runs. |
| `preparing` | The server checks the helper and scripts, writes the lease data, and runs the `start` script of each requested screen tool. |
| `ready` | The guest can connect. |
| `cleaning` | Cleanup runs (section 17). |
| final: `released`, `expired`, `cancelled`, `failed` | `end_reason` records why: guest release, idle, max duration, poll timeout, machine lost, or error text. |

Rules:

- A lease with a room always goes through `cleaning` before a final state.
- If the server has not yet called the helper `begin` command for the lease
  (`leases.began_at` is empty), cleaning only frees the room. It does not call the
  helper, so a lease that ends while its machine is still in creation does not break the
  machine. The machine continues its creation and becomes a warm machine.
- Exception: if the machine is lost (reconcile, section 7), cleanup is not possible.
  Leases on it in `ready` or `cleaning` go directly to `failed` with
  `end_reason = "machine lost"`. Leases in `assigned`, `provisioning`, or `preparing` go
  back to `queued`.
- A lease that was not yet `ready` when its machine failed goes back to `queued`
  (section 6, failures).

### Machine states

| State | Meaning |
|---|---|
| `creating` | The row exists; the provider call runs. |
| `provisioning` | The server probes the machine, uploads the helper, mise, and the tool directories, installs the mise tools, runs the `install` and `check` scripts, and writes the inventory. |
| `running` | Usable. Rooms have their own states. |
| `stopping`, `stopped`, `resuming` | Warm stop and resume (providers that can stop). |
| `destroying`, `destroyed` | Removal. |
| `broken` | Not usable until repaired (static) or destroyed (on-demand). |
| `retired` | A static machine that was removed from the configuration. It takes no new lease. |

`drained` is a flag, not a state: a drained machine takes no new lease.

### Room states

`free` → `assigned` → `preparing` → `leased` → `cleaning` → `free`.

- A failed cleanup sets the room to `broken` and the machine to `broken`.
- **Dirty room.** If a lease leaves a room after `begin` (`began_at` is set) and
  cleanup cannot run now (the machine is unreachable), the room becomes `dirty` and
  records the lease ID, its started screen tools, and its ports in
  `rooms.cleanup_data`. The
  machine runs this cleanup before it takes a new lease: when a probe succeeds again,
  the server runs the cleanup of each dirty room, then sets the machine to `running`.
- When a lease returns to `queued`, its `began_at`, machine, and room are cleared. A new
  assignment calls `begin` again. Its old room keeps no `lease_id` (a dirty room has
  the lease data in `cleanup_data`), and every room update for a lease is limited to
  the lease's current machine, so a later step of the lease never changes the old room.

An append-only `events` table records each change. `hostel status` and debug
output read from it.

## 5. Scheduling

The scheduler runs after each event (new lease, release, machine ready, machine failed)
and every `intervals.scheduler` (default 10 seconds). It is a pure function in
`internal/scheduler`: input = queued leases, machines, rooms, templates, limits, and
time; output = assignments and provider actions. Workers do the provider actions
after the pass. When an action fails in a way that changes the options (for example
a resume that returns "not found"), the worker updates the state and starts a new
pass at once. A unit test is allowed for the scheduler (see `AGENTS.md`).

**Order** (M8, owner decision 2026-10-30). Each lease has a lane: `low` = 0,
`normal` = 1 (default), `high` = 2; an agent sets a lane only when the user asks
for it. Order: leases that an admin placed (`manual_position` > 0, in that
order), then the lane (high first), then the request time. There is no aging: an
old `low` lease stays behind a new `normal` one. There is one waiting line for
all leases (owner decision, 2026-11-05), not one for each kind of request. An
admin places a queued lease first or before another queued lease
(`POST /v1/leases/{id}/move`): the
lease and the leases before it get manual positions, so they keep that order
until they start. An admin can also change a queued lease's lane.

**Candidates.** A machine or a template is a candidate for a lease if all of these
are true:

1. It has every requested tool, and each requested option matches: the machine's
   inventory (`machine_tools`) when it has one, else the configured `tools` (a
   template before its first machine, a machine before its first
   install). A requested tool also needs the tools in its `needs`; the
   configuration gives them to each machine or template that lists the tool
   (section 8). A lease with no tools has every machine and template as a
   candidate.
2. Its rooms have at least as many pool ports as the lease has tunnels without a
   fixed `machine_port`.
3. For `--fresh`: it is a template.

The server checks the request at `POST /v1/leases`: each tool exists, each option
of a screen tool is in the tool's `options`, the number of options of one screen
tool is at most its `max_options`, and at least one machine or template is a
candidate (online or offline). Else the request fails at once (R4, owner decision
2026-11-05) with the reason, for example "no machine has xcode and android-sdk
together; machines with xcode: home/macbook". When all candidates are offline,
the lease waits, and the answer says so. This check uses the inventory of a
machine row only when the row matches the current settings of its machine or template
and its install is current: a stale machine
installs again before its next lease, so an old version cannot admit a request.

**Assignment.** For each queued lease, in order, select the first possible option.
Inside each option, candidates are in configuration order (providers in file
order, then machines or templates in file order):

1. a `free` room in a `running`, not drained machine that is ready for the lease
   (each requested tool and its needs are `ready` in `machine_tools`, with the
   current scripts) and has enough remaining provider lifetime (section 7,
   deadlines);
2. a `free` room in a `running`, not drained machine that is not yet ready for the
   lease (the `install` scripts run first);
3. a `stopped` machine that the provider can resume;
4. a new machine, if the template and its provider are under their limits;
5. a new machine after **eviction**: if each limit that blocks option 4 is full only
   because of Hostel machines with no lease (`running` with all rooms free, or
   `stopped`) in that limit's scope, destroy the oldest of them, then create a new
   machine.

`--fresh` allows only options 4 and 5.

**Exclusive leases.** A lease is exclusive if one of its tools has
`whole_machine = true` (the desktops; section 9, "The Mac desktop"). An exclusive
lease needs a machine where all rooms are free, and it reserves all of them.

**Options of a screen tool.** A lease that names no option of a requested screen
tool gets the first option that the machine has, in the order of the tool's
`options`.

If no option is possible, the lease **blocks** its candidates. A lease later
in the order cannot take a room or a new machine from a blocked candidate. It can use
other candidates. This prevents starvation and still lets leases that other
machines can serve continue: a lease that waits for xcode does not block a lease
for android-sdk on a Linux machine.

**Limits.**

- A static entry is always exactly one machine.
- Template `max_machines` (default 1) counts its machines that are not
  `destroyed` (running, stopped, and broken).
- Provider `max_machines` (default: no limit) counts all Hostel machines of all its
  templates that are not `destroyed`.
- Provider `limits` count the machines of a subset of its templates:
  `limits = [{ templates = ["mac", "linux-devbox"], max_machines = 1 }]`. Use them for
  account limits that apply to some products only (for example "one devbox per
  user", which does not apply to instances).
- A provider "limit reached" error marks that **template** as full. The flag clears
  when a Hostel machine of that provider that existed at the provider is destroyed, or
  after `limit_retry` (default 2 minutes). The time-based retry is necessary because
  machines that Hostel did not create (for example the owner's own devbox) can use
  the limit.
- After a "limit reached" error, if the provider has Hostel machines with no lease, the
  server evicts the oldest of them and tries again at once (not an attempt). This
  covers limits that Hostel cannot count, for example a vCPU limit.
- A `creating` row whose provider call failed (limit or other error) is set to
  `destroyed` after a `Destroy` by name (the provider returns `not_found` if nothing
  exists). This does not clear the full flag, because the machine never existed at the
  provider.
- Machine rows are never deleted. `machines.id` uses SQLite `AUTOINCREMENT`, so an ID and
  its provider name are never used twice.

**Estimated wait.** A whole-machine request counts machines with all rooms free, not
rooms. For each waiting lease: the number of leases before it in the
order that share one of its candidates, plus one for the lease itself, ×
the median lease duration on its candidates (from the last 50 leases) ÷
the number of rooms on its candidates. When the candidates have no free
room and an on-demand candidate can create a machine, the lease waits for a new
machine: the estimate is the leases before it (without the lease itself) × the
median ÷ the rooms, plus the median creation time of the last 20 machines of its
candidate templates (from the machine row to its first `running` state,
`machines.ready_at`, migration 012; 5 minutes before the first machine). Show it as an
estimate, with "waiting behind N leases"; `hostel tools` adds "(new machine)".

## 6. Failures

| Failure | Result |
|---|---|
| Provider "limit reached" | Template full (section 5). The lease returns to `queued`. Not an attempt. |
| Other provider error at create or resume | Retry with backoff. **3 attempts in total**, then the lease fails with the provider message. |
| Machine provisioning fails (probe, upload, mise tools) | The machine becomes `broken`. The lease returns to `queued`. This is an attempt. |
| SSH connection to a machine fails (dial or handshake), in any step | Not a script failure. A static machine becomes `broken` with reason "unreachable"; its room becomes `dirty` if the lease had called `begin` (section 4, room states); the machine recovers by itself when a later probe succeeds (each reconcile) and its dirty rooms are cleaned; the lease returns to `queued` (not an attempt). An on-demand machine is destroyed; the lease returns to `queued`, and this **is** an attempt, so a template with a bad SSH setup cannot create machines without limit. A lease that was `ready` fails with "machine unreachable". |
| A tool's `install` script fails, or its first `check` fails | The tool is `failed` in the machine's inventory; the machine becomes `broken`. The lease returns to `queued`. This is an attempt. |
| A screen tool's `start` script fails | Run the `stop` scripts of the screen tools that already started and of the tool that failed, in reverse order, and cleanup for the room. The machine becomes `broken`. The lease returns to `queued`. This is an attempt. |
| A lease reaches 3 attempts | The lease fails with the last error message. |
| Cleanup fails | Room and machine become `broken` (R24). |
| `mise install` or repository setup fails (guest side) | The lease stays `ready`. `acquire` exits with code 1 and prints the job ID and the end of its output. The agent can fix the code and run `hostel sync --setup`. |
| Machine lost (reconcile) | Leases in `ready` or `cleaning` fail with "machine lost". Leases in `assigned`, `provisioning`, or `preparing` return to `queued`. |
| Resume fails with "not found" | Mark the machine `destroyed` and start a new scheduler pass at once. |

Broken on-demand machines are destroyed. Broken static machines stay until the owner runs
`hostel admin machine repair <machine> [--accept-host-key]`. Repair runs the full cleanup
(section 17) of each `broken` or `dirty` room again (this also restores
`authorized_keys`), then the `check` scripts of the tools that the machine has
installed, and sets the machine to `running` if all
pass. It does this through the probe: a broken room (with its `cleanup_data`)
becomes `dirty`, and the machine goes to `provisioning`; the probe cleans each dirty
room before the machine is `running`.

**Workers and retries.** A machine worker ends when a step does not change the machine
state (for example a failed destroy); the next loop pass starts it again, so such
a retry follows `intervals.scheduler`. The provider `provision` call is retried
inside the worker with backoff (2 s, 4 s), three attempts in total.

**Machine host key.** The server pins a machine's SSH host key at the first connection. A
different key later makes the machine `broken` with reason "host key changed". Only
`admin machine repair --accept-host-key` accepts the new key (for example after the
owner reinstalls the machine).

## 7. Providers

```go
type Provider interface {
    Kind() string
    Capabilities(template string) Capabilities // OnDemand, CanStop, CanExtend, MaxLifetime
    Provision(ctx context.Context, req ProvisionRequest) (MachineRef, error)
    Resume(ctx context.Context, machine MachineRef) error
    Stop(ctx context.Context, machine MachineRef) error
    Extend(ctx context.Context, machine MachineRef, deadline time.Time) error
    Destroy(ctx context.Context, machine MachineRef) error
    List(ctx context.Context) ([]MachineRef, error) // only machines with this server's name prefix
    Access(ctx context.Context, machine MachineRef) (MachineAccess, error)
}

type ProvisionRequest struct {
    Name      string         // hostel-<server_id>-<machine id>; chosen before the call
    Template  string         // template name in the provider configuration
    Spec      map[string]any // the template's spec
    Deadline  time.Time
    ServerKey string         // server's SSH public key (authorized_keys format)
}

type MachineAccess struct {
    User    string
    Dial    func(ctx context.Context) (net.Conn, error) // stream to the machine's SSH server
    Signers []ssh.Signer                                 // keys the server uses
}
```

- `Dial` with a proxy command runs the command and uses its stdin and stdout as
  the connection. A proxy connection has no deadlines, so a timer bounds the SSH
  handshake.
- The server asks an external provider for the capabilities of each template at
  start; a failure stops the start with the provider's message.
- `machines.os` is the probed OS: it selects the helper and mise binaries. The
  configured `os` of the machine or template selects the tool scripts; the server writes it to
  `machine-env.json` (`"os"`), and the helper uses it for script names and
  `HOSTEL_MACHINE_OS`. For static machines both must be the same.
- `Signers` is the server key for `static`, `external`, and Namespace instance
  machines. For a Namespace
  devbox it is the key that the `devbox` CLI manages
  (`~/.namespace/ssh/<name>.devbox.namespace.key`): the devbox SSH server accepts
  only that key (M3, 2026-10-26).

| Kind | Entries | Provision | Access | Stop | Deadline |
|---|---|---|---|---|---|
| `static` | real hosts (`address`) | Not called. Each entry is a machine from server start. | TCP to the host with the server key, or the `machine connect` tunnel | no | none |
| `namespace`, `spec.product = "devbox"` | templates | `devbox create --name <Name> --ephemeral ...`; a devbox with the name that another call made (for example before a restart) is taken when it accepts SSH | proxy command `devbox ssh-proxy <Name>`, user `main_user` | no (ephemeral: a stop deletes it) | idle stop (it deletes the devbox); plan maximum duration (3 h on the Developer plan) |
| `namespace`, `spec.product = "instance"` (Linux) | templates | `nsc create --bare --machine_type ... --duration ... --ssh_key <file> --label hostel.name=<Name> --label hostel.machine=<template>`, then `nsc ssh <id> -- docker run ... <spec.image>`: the machine container (below). Idempotent: an instance with the label and an existing container are taken; the machine is ready when its SSH server accepts the server key | `nsc instance port-forward <id> --target_port 2222` for each connection; user `hostel`, server key | no | `--duration`; `nsc instance extend-duration --ensure_minimum` |
| `external` | templates | the configured command | from the command's response | if the command declares it | the command must apply it |

**Namespace instances** (M4, 2026-10-27). The SSH server of an instance (the
Namespace agent) refuses remote forwards, which tunnels need. So the machine is a
container on the instance: `docker run -d --name hostel-machine --restart
unless-stopped --network host --device /dev/kvm` with `spec.image` (default
`ubuntu:24.04`; an image with `apt-get`, or with OpenSSH already). Its start
script installs OpenSSH and `sudo` if they are missing, creates the user `hostel`
with the server key (and `sudo` without a password), lets it use `/dev/kvm`, and
runs `sshd` on port 2222. At the plan's concurrency limit, `nsc create` does not
fail: it blocks without output. So the create call has a 2 minute deadline, and a
timeout is "limit reached" (an instance that the call still makes later has the
prefix, and reconcile destroys it). Only `linux` machine types are supported.

Version 1 calls the `devbox` and `nsc` CLIs on the server. They must be installed
and logged in on the server machine. One `namespace` provider uses one login and
can mix devbox and instance machines.

**Machine OS.**

- `static`: the `os` key is required, so that `admin config check` and request
  validation work without a connection. At start, and at each reconcile while a
  machine is `broken` with reason "unreachable", the server probes the host (section 8, "Probe and upload"). An
  unreachable host is `broken` with reason "unreachable" and becomes `running` when
  a probe succeeds. A probe result that differs from `os` makes the machine `broken`.
- `namespace`: from `spec` (`platform` for a devbox, the prefix of `machine_type` for
  an instance).
- `external`: the `os` key is required. Tool checks and script selection use
  it. The helper binary follows the probe. (The E2E tests use this to run `.macos.sh`
  scripts in a Linux container.)

### Ownership and reconcile

- `server_id` must match `^[a-z0-9]{1,20}$` (no hyphen). Machine IDs are integers.
- Lease IDs match `^[a-z0-9]{10,26}$` (for example a lowercase ULID). Provider,
  machine, template, and tool names match `^[a-z][a-z0-9-]*$`; an option matches
  `^[a-z0-9][a-z0-9.-]*$`. No name contains
  `.`, so gateway user names (section 13) parse without ambiguity.
- Before a provider call, the server inserts the `machines` row in state `creating`
  with a name it chooses: `hostel-<server_id>-<machine id>`. It passes this name to
  the provider. Providers that cannot set a name use a label with the same value.
- **`Provision` is idempotent by name, also under concurrent calls.** After a
  server crash, an old provider process can still run when the restarted server calls
  `Provision` again. If a machine with that name exists or is in creation, the provider
  waits for it or returns it. A "name already exists" error from the cloud counts as
  success: the provider then returns the existing machine. It never creates a second
  machine with the same name. (The external fake provider uses a lock file for each
  name.)
- After a restart, the worker calls `Provision` again for each machine in `creating`.
- At start and every `intervals.reconcile` (default 5 minutes), reconcile calls
  `List` on each on-demand provider. It parses each name with
  `^hostel-([a-z0-9]+)-([0-9]+)$` and ignores names that do not match or that have
  another `server_id`. Then:
  - A listed machine with no database row, or with a `destroyed` row: destroy it.
  - A listed machine with the name of an active database machine but another provider
    ID (not in `creating`): destroy it. A Namespace instance has no unique name,
    so a create call of a killed server can make a second instance late
    (provider IDs are set only at creation).
  - A database machine in `creating`: skip it (its worker owns it).
  - Another database machine that is not listed: mark it `destroyed` (machine lost).
- A provider never acts on machines without the prefix. The owner uses Namespace for
  other work.

### Static machines

- At server start, each static machine gets a `machines` row (if it has none). A machine
  that is no longer in the configuration becomes `retired`; its active lease
  continues, and the row stays `retired` after that lease ends.
- Reconcile never destroys a static machine.
- Static machines install all their tools at server start, so the first lease is
  fast. On-demand machines install all the tools of their template when a lease first
  uses the machine (option 2 in section 5); the inventory then lists them all.
- A change of the configuration or of a tool directory applies at once (M9,
  `hostel config`): a free machine whose tools changed installs again.

### Deadlines

- Machines that cannot stop (`instance`):
  - At creation and at each assignment, the deadline is at least
    `now + creation timeout + install timeouts + start timeouts + max_lease +
    warm_running + 10 minutes`. The creation timeout is the provider's timeout for
    `provision` (external `timeouts.provision`; Namespace default 10 minutes). The
    script timeouts are those of the machine's tools (`install`) and of the
    lease's screen tools (`start`).
  - At `ready`, the server extends it to `ready_at + max_lease + warm_running + 10
    minutes` if that is later.
- Machines that can stop: the server destroys stopped machines after
  `warm_stopped`. If the server is down, stopped machines stay; reconcile destroys them
  after the restart if they are too old.
- Namespace devboxes are ephemeral (M3, 2026-10-26: a stopped macOS devbox keeps
  only its workspace volume, so a stop has no value). The provider-side bounds
  for compute cost (N2) are the idle stop (`--auto_stop_idle_timeout 15m`; it
  deletes an ephemeral devbox; measured: about 15 minutes after the last
  activity; 972 s for an idle ephemeral devbox) and the plan's maximum duration.
  While a job runs, its supervisor keeps the task marker
  `$NAMESPACE_DEVBOX_TASKS_DIR/hostel-<room>.<job>`, so a long job without an SSH
  connection does not stop the devbox. Only jobs have a marker: a devbox that a dead
  server leaked still stops on idle when its jobs end. Between commands, a lease
  without jobs expires (`idle_timeout`, 10 minutes) before the idle stop.
  `max_lifetime` is required for a devbox template.
- A machine takes a new lease only if its remaining provider lifetime (if its template
  has a `max_lifetime`, counted from the machine's creation) is at least
  `max_lease + 5 minutes`. Else the server destroys it
  when it has no lease, and a new machine is created.
- At config load, the server rejects a template whose `max_lease + 5 minutes` is longer
  than its `max_lifetime`. The owner sets `max_lifetime` from the provider's plan (for
  example 3 hours for a devbox on the Namespace Developer plan).

### External provider contract

```toml
[providers.docker]
kind = "external"
command = ["/path/to/provider", "--flag"]
command_env = { DOCKER_HOST = "unix:///var/run/docker.sock" }
timeouts = { provision = "5m", default = "1m" }

[providers.docker.templates.small]
os = "linux"
spec = { image = "debian:13", cpus = 2 }
tools = ["android-sdk", "android-emulator", "jdk:21"]
max_machines = 2
```

The server runs `command... <operation>`, writes one JSON object to stdin, and reads
one JSON object from stdout. Exit code 0 = success. A non-zero exit code = failure;
stdout then contains `{"error": "limit" | "not_found" | "other", "message": "..."}`.
stderr goes to the server log at debug level. Provider commands must not print
secrets.

| Operation | Request | Response |
|---|---|---|
| `capabilities` | `{"template"}` | `{"can_stop": bool, "can_extend": bool}`. External machines are always on demand. |
| `provision` | `{"name", "template", "spec", "deadline" (RFC 3339), "server_key"}` | `{"id", "user", "ssh": {"host", "port"}}` or `{"id", "user", "proxy_command": [...]}`. Idempotent by `name`. |
| `resume`, `stop` | `{"id"}` | `{}` |
| `destroy` | `{"id", "name"}`; `id` can be empty, then the provider finds the machine by `name` | `{}`, or the `not_found` error |
| `extend` | `{"id", "deadline"}` | `{}` |
| `list` | `{"prefix"}` | `{"machines": [{"id", "name", "state"}]}` |
| `access` | `{"id"}` | same as the `provision` response |

`provision` must install `server_key` in the user's `authorized_keys`. A provider
must refuse a request without a name: a server that stops during a call can leave
stdin empty or cut. The E2E tests use this contract with Docker
(`test/fakeprovider`).

## 8. Machines: layout, helper, and scripts

### Layout

Paths are relative to the machine user's home directory. On Windows, use the same
structure under `%USERPROFILE%`.

```
hostel/
  bin/hostel                 helper binary (hostel.exe on Windows)
  bin/mise                   mise binary (mise.exe on Windows; "mise on machines" below)
  mise/config.toml           the machine's mise tools (MISE_CONFIG_DIR)
  machine-env.json              provider, machine, and room env and the MISE_* variables (mode 0600)
  tools/<tool>/              the defined tools of this machine: tool.toml and the scripts for this machine's OS
  authorized_keys.base       authorized_keys at the first lease (restored at cleanup)
  cache/                     shared caches; kept between leases (HOSTEL_CACHE_DIR)
  rooms/<room>/
    work/                    the lease's files; a stable path for each room
    baseline.git/            git directory for the baseline commits
    lease.json               lease ID, requested tools, started screen tools with their options
    tool.env                 KEY=VALUE lines that the start scripts write (for example SIMULATOR_UDID)
    lease.env                KEY=VALUE lines that the guest sets (section 10)
    manifest.json            files of the last sync
    plans/<plan-id>.json     pending sync plans
    jobs/<job-id>/           output, exit, supervisor.json
```

The `work` path is the same for each lease in a room. Caches that use the project
path as a key (for example Xcode DerivedData) are therefore reused.

### Probe and upload

1. The server runs `uname -s -m` on the machine. If that fails, it runs
   `cmd /c echo windows %PROCESSOR_ARCHITECTURE% %USERPROFILE%` (the same in
   `cmd` and in PowerShell), and then `echo %OS%` to find the SSH server's default
   shell: `cmd` prints `Windows_NT`, PowerShell prints `%OS%`. The shell goes to
   `machines.shell`; the server and the gateway quote each command for it (package
   `shell`). Only the PowerShell default shell is tested (on a Windows VM); `cmd`
   is not (its quoting rules for `cmd /c` with several quoted words are a risk). A PowerShell command line ends with `exit $LASTEXITCODE`, because
   PowerShell itself exits with 0 or 1. The result selects
   `hostel-<goos>-<goarch>` and `mise-<goos>-<goarch>` from `helper_dir` (Go names:
   `darwin`, `linux`, `windows`; `amd64`, `arm64`; `.exe` on Windows). The Hostel OS
   names in configuration and script files are `macos`, `linux`, and `windows`;
   `darwin` maps to `macos`.
2. The server compares SHA-256 hashes (from the helper `info` command) and uploads
   what differs. The hash of a file is the SHA-256 of its bytes. The hash of a
   tool directory is the SHA-256 of a text with one line `<path> <mode>
   <sha256>\n` for each included file, sorted by path. Included files are
   `tool.toml` and the scripts for the machine's OS. Upload candidates: the helper, mise,
   `hostel/mise/config.toml`, `hostel/machine-env.json`, and the directory of each
   defined tool of the machine (one hash for each directory, which is also
   `machine_tools.scripts_sha256`).
3. The server repeats step 2 at each `preparing`, so that a guest cannot leave a
   changed helper or script for the next lease.
4. Transfer. The helper itself goes first, with `sh -c 'cat > tmp && chmod 755 tmp
   && mv tmp hostel/bin/hostel'`. On Windows, Windows PowerShell does not give its
   stdin to a script, so the helper goes as a `tar` stream to `tar.exe` (part of
   Windows since 10 1803), and PowerShell then moves it into place. Windows cannot
   replace a running executable, but it can rename it: the old file moves aside
   and a later install deletes it. All other items go through the helper `install`
   command, which writes a temporary file, checks the SHA-256, and renames it. mise
   goes compressed with gzip (151 MB → about 52 MB for Linux). A tool that the
   machine no longer lists is removed from `hostel/tools/`.

### Helper commands

All helper commands take `--room <name>` where it applies. Control commands read
one JSON object on stdin and write one JSON object on stdout. Exit code 0 =
success; else stdout has `{"error": "..."}`.

| Command | Caller | Input | Output |
|---|---|---|---|
| `info` | server | — | `{"os", "arch", "version", "hashes": {"helper", "mise", "mise_config", "machine_env", "tools": {"<name>": "<hash>"}}}` (empty string for a missing item) |
| `install mise\|mise-config\|machine-env --sha256 H` / `install tool <name> --sha256 H` | server | gzip stream (mise), bytes, or a `tar` stream (tool) | `{}`; refuses data with another hash |
| `usage` | server | — | `{"cpu_percent", "memory_used_bytes", "memory_total_bytes", "disk_free_bytes"}`, best effort (M10b): the CPU use over 1 s (`/proc/stat`; `ps` on macOS; `GetSystemTimes` on Windows) and the memory (`/proc/meminfo`; `vm_stat`; `GlobalMemoryStatusEx`). The server runs it for the machine details and for `GET /v1/usage`. |
| `hardware` | server | — | `{"cpu", "cores", "memory_bytes", "disk_free_bytes", "os_version"}`, best effort (M10). The server runs it after each provisioning and keeps it on the machine for the status view. |
| `mise-install` | server | `{"timeout_seconds"}` | `{"exit_code", "output_tail", "timed_out", "versions": {"<tool>": "<version>"}}`; installs the machine's mise tools of `hostel/mise/config.toml` and reports their versions (the inventory of the mise tools) |
| `begin` | server | `{"lease_id", "tools", "started"}` | `{}`. Writes `rooms/<room>/lease.json`. Saves the current `authorized_keys` as `authorized_keys.base` when no other room of the machine has a lease: the previous cleanup restored it, so it holds the owner's latest keys (for example a new server key). With a lease in another room, it keeps the base, because `authorized_keys` can then have a key that that lease added. |
| `run-script <tool> <step>` | server, guest (`screenshot`) | `{"args": [...], "env": {...}, "timeout_seconds"}` | `{"exit_code", "output_tail", "last_line", "missing", "timed_out"}`; `missing` when the tool has no script for this step and OS; `last_line` is the version that a `check` prints |
| `cleanup` | server | `{"lease_id", "started", "ports", "check"}` (from the database, so a repeated cleanup does not need `lease.json`; `started` are the screen tools of the lease with their options, stopped in reverse order; the server sets `check` at the time of the cleanup: the tools that the machine has installed) | `{"steps": [...]}` (section 17) |
| `set-env` | guest | `{"env": {"K": "V"}}` | `{}`. Writes `lease.env`. |
| `mise-current` | guest | — | `[{"name", "version"}]`: the tools that the mise files in the work directory select now (for the lease facts). |
| `sync-plan` | guest | manifest (section 12) | plan (section 12) |
| `sync-apply <plan-id>` | guest | a `tar` stream (not JSON) | `{"written", "deleted"}` |
| `diff` | guest | — | unified diff text (not JSON) |
| `tar-out <path>` / `tar-in <path>` | guest (`cp`) | — / `tar` stream | `tar` stream / `{}` |
| `job start` | guest | `{"command": [...], "shell": bool}` | `{"job_id"}` |
| `job attach <id> --offset N` | guest | — | the output bytes, then the exit trailer (section 15) |
| `job list` | guest | — | `{"jobs": [{"id", "command", "state", "exit_code", "started_at"}]}` |
| `job stop <id>` | guest | — | `{}` |
| `shell` | gateway | — | an interactive shell with the lease environment |
| `exec -- <command>` | gateway | — | runs a guest command with the lease environment |
| `connect` | static machine service | flags | runs until stopped (section 13) |

The `ready` lease data contains the absolute helper path, so the guest can call it.

### Tool directories

A defined tool is a directory (M10e; `m10e-implementation.md`). The binary
embeds the default tools (`android-sdk`, `android-emulator`, `xcode`,
`ios-simulator`, `macos-desktop`, `windows-desktop`, from `tools/` of the
repository); each default applies until an admin replaces it with their own
files or removes it. The tools are in the server's database
(`tool_revisions`). Admins add their own tools from a client:

```
hostel config tools                          # name, screen, machines, source (default, changed, own)
hostel config tools new tvos-sim ./tvos-sim  # a folder with a template (--os linux|macos|windows, --screen)
hostel config tools pull ios-simulator ./sim # the current files into a new folder
hostel config tools push ./tvos-sim          # check and save the folder (the name is the folder name)
hostel config tools reset ios-simulator      # a default back to the version of this release
hostel config tools remove tvos-sim          # refused while a machine lists it
```

A push sends only tool files (`tool.toml`, `GUIDE.md`, and scripts); the server
checks it with the whole configuration (the same rules as below) and applies it
at once. The server writes the effective tools to `data_dir/tools/<hash>/` and
uses them. A free machine whose tools changed installs again (for a template:
when a lease next uses it). mise tools have no directory (section 3).

```
tools/
  android-sdk/
    tool.toml   GUIDE.md
    install.linux.sh   check.linux.sh   install.macos.sh   check.macos.sh
  android-emulator/
    tool.toml   GUIDE.md
    install.linux.sh   start.linux.sh   screenshot.linux.sh   stop.linux.sh   check.linux.sh
    install.macos.sh   start.macos.sh   screenshot.macos.sh   stop.macos.sh   check.macos.sh
  xcode/
    tool.toml   GUIDE.md   check.macos.sh
  ios-simulator/
    tool.toml   GUIDE.md
    install.macos.sh   start.macos.sh   screenshot.macos.sh   stop.macos.sh   check.macos.sh
  macos-desktop/
    tool.toml   GUIDE.md   start.macos.sh   screenshot.macos.sh   stop.macos.sh
  windows-desktop/
    tool.toml   GUIDE.md   start.windows.ps1   screenshot.windows.ps1
```

`tool.toml`:

```toml
description = "Android emulator: one clean emulator for each lease"
needs = ["android-sdk"]                    # tools that this tool uses; a machine that lists it gets them too
screen = true                              # a screen tool: start, screenshot, and stop for each lease
options = ["pixel-9", "pixel-tablet"]      # screen tools: the phones and tablets; the first is the default
max_options = 2                            # options in one lease (default 1)
whole_machine = false                      # true: the lease takes all rooms of the machine
machine_user = false                       # true: needs the machine user's account; no lease user (M11)
env = { ANDROID_HOME = "{cache}/android-sdk" }        # in every lease on a machine with this tool
path = ["{cache}/android-sdk/platform-tools", "{cache}/android-sdk/emulator"]
script_timeouts = { install = "30m", start = "5m" }   # defaults: install 20m, others 3m
```

- `{cache}` is `$HOSTEL_CACHE_DIR`, `{home}` the machine user's home directory.
- `env` and `path` of a software tool apply to **every** lease on a machine that
  has the tool, also when the lease did not ask for it. The `env` and `path` of a
  screen tool apply when the lease started it. Each tool declares its own
  environment; Hostel adds nothing for one tool only.
- A machine that lists a tool gets the tools in its `needs` (they are added to
  its list; `hostel admin config check` shows the result).
- A software tool has no `options` key: its option is the version that `check`
  prints (or, for a mise tool, the installed version).

**Script files.** `<step>.<os>.sh` for `macos` and `linux`, `<step>.<os>.ps1` for
`windows`. The machine's OS selects the file. A `.sh` or `.ps1` file with another
name is a configuration error (it catches typing errors such as `strat.linux.sh`).
Other files in the directory are ignored and not uploaded. A tool is available
on an OS when its directory has at least one script for that OS, or when it has
no scripts at all (a tool that only sets `env` and `path`). Steps:

| Step | When | Example for `ios-simulator` |
|---|---|---|
| `install` | once for each machine, and again when the tool's files, the machine's options of a screen tool, or the machine `env` change; before it, the `install` of each tool in `needs` | create one golden simulator for each option |
| `check` | after `install`, after each cleanup, at repair, and before a warm machine takes a lease; the last line of the output is the tool's versions (a software tool: all that it has now, also what builds added to the cache) or its options (a screen tool), comma-separated; a non-zero exit code means that the tool does not work. A screen tool whose check reports none of the options that the machine lists fails too (no lease could start it) | Xcode works; the golden simulators exist; enough free disk |
| `start` | screen tools only: for each lease that asked for the tool, before `ready`, after the `start` of the tools in `needs`; writes facts to `tool.env` | clone the golden simulator of each requested option; boot it |
| `screenshot` | screen tools only: `hostel screenshot`; `$1` is the output path, `$2` the option (default: the first of the lease) | `xcrun simctl io "$udid" screenshot "$1"` |
| `stop` | screen tools only: cleanup of a lease that started the tool, in reverse order of `start` | stop the processes that use the room's clones (by UDID), then shut down and delete the clones |

Contract:

- Each step is optional, except `start` for screen tools: a screen tool must
  have `start.<os>` for each OS that it is available on.
- Each step must be idempotent. A restart can run it again.
- `install` and `check` run in `hostel/`. `start`, `screenshot`, and `stop` run in
  `rooms/<room>/`. The server creates `rooms/<room>/` before `start`.
- Environment: `HOSTEL_TOOL` (this tool), `HOSTEL_TOOL_OPTIONS` (the lease's
  options of this tool, comma-separated; `start`, `screenshot`, `stop`),
  `HOSTEL_TOOL_ALL_OPTIONS` (the options that the machine lists for this tool;
  all options in `tool.toml` when the machine lists it without one), `HOSTEL_TOOLS` (all
  tools of the lease, with their options), `HOSTEL_ROOM`, `HOSTEL_ROOM_DIR`,
  `HOSTEL_WORK_DIR`, `HOSTEL_CACHE_DIR`, `HOSTEL_LEASE_ID`, `HOSTEL_EXCLUSIVE`
  (`1` when the lease holds the whole machine, for example a desktop lease, else
  empty; `start`, `screenshot`, `stop`), the `MISE_*` variables, the `env` and
  `path` of the machine's software tools, the provider, machine, and room `env`,
  and `tool.env` (for `screenshot` and `stop`). Machine `env` is also the way to
  vary a script between machines with the same OS. Scripts get
  `HOSTEL_LEASE_ID`, not `HOSTEL_LEASE`: screen processes that `start` starts are
  therefore not stopped by cleanup step 1, and `stop` can shut them down cleanly.
- **Tool facts.** `start` writes them to `tool.env`. Each tool documents its
  variable names in its `GUIDE.md`. A `start` script may replace the file: the
  helper keeps the facts of the screen tools that started before it and
  reports the names that the script wrote (`run-script` output `facts`).
- Before the `install` scripts, the server runs `mise install` for the machine's
  mise tools (helper `mise-install`, at each machine provisioning), so scripts can
  use them: the helper adds `hostel/bin` and the output of `mise env --json` (in
  the script's directory) to the script environment.
- A non-zero exit code is a failure. The last 4 KB of output go to the event log.
- The helper runs a script with `/bin/sh <script>` (so the file needs no execute
  bit) in its own process group, and stops the group at the step timeout. A
  background process that the script starts (for example a simulator) can keep
  running after the script ends.

`hostel admin config check` (section 9) fails if a screen tool lacks `start` for
an OS that it is available on, if a software tool has `start`, `screenshot`, or
`stop` scripts, if a machine lists a tool that is neither a defined tool nor a
mise tool with its version, if a machine lists a defined software tool with an
option, or a screen tool with an option that the tool does not have, if a machine lists a tool that is not available on
its OS (for example `windows-desktop` on Linux), if a `needs` entry is not a
valid tool name or makes a loop (a need without a definition is a mise tool),
and on the old keys (`platforms`, `labels`, `requires`,
`devices`).

**`GUIDE.md`.** A short text for agents: what the tool gives, its environment
variables, and how to use it (build with it; start, see, and drive a screen).
`hostel tools <name>` and `hostel agent-guide` show it (section 10). The agent
guide shows only the guides of the tools that the server has.

**The default tools.**

- **`android-sdk`** (software; `needs = ["jdk"]`; Linux and macOS). `install`
  installs the SDK in `$HOSTEL_CACHE_DIR/android-sdk` (Java from the `jdk` tool)
  and, on Linux, the Ubuntu packages that the emulator needs (with `sudo`).
  `check` prints the SDK platform version. `env`: `ANDROID_HOME`; `path`:
  `platform-tools` (`adb`), `cmdline-tools/latest/bin` (`sdkmanager`), and
  `emulator`. The machine `env` value `HOSTEL_ANDROID_API` (default 36) selects
  the SDK platform and the system image.
- **`android-emulator`** (screen; `needs = ["android-sdk"]`; options `pixel-9`,
  `pixel-tablet`; `max_options = 2`). `install` creates one golden AVD
  `hostel-golden-<option>-<API>-<tag>` for each option with a `clean` snapshot.
  `start` starts each lease option from that snapshot with `-read-only` (the AVD
  does not change, so each lease gets a clean emulator) on the console port
  `HOSTEL_ANDROID_PORT` (room or machine `env`, default 5554) + 2 × option index; a
  machine with several rooms sets it for each room. It writes `ANDROID_SERIAL`
  (the first option) and `ANDROID_SERIALS` (all, comma-separated). `stop` stops
  the emulators on the room's ports. The machine `env` value `HOSTEL_ANDROID_TAG`
  (default `google_apis`) selects the system image. Linux machines need `/dev/kvm`.
- **`xcode`** (software; macOS). `check` prints the Xcode version (`xcodebuild
  -version`). It installs nothing: Xcode comes with the machine image.
- **`ios-simulator`** (screen; `needs = ["xcode"]`; options `iphone-17-pro`,
  `ipad-pro-13`; `max_options = 2`). It acts only on simulators with the name
  prefix `hostel-`: the golden simulators `hostel-golden-<option>-<iOS version>`
  (from `install`) and the lease's simulators `hostel-<room>-<option>`. `start`
  writes `SIMULATOR_UDID` (the first option) and `SIMULATOR_UDIDS` (all,
  comma-separated). The machine `env` value `HOSTEL_IOS_RUNTIME` (for example
  `26.5`) selects the runtime; the default is the newest iOS runtime.
- **`macos-desktop`** (screen; macOS; `whole_machine = true`): section 9, "The
  Mac desktop".
- **`windows-desktop`** (screen; Windows; `whole_machine = true`): section 9,
  "The Windows desktop". Its `start` fails when the desktop program does not
  run (no `hostel/desktop.json`). It has no `stop`: the desktop program ends
  the lease's commands at cleanup.

## 9. Configuration files

All configuration uses TOML: the same format as `mise.toml`. The server decodes
strictly: an unknown key or a wrong type is an error with the file name and line.
Durations are strings with a unit (`"10m"`); a number is a type error. In
`server.toml`, a path that starts with `~/` is relative to the home directory, and
another relative path is relative to the directory of `server.toml`.

### Server (`server.toml`)

`hostel server init` asks for the server name, the public addresses, and the
owner's name (or takes `--server-id`, `--url`, `--user`, `--listen`, `--tls`), and
writes a commented template with `tls = "self-signed"`, a gateway host key, the
server's own SSH key pair (`data_dir/server_ed25519`), and the TLS certificate.
When the server has no users, it makes the first admin
invite and prints `hostel connect <code>` (M8). `hostel server invite [--role
admin|member] [--name n]` makes an invite on the server machine without the API
(for example when nobody can sign in as admin). `hostel admin pubkey` prints the
server's public key.

**Two parts (M9).** `server.toml` has only the keys that the server needs on
its machine before it opens its database: `server_id`, `listen`,
`gateway_listen`, `gateway_public`, `tls`, `public_urls`, `data_dir`,
`helper_dir`. Everything else (the providers with their
machines or templates, and the settings `idle_timeout`, `poll_timeout`, `limit_retry`,
`port_process_offline`, `intervals`, `mise`) is the **configuration document**:
TOML with the same keys, kept in the database with a revision for each change
(`config_revisions`). The server validates the two parts together; a key in
the wrong part is an error with its key path. Errors in the document name its
revision ("configuration revision 4: providers.home.machines.box.os: ...").

- **No old keys.** The keys of the configuration before M10e (`platforms`,
  `labels`, `requires`, a `tools` map) fail the check with the new form in the
  message. There is no import or migration (owner decision, 2026-11-05).
  Exception: `platforms_dir` in `server.toml` (`install.sh --server` wrote it
  before M10e). The server ignores it and logs a warning, and `hostel admin
  config check` prints the warning, so the update does not stop the server
  (owner decision, 2026-11-05).
- **Changes.** An admin changes the document from a client: `hostel config
  edit` (in `$EDITOR`), `hostel config apply <file>`, or the targeted commands:
  `hostel config settings set <key>=<value>`; `hostel config machines` (the
  machines and templates, each with its running machines), `machines set <machine>
  <key>=<value>` (`tools` as a comma list), `machines remove <machine>`, `machines
  drain|undrain|repair|destroy <machine>` (the same as `hostel admin machine`, by
  name; `<machine>` is also a template key, when the template has one machine);
  `hostel config providers` (list), `providers add <name> --kind <kind>`,
  `providers remove <name>`. The client sends the whole
  document with its base revision; the server refuses it if another revision
  came in the meantime (HTTP 409) or if it is invalid (HTTP 422 with each key
  path). The targeted commands change the text in place, so comments and the
  machine order (the scheduler's order) stay.
- **Apply.** A valid revision applies at once, without a restart: the server
  builds the providers again (only the changed ones: a Namespace provider
  checks its login, an external provider loads its capabilities), hashes the
  tools, adds rows for new static machines, and wakes the scheduler. A machine
  records the hash of its settings, its tools, and the `mise` settings
  when it is provisioned. When it is free and the hash changed, a static machine
  provisions again and an on-demand machine is destroyed. A static
  machine with an active lease cannot be removed (drain it, and remove it when
  the lease ends); a template cannot be removed while it has machines
  (destroy them first: `hostel config machines destroy <template>`); a provider
  cannot be removed while it has on-demand machines. A removed static
  machine is retired.
  The local keys still need a restart.
- **Machines that join.** `hostel config machines add <name> [--tool t]...
  [--provider p]` makes a machine invite (24 hours, once) and prints
  one command for macOS and Linux (`curl ... /v1/install.sh | sudo sh -s --
  <code>`) and one for Windows (`curl.exe ... /v1/download/hostel-windows-<arch>.exe`,
  then `hostel.exe machine join <code>`). With TLS `self-signed`, curl pins the
  server's key (`--pinnedpubkey`). `hostel machine join <code>`, as an
  administrator on the machine: (1) creates the user `hostel` without
  administrator rights if it is missing; (2) sends the OS, architecture, and
  host name with the code (`POST /v1/machines/join`, no token, the rate limit
  of `connect`); the server adds the machine to the document (`connect = true`,
  in the static provider `home` unless the invite names another, which it adds
  if it is missing) and returns a machine token and its SSH public key; (3) puts
  the key into `authorized_keys` and `hostel/authorized_keys.base` of `hostel`;
  (4) writes the token, the result of the join (`join.json`: a second run
  after a failure continues with it), and a copy of the binary into a
  directory that only administrators can write (`/etc/hostel`, or
  `C:\\ProgramData\\hostel` with its owner and rules set by security IDs), and
  installs the service that runs `hostel machine connect` from there (Linux:
  systemd unit as `hostel-connect`, which owns the token; macOS: LaunchDaemon as
  root, which is not the machine user; Windows: scheduled task as `LOCAL SERVICE`
  that runs `machine-connect.cmd`); (5) prints the steps that only a person can
  do. The server key goes into the machine user's files as that user (a lease can
  leave links there). The server refuses a join for a machine that exists, and
  revokes the old machine tokens of the name; removing a machine revokes its machine
  tokens. `--no-service` skips the service. `hostel machine connect` is the
  former `hostel helper connect` (still accepted).
- **Provider login.** `hostel config providers login namespace [--cli
  devbox|nsc]`: the server runs `<cli> login --browser=false` (deadline 10
  minutes, one at a time for each CLI) and gives the client the address that it
  prints; the client prints it and waits until the login ends. The credentials
  stay on the server. A logged-in CLI is reported, and no login runs.
- **Reading.** Members and admins read the document (`hostel config`,
  `hostel config settings`, `hostel config history`); for members, the values
  of `env`, `command_env`, and `github_token` are `***`.

The document:

```toml

[providers.home]
kind = "static"
label = "Home"                      # optional: the name that people see; default: the key

[providers.home.machines.macbook]
address = "hostel@macbook.local"    # user@host[:port]; default user hostel, port 22
os = "macos"                        # required for static machines
tools = ["xcode", "ios-simulator", "android-emulator", "jdk:21", "macos-desktop"]   # android-sdk comes with android-emulator

[providers.namespace]
kind = "namespace"
max_machines = 3                       # limit for all its machines
warm_running = "5m"                 # default for its templates
# limits = [{ templates = ["mac"], max_machines = 1 }]   # for example "1 devbox per user"

[providers.namespace.templates.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
tools = ["xcode", "ios-simulator", "macos-desktop"]
# tahoe-slim has several Xcode versions; select one (the xcode check then reports 26.5).
env = { DEVELOPER_DIR = "/Applications/Xcode_26.5.app/Contents/Developer", HOSTEL_IOS_RUNTIME = "26.5" }
max_machines = 1
max_lifetime = "3h"                 # required for a devbox: the plan's maximum duration

[providers.namespace.templates.linux]
# image: the machine container (default ubuntu:24.04)
spec = { product = "instance", machine_type = "linux/amd64:8x16" }
tools = ["android-sdk", "android-emulator", "jdk:21", "node:22"]   # jdk and node are mise tools
max_machines = 2
```

Server-level keys and defaults:

| Key | Default |
|---|---|
| `listen` | `127.0.0.1:7410` (HTTP API) |
| `gateway_listen` / `gateway_public` | `127.0.0.1:7422` / same as `gateway_listen`, or `/v1/gateway` with `tls = "self-signed"` (the client adds the server address that it uses and its pinned key); `gateway_public` can also be a `wss://<host>/v1/gateway` URL (section 13) |
| `data_dir` | `~/.local/share/hostel` |
| `helper_dir` | `<data_dir>/helpers` (`hostel-<goos>-<goarch>` and `mise-<goos>-<goarch>`, section 8) |
| `idle_timeout`, `poll_timeout`, `limit_retry` | `10m`, `5m`, `2m` (`aging` was removed in M8; the key is an error) |
| `tls` | `off`; `hostel server init` writes `self-signed` (the server's own certificate in `data_dir/tls_cert.pem`, `tls_key.pem`; clients pin its key) |
| `public_urls` | `[]`: the addresses that clients use, in invite codes; port 0 becomes the listen port; empty: the listen address. Each is `https://` (also with `tls = "off"`: the proxy has TLS); `http://` only on loopback with `tls = "off"`, because clients send the invite secret and their token to it |
| `port_process_offline` | `10m` (section 14) |
| `intervals` | `{ scheduler = "10s", reconcile = "5m", tunnel_check = "30s" }` (short values in tests) |
| `[mise]` | `idiomatic_tools = ["node", "python"]` (also when `[mise]` sets only other keys; `[]` turns idiomatic files off); `github_token` unset; `agent_device = true` |

Provider, machine, and template keys. A machine or template inherits only these provider keys: `tools`,
`env`, `max_lease`, `max_lifetime`, `warm_running`, `warm_stopped`, `reuse`. `max_machines` is
not inherited: on a provider it is a limit for all machines together.

| Key | Where | Default |
|---|---|---|
| `kind` | provider | required: `static`, `namespace`, `external` |
| `label` | provider | the provider's key; the name that people see (a heading in Rooms) |
| `max_machines` | provider: limit for all its machines; template: its own limit | provider: none; template: 1 |
| `limits` | provider | `[]`; each entry `{ templates = [...], max_machines = N }` |
| `max_lifetime` | provider, machine, template | none (the provider's maximum machine lifetime) |
| `address`, `connect` | static machine | `connect = false` (true: the host is behind NAT; section 13) |
| `os` | machine, template | required for `static` and `external`; from `spec` for `namespace` |
| `spec` | template | required |
| `tools` | provider, machine, template | `[]` (no tools; the machine still takes leases that ask for no tools). Entries: a defined tool (`android-emulator`, `android-emulator:pixel-9` to give only that option), or a mise tool with its version (`jdk:21`, `node:22`; several versions of one tool are allowed) |
| `rooms` | machine, template | one room `a`; room `n` (from 0) gets ports `7500 + 10n` to `7509 + 10n` |
| `env` | provider, machine, template, room | `{}` |
| `max_lease` | provider, machine, template | `2h` |
| `warm_running`, `warm_stopped` | provider, template | `5m`, `24h` (on-demand only) |
| `reuse` | provider, template | `true` (on-demand only); `false`: one lease for each machine (M11) |
| `lease_users` | machine, template | `false`; `true`: an OS user for each lease (macOS and Linux, M11, section 18) |
| `command`, `command_env`, `timeouts` | external provider | `command` required; `timeouts = { provision = "5m", default = "1m" }` |

Explicit rooms: `rooms = [{ name = "a", ports = [7500, 7501], env = { ANDROID_SERIAL = "emulator-5554" } }]`.

**Room users** (static machines, M6). A room can have its own OS `user`
(`rooms = [{ name = "a", user = "hostel-a" }, { name = "b", user = "hostel-b" }]`),
so two leases on one machine cannot read each other's files or stop each other's
processes. The user needs the server key in its `authorized_keys`, and a home
directory that other users cannot read. The server connects as the machine user
(`address`) for the machine-level steps (probe, upload, `install`, `check`) and as
the room user for the room steps (begin, `start`, cleanup) and for the guest's
connections. At each machine provisioning, it probes each room user (its home goes to
`rooms.home`), uploads its own `hostel/` directory, and installs the machine's
mise tools for it. After the cleanup of such a room, the server runs the `check`
scripts as the machine user. A screen tool whose `start` needs the result of
`install` in the machine user's account (for example the iOS golden simulators,
which belong to the user that made them) does not work with room users; such
machines keep one room or use rooms without users.

**Commands for the owner:**

- The server reads `~/.config/hostel/server.toml` by default; `--config` selects
  another file.
- `hostel admin config check [--config server.toml]` validates `server.toml` and all
  tool directories, and prints the effective configuration with all defaults (with
  the tools that `needs` adds to each machine).
  The server runs the same check at start and refuses to start on an error.
- Error positions: TOML syntax errors, type errors, and unknown keys give the file,
  line, and column (from the decoder). Semantic errors (for example an unknown
  tool) give the file and the key path, for example
  `server.toml: providers.home.machines.macbook.tools[1]: unknown tool
  "andriod-sdk"`. A missing script names the tool directory and the missing file.
- `hostel admin config schema` writes `hostel.schema.json` (JSON Schema from the Go
  types). Editors use it through the `#:schema` line for completion and checks.
  Generate it with a small reflection over the configuration structs. If that code
  grows beyond about 200 lines, `github.com/invopop/jsonschema` is an allowed
  dependency.

### Repository (`hostel.toml`)

The repository declares what it needs (owner decision, 2026-11-05). The file goes
to the machine with the other files. The settings for every lease are on the top
level; `[tool.<name>]` holds only what changes when the lease has that tool.

```toml
tools = ["android-sdk", "jdk:21"]      # what "hostel acquire" asks for when no tools are given
include = [".env"]                     # extra files that git ignores; glob patterns
setup = ["./gradlew --version"]        # after mise install, in every lease
env = { APP_ENV = "test" }             # in every lease

[tunnels.api]                          # machine → guest (section 14)
local_port_env = "PORT"

[tool.android-emulator]                # only when the lease has this tool
env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }

[tool.ios-simulator]
setup = ["tuist install && tuist generate --no-open"]
env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }
```

- Tools on the command line replace `tools`.
- A `[tool.<name>]` applies when the lease has that tool: the lease asked for
  it, or a tool that the lease asked for needs it, also a mise tool (owner
  decision, 2026-11-05; `[tool.android-sdk]` and `[tool.jdk]` apply to a lease
  for `android-emulator`). It does not
  apply to every lease on a machine that has the tool. Each section applies
  once, in the order of needs (the server gives the list: `lease_tools`,
  section 11).
- The `setup` of each `[tool.<name>]` that applies runs after the top-level
  `setup`; its `env` replaces keys of the top-level `env`.
- The keys of the form before M10e (`[platforms.<name>]`, `devices`, `requires`)
  fail the check with the new form in the message.
- The setup job always starts with `mise install` in `work/` ("mise on machines" below).
  Then the `setup` commands run in order, with `sh -c` (or PowerShell on Windows):
  the setup job is `mise install && PATH="$(mise bin-paths | tr '\n' ':')$PATH" &&
  (command 1) && (command 2)`. The `PATH` step is necessary because the job
  environment comes from `mise env` at the job start, before this `mise install`
  (found in M2). The first failure stops the job.
- `hostel admin config check --repo hostel.toml` validates a repository file.

**Caches.** mise installs, package stores, DerivedData, and Gradle caches are outside
`rooms/<room>/work`. Cleanup does not delete them: all files outside `rooms/<room>/`
(`hostel/cache`, `~/.gradle`, and the rest of the machine user's home directory) stay for
the next lease, also of another user. The agent guide says so: no secrets there.

### The Mac desktop (`macos-desktop`)

A Mac app (also a Mac Catalyst app) runs on the macOS desktop, not in a
simulator. The screen tool `macos-desktop` gives it. Rules:

- The guest asks for the desktop for each lease that needs it:
  `hostel acquire xcode macos-desktop`. A repository puts `macos-desktop` in its
  `tools` only when each of its leases needs the desktop, because each such lease
  takes the whole machine.
- The owner lists `macos-desktop` only for machines with a logged-in graphical
  session: a static Mac with automatic login, or a Namespace devbox (it has a
  desktop).
- `macos-desktop` has `whole_machine = true` (section 5): two leases must not
  share one desktop. A lease on the same Mac without it (for example a build
  with `xcode`) is not exclusive.
- `tools/macos-desktop/screenshot.macos.sh` uses `screencapture` on the machine. Over SSH,
  macOS can refuse this without the Screen Recording permission. On the static Mac
  it needs the permission for `sshd-keygen-wrapper` (M2); on a devbox it works
  without a prompt (M3, 2026-10-26).
- **Control (M7).** The guest controls desktop apps with `agent-device
  --platform macos` (an XCTest runner; Automation Mode without a password
  prompt). It starts its own app with `open <path to .app>` and then `agent-device
  open <bundle id>`: `agent-device open` with a path failed (2026-10-29). The
  first command of a lease builds and starts the runner (about 30 s). A command
  can fail while the runner still drains an earlier one that hit its 30 s
  watchdog; a retry a few seconds later works.
- **Desktop cleanup (M7).** Apps that a guest starts with `open` are children of
  launchd, without `HOSTEL_LEASE` and outside the lease's sessions, so the process
  cleanup (section 17) does not find them. Agents that an app registers
  (`SMAppService`, `launchctl bootstrap`) are jobs of the user's launchd domain.
  A desktop lease holds the whole machine, so all of these belong to it. For a
  desktop lease (`macos-desktop`), its `start.macos.sh` records the apps of all
  types (`lsappinfo`; PID, start time, type, bundle ID, executable), the jobs of
  `gui/<uid>`, and the entries of `~/Applications` and `~/Library/LaunchAgents`.
  Its `stop.macos.sh` (M10h) first runs `launchctl bootout` on each new job that
  is not `com.apple.*` and deletes the new plists in `~/Library/LaunchAgents`.
  Then it ends each app that was not running at the start (SIGKILL: a quit can
  wait at a save dialog): a foreground app, except Finder and the Dock, or a
  background app (UI element or background only) that is not `com.apple.*` and
  not under `/System`, `/usr`, `/Library/Apple`, `/bin`, or `/sbin` (macOS starts
  its own agents at any time). It deletes their saved window state, ends the
  `agent-device` runner, and deletes the new entries of `~/Applications`. Jobs
  and apps from before the lease stay. Tested on a real Mac (2026-11-12): a
  test launch agent, a background app, and a new foreground app ended; the
  agents from before stayed. A lease without `macos-desktop` does not get this
  cleanup: apps that it opens stay (found on a real Mac: windows from earlier
  leases still ran days later). A lease without `macos-desktop`
  does not touch the desktop. The machine needs an unlocked desktop session: a locked screen
  gives the lock screen to `screencapture` and blocks the runner (found on the
  static MacBook, 2026-10-29).

### The Windows desktop (`windows-desktop`)

- The guest asks for it with `hostel acquire windows-desktop` (with `dotnet` or
  other tools to build; `whole_machine = true`, as on the Mac). The owner lists
  `windows-desktop` only for a machine whose desktop user is the machine user
  (automatic sign-in) and that has the "Hostel desktop" task (`machine-setup.md`,
  section 4.4).
- **The desktop program.** SSH sessions (Windows session 0) cannot show
  windows, take screenshots, or use UI Automation. The task of the machine user
  starts `hostel helper desktop serve` at its sign-in, in its desktop session.
  (Until M10h, the desktop user was another user, and its task started the
  program with `Start-Process -Credential`; packaged apps cannot start that way.)
  It listens on `127.0.0.1` with a random port; `hostel/desktop.json`
  (only the machine user can read it) has the address and a random token. It ends
  when the helper file changes, and the task starts the new helper.
- **`desktop [--detach] <command>`** (`hostel/bin/desktop`, `hostel helper
  desktop run`) sends a command with the lease environment and the current directory.
  The program accepts only the lease of the room (the caller's `HOSTEL_LEASE`
  must match `lease.json`) and only a lease that started `windows-desktop`. It sets
  `HOSTEL_LEASE` and `HOSTEL_ROOM` from `lease.json` (not from the request), runs the command in its own job object
  (kill on close, no breakaway; on macOS and Linux a process group), and returns
  the output and exit code. With `--detach`, it returns the PID at once. If the
  caller ends, the command ends. A command and its processes stay until the
  lease ends: within 2 s after cleanup starts (it writes the room's
  `lease-ended` marker first) or the room's lease changes, the program ends the
  job object. A command's output ends 2 s after it exits, also when a child
  keeps it open. The program removes `desktop.json` when it ends, and it waits
  for a new helper until no lease command runs. Cleanup (section 17) finds the processes by `HOSTEL_LEASE` before
  that. Without the program, `desktop` exits with 69.
- **Control.** Windows machines with `agent_device` get the pinned `winapp` CLI
  (Microsoft, `config.WinAppVersion`, SHA-256 checked) in
  `hostel/winapp-<version>` (not under `hostel/tools/`, which holds only tool
  directories). `hostel/bin/winapp.cmd` runs it through
  `desktop`. `winapp ui` reads the UI Automation tree and drives apps with
  patterns (`invoke`, `set-value`) and with real input (`click`, `send-keys
  --via send-input`), which it sends only when the app is the foreground
  window.
- **Screenshot.** `tools/windows-desktop/screenshot.windows.ps1` captures the
  whole desktop through `desktop`.
- **Limits.** The desktop user must be the machine user: a packaged app (MSIX)
  starts only for the user of the desktop session (with another user, activation
  fails with `0x80070520`, "A specified logon session does not exist"; M10h test
  flight, 2026-11-12). Windows 11 has one desktop session: a Remote Desktop
  connection of another user disconnects it, and a disconnected or locked
  desktop stops real input and screenshots.
- **Desktop cleanup (M10h).** Programs that the shell starts for a lease
  (`explorer.exe <file>`, packaged apps) are not children of the lease and do
  not have `HOSTEL_LEASE`. `start.windows.ps1` records the processes of the
  desktop session and the packages of the machine user (`Get-AppxPackage`).
  `stop.windows.ps1` ends the new processes of the session (not `hostel.exe`
  and `explorer.exe`, and not programs in the Windows folder, which Windows
  starts at any time), and removes the packages that the lease installed, with
  their app data. Install packages through `desktop`: in a job (a network logon),
  `Add-AppxPackage` fails with `0x80070005` when the machine user has a desktop
  session (M10h). Other files
  and settings of the machine user stay for the next lease, as on macOS.

### mise on machines

Hostel uses [mise](https://mise.jdx.dev) to install the package managers,
compilers, and runtimes on machines. Machines do not need mise before Hostel uses them,
and Hostel does not use another mise that the machine has.

- **Binary.** The server uploads a pinned mise binary to `hostel/bin/mise`. The pin
  is `MISE_VERSION` in the `Makefile`. `make dist` downloads the pinned version for each OS and architecture into `dist/`
  (point `helper_dir` at it) and verifies the release checksums. The pin is in the
  Hostel repository, so a Hostel release fixes the mise version.
- **Two levels of tools.**
  - Machine level: the mise tools in the machine's `tools` (`jdk:21`,
    `node:22`; section 3). The server writes them to `hostel/mise/config.toml`,
    installs them when it provisions the machine, and records their versions in the
    inventory. They are tools as the defined tools are: a guest can ask for
    them (`hostel acquire node:22`).
  - Repository level: the repository's own `mise.toml`, `.mise.toml`, or
    `.tool-versions`, and idiomatic files (`.nvmrc`, `.python-version`). The setup
    job runs `mise install` in `work/` first. Repository versions override machine
    versions (normal mise precedence). These are not machine tools: they work on
    every machine, and a guest does not ask for them.
- **What mise does not install.** Xcode, simulator runtimes, Android system images,
  and other heavy toolchains come from the machine image or a defined tool's
  `install` script (section 8).
- **Environment.** All machine-side processes of Hostel (scripts, jobs, shells) get:

  | Variable | Value |
  |---|---|
  | `MISE_DATA_DIR` | `hostel/cache/mise` (installed tools; kept between leases) |
  | `MISE_CACHE_DIR` | `hostel/cache/mise-cache` |
  | `MISE_CONFIG_DIR` | `hostel/mise` |
  | `MISE_STATE_DIR` | `hostel/mise-state` |
  | `MISE_TRUSTED_CONFIG_PATHS` | `hostel/rooms` (the repository config is trusted without a prompt) |
  | `MISE_YES` | `1` |
  | `MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS` | from `mise.idiomatic_tools` |
  | `MISE_GITHUB_TOKEN` | from `mise.github_token`, if set (avoids GitHub rate limits; a secret, never logged) |

- **Tool paths in commands.** At each job or shell start, the helper runs
  `mise env --json` in `work/` and merges the result into the environment (section
  10). Commands therefore find the right `node`, `java`, or `tuist` without
  `mise activate`.
- **Sync.** `hostel sync` without `--setup` runs `mise install` again only when a
  mise configuration file changed.
- **Opt-out.** `mise = false` in `hostel.toml` skips the repository-level steps.
- **agent-device.** Each machine gets [agent-device](https://github.com/callstack/agent-device)
  (owner decision, 2026-10-23): agents use it to tap, type, and read the UI of
  screen tools on the machine (simulators, emulators, the Mac desktop). Hostel pins its version and the Node version that runs it
  (`internal/config/agentdevice.go`; T3 Code pins the same version). At
  provisioning, `mise-install` installs both and writes `hostel/bin/agent-device`
  (`agent-device.cmd` on Windows), which runs `mise exec node@<pin>
  npm:agent-device@<pin> -- agent-device`. The job `PATH` therefore gets no Node, and
  a repository's Node pin does not change it. Jobs and shells get
  `AGENT_DEVICE_STATE_DIR=rooms/<room>/agent-device`: each room has its own daemon
  and sessions, and cleanup deletes them with the room. The iOS runner build stays
  in `~/.agent-device/apple-runner` and is reused by later leases. The server
  option `mise.agent_device = false` turns it off.

### Guest (`~/.config/hostel/config.toml`)

`hostel connect` writes it (mode 0600):

```toml
server_id = "acme"
addresses = ["https://100.80.1.2:7410"]
key = "sha256:q3J1...e8Q"      # the pinned TLS key; absent for a server behind a TLS proxy
token = "..."
```

The form before M8 (`server = "..."`, `token = "..."`) still works.

`HOSTEL_SERVER`, `HOSTEL_TOKEN`, and `HOSTEL_SERVER_KEY` override the file. Without
a connection, commands that need the server exit with 78 and print how to connect.

## 10. Guest flow

### Local state

- Directory: `$(git rev-parse --git-dir)/hostel/lease/` (the per-worktree git
  directory). It holds the lease ID, the request, the private key, the pinned
  gateway host key, the port process PID and control socket, and the step that
  `acquire` finished last.
- **One lease per worktree** (owner decision, 2026-11-05). Lease commands
  (`run`, `sync`, `screenshot`, `release`, …) always use the lease of the
  worktree; no command has a lease selector. `acquire` in a worktree with an
  active lease and other tools fails at once: "this worktree has the lease
  <id>; release it first, or use another worktree". One lease can have several
  tools (build tools and an emulator together); work on two machines at the same
  time uses a second worktree (`git worktree add`). The server knows each lease
  by its ID; this rule is only local.

### `hostel acquire [<tool[:option]>...] --task "..."`

The command is resumable. Each step records its result in the local state.

1. If the state has a lease that is not final, continue it. If tools on the
   command line differ from the lease's tools, fail at once (one lease per
   worktree, "Local state" above). If `--task`, `--priority`, or `--fresh`
   differ, print a warning and continue the existing lease (release it first
   to change them). If the stored lease is final, start a new one.
2. Read `hostel.toml`. The tools are the arguments, else `tools` of
   `hostel.toml`, else none (any machine). Generate an ed25519 key pair for the
   lease.
3. `POST /v1/leases` with the tools, task, priority, tunnels (names and fixed
   machine ports), `fresh`, public key, repository name, worktree path, and host
   name. A request that no configured machine can serve fails here (exit code 69,
   with the reason).
4. Long-poll `GET /v1/leases/{id}?since=<revision>&wait=50s`. Print one line for
   each change: state, position, "waiting behind N leases", reason, estimated
   wait.
5. When the lease is `ready`: compute the environment (below) and call `set-env`.
6. Start the port process and wait until all tunnels are confirmed (section 14).
7. Sync the files (section 12).
8. Start the setup job and attach to it.
9. Print the **lease facts** and the next commands: the machine (provider and
   name), its OS with version and architecture, its hardware, all tools of the
   machine with their options and environment names (also the ones that the lease
   did not ask for), the screen tools that started for the lease with their
   options and facts (`ANDROID_SERIAL`, `SIMULATOR_UDID`), the tools that the
   repository's mise files select now (they come first in jobs), the work
   directory, the tunnels with what is missing on the guest side, and the expiry:

   ```
   lease l-4f2a is ready on ns/linux
     machine  linux 24.04 (amd64), 8 cores, 16 GB
     tools    android-sdk 36 (ANDROID_HOME; adb on PATH) · jdk 21 · docker 27.3
     started  —   (no screen tools; add android-emulator to get one)
   ```

If `--wait` (default 100 seconds) ends before step 9, the command prints
`hostel: not finished; run the same command again` and exits with code 75.

### Environment of lease commands

The helper builds the environment of each job and shell in this order (a later
source overrides an earlier one):

1. the machine user's login environment;
2. the provider, machine, and room `env`, and the `MISE_*` variables (section 9);
3. the output of `mise env --json` in `work/` (tool paths and the repository's
   mise `[env]`);
4. the `env` and `path` of the machine's software tools, and of the screen tools
   that started for the lease (section 8);
5. `tool.env` (from the `start` scripts);
6. `lease.env`: the repository `env` and the `env` of its `[tool.<name>]`
   sections for the lease's tools and their needs, after placeholder
   substitution by the guest,
   plus `HOSTEL_TUNNEL_<NAME>_PORT` for each tunnel;
7. `HOSTEL_LEASE`, `HOSTEL_TOOLS` (the lease's tools with their options),
   `HOSTEL_ROOM`,
   `HOSTEL_WORK_DIR`, `HOSTEL_CACHE_DIR`, `HOSTEL_MACHINE_OS`, `HOSTEL_MACHINE_ARCH`, and
   `AGENT_DEVICE_STATE_DIR` (section 9, "mise on machines"). The helper reads the lease
   data from `rooms/<room>/lease.json`.

`{tunnels.<name>.port}` is the machine port of the tunnel (section 14). Tunnels without
a fixed `machine_port` get ports from the room's pool in name order.

### Guest commands

| Command | Effect |
|---|---|
| `hostel tools [<tool>] [--json]` | Each tool: its options (from the inventories), whether it is a screen tool, the machines that have it, free rooms, waiting leases, and the estimated wait (R1); a last line names the repository's mise tools, which work on every machine. With a name: the description, the needs, the environment names, the machines with their options, OS, and size (from the newest machine of the template, else from its configuration), and the tool's `GUIDE.md`. |
| `hostel acquire [<tool[:option]>...] --task ... [--priority low\|normal\|high] [--fresh] [--wait 100s] [--background]` | Creates or continues a lease. The lease ID is in the worktree (`$(git rev-parse --git-dir)/hostel/lease/lease.json`), so the same command continues the same lease and its place. When it returns with code 75 for a waiting lease, it says how long the server keeps the place without it (`poll_timeout`). With `--background`, a lease that is not ready makes it return at once and start a detached `hostel keep-place --dir <state dir>` that polls the lease until it is ready or ends (one keeper for each lease: `keeper.json` holds its ID, the lease ID, its process ID, and its heartbeat; it also stops when the worktree has another lease, after 1 h without an answer, and after 24 h; it removes its heartbeat when it stops; server errors and network errors do not stop it). |
| `hostel sync [--setup]` | Sends the changed files again. `--setup` runs the setup job again. |
| `hostel run [--detach] [--wait 100s] [--json] -- <command>` | Starts a job in the work directory and streams its output. One argument runs with `sh -c` (`hostel run -- "make && make test"`); several arguments run without a shell. The command is found in the job's PATH (with the mise tools). |
| `hostel attach <job> [--wait 100s]` / `hostel jobs` / `hostel stop <job>` | Job control. |
| `hostel screenshot [--tool <tool[:option]>] [-o file.png]` | Runs the `screenshot` script of a screen tool of the lease and downloads the PNG. `--tool` is necessary only when the lease started more than one screen, or a screen tool with several options. |
| `hostel info` | Prints the lease facts again (acquire step 9). |
| `hostel cp <from> <to>` | Copies files with `tar-out` or `tar-in`. A `:` prefix marks a path in the work directory. The target is the full new path; a target that ends with `/` (or an existing local directory) receives the source with its own name. Both sides refuse to write below a symbolic link and replace a link at a file path instead of following it. |
| `hostel diff` | Shows the temporary changes against the last baseline commit. |
| `hostel ssh [-- command]` | Interactive shell, or one command, with the system `ssh` client and the file of `ssh-config`. |
| `hostel ssh-config` | Writes an `ssh_config` file for `ssh -F` (gateway address, lease user, lease key, pinned gateway host key) and prints its path. |
| `hostel tunnel list` | Lists the tunnels with their local and machine ports and their state: `confirmed` only when the machine listens and a local server answers, else what is missing. |
| `hostel tunnel add <name> (--local-port N \| --local-port-env VAR) [--machine-port P]` / `tunnel set <name> [--local-port N] [--machine-port P]` / `tunnel remove <name>` | Changes tunnels during a lease (section 14). |
| `hostel forward <machine-port> [--local-port N]` / `forward list` / `forward stop <machine-port>` | Guest → machine forwards (section 14). |
| `hostel status [machine \| template \| tool]` (aliases `ls`, `machines`) | Plain text for agents and scripts (people use the app, below): the machines with their leases (user, task, tools, time, lease ID) and their inventory, the templates and static machines that have no running machine, and the waiting line. `hostel status <machine name>` prints the details of a machine (hardware, inventory, leases with jobs, the waiting leases that it can serve, events, machine ID); `hostel status <provider>/<name>` prints a configuration entry (kind, OS, size, tools, its machines now); `hostel status <tool>` prints the waiting leases that asked for that tool (position, user, task, tools, priority, wait, estimate, lease ID, reason). Control characters from users (tasks, commands) are removed. |
| `hostel release` | Ends the lease (or cancels it if it is still queued) and stops the port process. |
| `hostel agent-guide` | Prints short usage instructions for coding agents: the general rule ("ask for the tools you need; ask for a screen tool only when you need to see the app"; the software of the machine is always usable; one lease per worktree; read the machine facts at ready), the commands, then the `GUIDE.md` of each tool that a machine of the server lists (from `GET /v1/tools`). Without a connection, it prints the general part and says that the tool guides come from the server. |
| `hostel update [--check] [--version v]` | Replaces this binary with a newer release (section 2, "Releases and updates"). |
| `hostel screenshot [--tool t] [-o file.png]` | (see above) The script writes to a file in the room directory; the guest downloads it, deletes it on the machine, and prints the local path. |

Connection and users (M8, `multiplayer.md`): `hostel connect <invite code>`,
`hostel disconnect`, `hostel config users [invite|code|set|remove]` (`remove
<name> --computer <computer>` removes one computer only), and
`hostel release <lease>` (another user's lease: admins only).

Admin commands: `hostel admin token create --role machine|revoke`, `admin pubkey`,
`admin lease release <lease-id>`, `admin machine drain|undrain|repair|destroy <machine>`,
`admin config check|schema`. The machine commands take the machine name or ID;
`hostel status` shows the lease IDs, `hostel status <machine>` the machine ID.

**The Hostel app (M10b).** People use `hostel` without arguments when stdin
and stdout are terminals: a full-screen app (Bubble Tea, `internal/app`); in a
pipe, `hostel` prints the help. Agents and scripts use only the `hostel <verb>`
commands; `hostel status` prints plain text with the next commands at the end.

- Not connected: *Check in* (an invite code, then the name; the same code as
  `hostel connect`, package `internal/connect`) or *Leave*.
- Main menu: *Front desk* (key `1`), *Lobby* (key `2`), *Rooms* (key `3`), and
  *Update* when a newer release exists. There is no Machines screen (owner
  decision, 2026-11-06). Themed labels have a muted plain description (owner
  decision, 2026-11-03).
- *Front desk*: *Guests* (*Invite a guest* for admins, *Add your other computer*,
  then the people), *This server* (address, version, and *Update the server*
  when it applies), and *Check out*.
- *Lobby*: the one waiting line first (each queued lease, in order, with its
  estimated wait); then Screens (one line each: vacancy, machines, waiting);
  then Software, in a short wrapped list. Enter on a tool opens the tool page
  (its options, its machines with the tool's options on each, the typical
  lease, its waiting leases with the tools that each asked for, and the first
  paragraph of its guide). Enter on a waiting lease opens the tool page of its
  first tool.
- *Rooms*: one list, provider › machine › room. Rooms are small squares by the
  machine: blue occupied, green vacant, yellow in setup or cleanup, red broken.
  Each room has one line: the guest, "Vacant", or its state. Gauges show the CPU and memory use. A
  provider shows text only when it does something special ("creates machines
  when needed"); a static provider is only a heading. Enter opens the machine
  (the OS, the hardware, the inventory with each tool's options and check
  state, the live use, and each lease with its user, task, tools, computer,
  repository, branch, and running jobs), the room page (from a guest line), the
  provider page (on-demand only), or the template. Admins: `a` opens the
  machine actions (stop or allow new leases, repair), and `+` adds a machine.
- Actions ask "… ? [y/N]" (or a choice) and use the API: end a lease (admins:
  any; members: their own), move (`shift+↑/↓` or `K`/`J`) and set the priority
  of a queued lease, stop or allow new leases on a static machine and repair it, invite
  someone, change a role, remove a user, a code for the caller's other
  computer, a machine code.
- Data: one long poll of `GET /v1/status`; `GET /v1/tools` after each
  status answer; `GET /v1/usage` every 10 s while Rooms is open; a machine page reads `GET /v1/machines/{name}` when it opens and
  at most every 10 s. Each request has a deadline; after an error the app asks
  again after 5 s; a refused token (401/403) goes to Check in. Text from users
  and the configuration is cleaned of control characters before it is drawn.
- Invite codes and join commands can be longer than a screen line: the app
  wraps them, and prints them again after the full screen ends, to copy.
- Presence (owner decision, 2026-11-03): every `intervals.presence` (default
  30 s) the server makes an SSH connection with its key to each idle static
  machine and to each offline one, and closes it (`presence.go`; through the
  gateway for a machine that connects to the server; 10 s deadline). It logs
  in on purpose: OpenSSH 9.8 and later (`PerSourcePenalties`) penalizes a
  source whose connections close without authentication, and then refuses the
  server's own connections. A running machine without a lease
  that does not answer goes offline (broken, reason `unreachable`, the state
  that the scheduler already skips); an offline machine that answers provisions
  again. `machines.last_seen_at` (migration 009) is the last answer (a presence
  check or a provisioning); the status shows it as `last_seen` for an offline
  machine. The reconcile loop no longer provisions unreachable machines blindly.
  The app shows "Offline · last seen …" in Rooms, and the Lobby leaves offline
  machines out; `hostel status` prints "offline (last seen …)".
- The look (owner decisions, 2026-11-03 and 2026-11-05): each page has the
  header bar (the Hostel sign, the place, the user and role) and the footer
  (the description of the selected item, or a question or message; then the
  keys as chips; `?` lists all keys). The levels stay: the main menu, lists
  (Lobby, Rooms, Front desk, Guests), and detail pages (a tool, a machine, a
  room, a provider, a template). A list item has a bold title line and a dim second line, each
  with a part on the right; the selected item has an amber bar on the left. A
  detail page starts with the name, a description, and a summary line, then
  sections with a small heading and a thin line, separated by empty space (no
  boxes). On a tool page one cursor goes from the machines down to the
  waiting line (`tab` jumps between them). A state shows as glyph, color, and
  word (● Vacant, ◉ Occupied, ◐ Getting ready, ✕ Broken, ○ Offline). Colors
  (ANSI 256; `NO_COLOR` turns them off): amber for Hostel and the selection,
  green vacant, blue occupied, yellow waiting, red broken, gray for secondary
  text. Motion stays inside a page: waiting glyphs turn and a changed state
  glows for a moment; pages do not slide; `HOSTEL_NO_ANIMATION=1` turns the
  motion off. Only the main menu has a drawing, a small street as an easter
  egg (`street.go`): the hostel has 10 windows with a yellow light, and the
  share of lit windows follows the share of occupied machines (a fixed order
  for each server); the other buildings, a lamp, and a tree are decoration;
  rarely a car passes (a random time of up to 6 minutes before each car). The
  live use shows as gauges. `1` `2` `3` jump to Front desk, Lobby, and Rooms. Selectable items
  of the main menu, Front desk, and Check in have an emoji as icon; the title
  starts at a fixed column after it (the escape code CHA), because terminals
  do not agree on the width of some emojis. Each choice and each
  question before a change is a popup over the page, which turns gray: ↑↓ and
  Enter choose, Esc cancels, and the letter of an option chooses it at once
  (y and n in a question; Cancel is selected first; a change that cannot be
  undone is red). Enter on a waiting lease of a tool page, or on a person, opens its actions.
  `hostel --demo` runs the app with made-up data and no server (`demo.go`);
  it does not read or change the guest configuration.

All guest commands except `tools`, `status`, and `agent-guide` call
`POST /v1/leases/{id}/touch`. When a lease command fails (for example because
the gateway closed the connection at expiry), the guest calls `touch` again; if
the lease ended, it reports the end reason instead of the connection error. An
attach that cannot connect again stops at once when the lease ended.

Example:

```
$ hostel tools
TOOL               OPTIONS                      SCREEN  MACHINES                FREE  WAITING  EST. WAIT
android-sdk        36                                   home/macbook, ns/linux  2     0        now
android-emulator   pixel-9, pixel-tablet        yes     home/macbook, ns/linux  1     1        ~6 min
xcode              26.1                                 home/macbook            1     0        now
ios-simulator      iphone-17-pro, ipad-pro-13   yes     home/macbook            1     0        now
macos-desktop                                   yes     home/macbook            1     0        now
From the repository's mise files, on every machine: node, pnpm (and any mise tool).

$ hostel acquire ios-simulator:ipad-pro-13 --task "QA the sidebar on iPad"
$ hostel acquire android-sdk jdk --task "build the Android app"
$ hostel acquire --task "run the API test suite"
```

### Exit codes and output

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | Error. The message starts with `hostel:`. |
| 2 | Usage error. |
| 69 | No configured machine can satisfy the request: an unknown tool or option, or no machine with all requested tools (R4). The message names the reason. |
| 75 | Not finished. The last line on stderr names the command to run next. For `acquire` it is the same command; for `run` and `attach` it is `hostel attach <id>`. Never run `hostel run` again for this reason: that starts a second job. |
| other | `run` and `attach`: the exit code of the remote command. |

Because a remote command can also exit with 1, 2, 69, or 75, `run` and `attach`
always print a last line on stderr: `hostel: job <id> exited with code N`,
`hostel: job <id> is still running; run "hostel attach <id>"`, or
`hostel: job <id> was lost`. Agents must read that line. `--json` prints one JSON
object on stdout with an explicit `status` field.

Progress goes to stderr. The final result goes to stdout.

## 11. Server API

All requests except `POST /v1/connect` use `Authorization: Bearer <token>`. JSON
in and out. A user can read and change only their own leases (from any of their
computers); an admin can do everything. Any user can read `status`, `usage`,
`tools`, and `users`. Machine tokens can only read `GET /v1/info`. Requests and answers
have `X-Hostel-Version` (M10d); a guest older than the minimum gets HTTP 426.

| Method and path | Effect |
|---|---|
| `GET /v1/info` | Server version, gateway address, gateway host key, server SSH public key (for `hostel admin pubkey`). |
| `POST /v1/server/update {"version"}`, `GET /v1/server/update` (admin) | Starts a remote update (`""`: the latest release; HTTP 202 with `{"state", "from", "to"}`; 409 while one runs or when the release is not newer); its state `{"state": "idle\|running\|failed", "from", "to", "error"}` (M10d). |
| `GET /v1/tools` | Tools: name, description, screen flag, needs, `options` (the configured options of a screen tool, or the versions in the inventories), `max_options`, `whole_machine`, the environment names, the guide, the machines that have it (key, OS, options on that machine, online), free rooms, waiting leases, estimated wait, and `typical_lease_seconds` (the median of the last leases that asked for it). Also the repository-level note (mise tools on every machine). |
| `POST /v1/leases` | Creates a lease: `{"tools": ["android-sdk", "android-emulator:pixel-9"], "task", ...}`. Returns its ID, state, and revision. HTTP 422 with `{"code": "unknown_tool" \| "unknown_option" \| "too_many_options" \| "no_machine", "message"}` for R4; `no_machine` names the tools that no machine has together and the machines of each tool. |
| `GET /v1/leases/{id}?since=R&wait=50s` | Returns the lease when its revision is greater than `R`, or after `wait` (at most 60 s and half of `poll_timeout`, so the guest polls again before the timeout). Updates `last_poll_at` when the token is the lease owner (an admin can look without keeping a lease alive). The lease has `poll_timeout_ms` and `idle_timeout_ms` (the server settings), for the messages of the guest. |
| `POST /v1/leases/{id}/touch` | Records guest activity. HTTP 409 with `{"code": "lease_ended"}` and the end reason for a lease that ended; the guest then marks its local state as final. |
| `PUT /v1/leases/{id}/tunnels` | Registers the lease's tunnels: `[{"name", "machine_port"}]` (`machine_port` 0 = select from the room pool). Returns the machine port of each tunnel, or HTTP 409 for a port that is not available. |
| `POST /v1/leases/{id}/release` | Releases or cancels. For another user's lease (admin), the end reason is "released by <admin> (admin)". |
| `POST /v1/leases/{id}/move`, `/priority` (admin) | Queue control (section 5): `{"first": true}` or `{"before": "<lease id>"}`; `{"priority": "low|normal|high"}`. HTTP 409 for a lease that is not queued. |
| `GET /v1/status[?since=R&wait=50s]` | Leases (with their user, ready time, and the estimated wait of queued leases), `providers` (in configuration order: name, `label`, kind, `max_machines`), `templates` (the entries of the configuration, with the `connect_version` of a connected machine, M10d), `machines` (with hardware), the server ID, the caller (user, role), and `revision` (a hash of the content without the wait estimates). With `since`, the answer waits (at most 50 s) until the revision differs from `R`; the server checks again on each lease change and every 2 s (M10). |
| `GET /v1/usage` | The live use of the running machines, by machine name: `{"machines": {"<name>": <output of the helper command usage>}}`. A machine that does not answer has no entry. The server samples only while a client asks, at most once in 8 seconds, and shares one sample between clients. Each read has a deadline (M10g). |
| `GET /v1/machines/{name}` | Machine details (M10): the machine, its template, its leases, the jobs of its ready leases (the server asks the machine helper, `job list`, all in one 10 s deadline; on an error `jobs_error` says "jobs unknown"), the waiting leases that it can serve, its last 10 events, and the live use (`usage`, the helper command `usage` in the same deadline; M10b). A member gets the jobs and events of the member's own leases only (commands and messages can hold secrets). |
| `POST /v1/connect` (no token) | `{"secret", "name", "computer"}` from an invite code; returns `{"server_id", "user", "role", "token"}` once. HTTP 403 for an invalid code, 409 `name_taken`, 429 after 10 failures per minute. |
| `GET /v1/me`, `DELETE /v1/me` | The token's user, role, and computer; `DELETE` revokes the token (`hostel disconnect`). |
| `GET /v1/users`, `PATCH /v1/users/{name}`, `DELETE /v1/users/{name}`, `DELETE /v1/users/{name}/computers/{computer}` (admin to change) | Users with their computers; `{"role"}`; removal (tokens revoked, leases ended); removal of one computer (its tokens revoked; the user stays). The server keeps at least one admin (HTTP 409 `last_admin`). |
| `GET /v1/config`, `PUT /v1/config`, `GET /v1/config/history` (admin to change) | The configuration document `{revision, document}`; a change `{base_revision, document}` gives the new revision, HTTP 409 (`conflict`) if `base_revision` is not the latest, or HTTP 422 with the errors (M9) |
| `GET /v1/config/tools`, `GET/PUT/DELETE /v1/config/tools/{name}`, `POST /v1/config/tools/{name}/reset` (admin to change) | The defined tools with their source and machines; a tool's files `{name, files}`; a new version; removal (HTTP 409 while a machine lists it); a default back to the release's version |
| `POST /v1/machines/invites` (admin), `POST /v1/machines/join` (the code, no token), `GET /v1/install.sh`, `GET /v1/download/hostel-<os>-<arch>[.exe]` (no token) | A machine code with the commands to run; the join (adds the machine, returns a machine token and the server's SSH key); the install script and the binaries of `helper_dir` (M9) |
| `POST /v1/logins/{cli}`, `GET /v1/logins/{cli}[?wait=1]` (admin; `cli` is `devbox` or `nsc`) | Starts a provider CLI login on the server and returns its address (or `already`); the state (`waiting`, `done`, `failed`) (M9) |
| `POST /v1/invites` | `{"role", "name"}` (admin) or `{"self": true}` (a code for the caller's other computer); returns the code, the key fingerprint, and the expiry (24 hours). |
| `POST /v1/machines/{id}/drain`, `/undrain`, `/repair`, `/destroy` (admin) | Machine control. `repair` takes `{"accept_host_key": bool}` and is for broken static machines; `destroy` is for on-demand machines without a lease. |
| `POST /v1/tokens`, `DELETE /v1/tokens/{id}` (admin) | Token control. Request: `{"name", "role", "template"}` (`template` = `<provider>/<machine>`, only for the role `machine`). A new token is returned once. |

A `ready` lease includes the access data and the lease facts: gateway address,
gateway host key, gateway user, helper path, room name, work directory, tunnel
ports, provider and template names, machine OS, OS version, architecture, and
hardware, the machine's inventory (each tool with its option, state, and
environment names), the started screen tools with their options and `tool.env`
names, `lease_tools` (the names of the requested tools and their needs, in the
order of needs: the `[tool.<name>]` sections of `hostel.toml` that apply),
`tunnel_check`, `expires_at`, and `idle_expires_at`. Each lease (also
queued) has `tools` (the request), `waiting_behind` (queued), and `started`.

## 12. File sync

### Manifest and plan

1. The guest lists files with `git ls-files --cached --others --exclude-standard -z`,
   removes paths that do not exist (deleted, not yet staged), and adds the files that
   match `include`. It never sends `.git`.
2. For each entry it computes the type (`file` or `symlink`), mode, size, and
   SHA-256 (for a symlink: the link target). It hashes all files at each sync. Do
   not cache hashes: a cache keyed by size and modification time misses fast edits
   (this happened in the prototype).
3. Manifest: `{"entries": [{"path", "type", "mode", "size", "sha256", "target"}]}`.
   Paths use `/` and are relative to the repository root.
4. `sync-plan` compares the manifest with the files in `work/` and with
   `manifest.json`. It returns
   `{"plan_id", "send": [paths], "delete": [paths], "overwrite": [paths]}`:
   - `send`: entries that are missing or different in `work/`;
   - `delete`: paths in the old `manifest.json` that are not in the new manifest;
   - `overwrite`: paths in `send` that a machine command changed after the last sync.
     It also lists machine objects that block a `send` path (step 2 of
     `sync-apply`).
   The helper stores the plan and the new manifest in `plans/<plan-id>.json`.
5. The guest sends a `tar` stream with exactly the `send` paths to
   `sync-apply <plan-id>`. The helper applies the plan in this order:
   1. Delete the `delete` paths, deepest first. Then remove directories that became
      empty, up to `work/`.
   2. For each `send` path: if any existing parent component is not a directory (a
      file or a symbolic link), or the path itself has another type (for example a
      directory where a file must go), remove that object first, but only if the
      plan lists it in `overwrite`; else stop with an error (a machine job made it after
      the plan).
   3. Write the files, and create missing parent directories.
   4. Replace `manifest.json` and make a baseline commit.
6. `hostel sync` prints the `overwrite` paths.

Files that machine commands created (build output, generated projects) are never in
`delete`, so they stay.

### Path safety

The helper must not write or delete outside `work/`:

- Reject absolute paths, `..` components, and paths with `\` or NUL.
- Before each write or delete, check each parent directory with `Lstat`. Reject the
  operation if a parent is a symbolic link.
- Write a symbolic link as a link. Never follow it.
- Delete only paths that the old manifest lists, and machine objects that block a
  `send` path and that the plan lists in `overwrite` (`sync-apply` step 2).

### Baseline commits

- The baseline uses a separate git directory (`baseline.git`) with
  `GIT_DIR=baseline.git GIT_WORK_TREE=work`. `work/` has no `.git`, like an export.
- After each sync, the helper stages exactly the manifest paths and the deleted
  paths, with `git add -A -f -- <paths>` (`-f` because `include` files such as
  `.env` are ignored files; `-A` stages the deletions), and commits. It does not
  stage other files.
- `hostel diff` shows changes to manifest files and new files that the repository's
  `.gitignore` does not ignore.
- If `git` is not on the machine, the helper skips the baseline and `diff` reports that
  it is not available.
- A build step that needs git metadata (`git describe`) does not work in `work/`.
  Check this for a real app build in M2.

This design works on all machine OSes without `rsync`.

## 13. Gateway and access

### Principle

The guest connects only to the gateway. The gateway is a full SSH server and SSH
client (`golang.org/x/crypto/ssh`), not a relay of encrypted bytes:

```
guest ──SSH (lease key)──► gateway ──SSH (server key)──► machine
```

- The lease key exists only on the guest (private part) and in the server database
  (public part). It is never installed on a machine.
- The machine's `authorized_keys` contains only the server key (and the provider's own
  key, if any). Cleanup restores it from `authorized_keys.base`, so a key that a
  guest added during a lease is removed.
- The server verifies the machine host key (recorded at the first connection). The
  guest verifies only the gateway host key.
- Because the gateway terminates SSH, it sees the plaintext of the sessions. This is
  acceptable: the gateway is the owner's own server.

### Gateway behavior

- **Transport.** The gateway listens on TCP (`gateway_listen`). It also accepts
  the same SSH inside a WebSocket (binary messages) on the API listener at
  `GET /v1/gateway`, for a server that only HTTPS reaches (a VPS behind an HTTPS proxy, M5).
  `gateway_public` tells guests which one to use: a `host:port`, or a `ws://` or
  `wss://` URL (port 0 means the real port). For the system `ssh` (`hostel ssh`,
  `hostel shell`), the ssh_config of a WebSocket gateway has a `ProxyCommand`
  with `hostel gateway-proxy <url>`, which connects stdin and stdout to the
  WebSocket.
- **Gateway host key.** `hostel server init` generates it. Guests get it from
  `GET /v1/info` over the authenticated API and pin it.
- **Gateway user.** `<lease-id>.<purpose>`. Purpose is `cmd` (commands, sync,
  shell) or `ports` (the port process; section 14). The gateway accepts only the lease's public key and only while
  the lease is `ready`.
- **Upstream connection.** For each guest connection, the gateway opens one SSH
  connection to the lease's machine with `MachineAccess` (dial and server signers).
- **Sessions.** The gateway supports `exec`, `shell`, `pty-req`, `env`,
  `window-change`, `signal`, and `exit-status`. It wraps each session in the
  helper: `shell` becomes `hostel helper shell --room <room>`, and `exec <string>`
  becomes `hostel helper exec --room <room> --command-b64 <base64 of the string>`. The
  helper decodes the string and runs it with `sh -c` (macOS, Linux) or PowerShell
  (Windows: PowerShell 7 `pwsh` if it is in the machine's `PATH`, else Windows
  PowerShell 5.1, with `-NoProfile -NonInteractive -Command`; not
  `-EncodedCommand`, with which Windows PowerShell writes stderr as CLIXML). On
  Windows, a wrapper gives the exit code of the last command and turns off
  progress records and colors. Base64 avoids all quoting problems.
  Guest calls of the helper are wrapped the same way; there is no exception. So every
  guest process has the lease environment and `HOSTEL_LEASE`, and cleanup can find
  it (section 17).
- **Port forwarding.** `direct-tcpip` from the guest goes to the same port on the
  machine's loopback interface. `tcpip-forward` from a `ports` connection is sent to
  the machine only for machine ports that the lease registered, and the gateway relays the
  `forwarded-tcpip` channels back (section 14). Forwarding is allowed only to and
  from `127.0.0.1` on the machine.
- **End of access.** When a lease leaves `ready` for any reason, the gateway closes
  all guest connections and upstream connections of that lease at once, before
  cleanup starts. An admin can end a lease at any time with
  `POST /v1/leases/{id}/release`.

### Network rule for machines

The gateway can enforce access only if the machine accepts SSH from nothing else.

| Provider | How the rule holds |
|---|---|
| `namespace` | The machine is reachable only through the Namespace proxy, which needs the server's Namespace login. |
| `static`, behind NAT (`connect = true`) | The machine has no inbound SSH from the internet. It connects out to the gateway. |
| `static`, public (VPS) | The owner must allow SSH only from the server's address (firewall), or use `connect = true` and close inbound SSH. `plan.md` (M5) includes this setup. |
| `external` | The provider must make the machine reachable only from the server. |

Even if a machine breaks this rule, a guest has no key that the machine accepts. The rule
is defense in depth.

### Server access to machines

- Static machines: the owner adds the server public key once (`hostel admin pubkey`).
- External machines: the provider installs `server_key`.
- Namespace: `MachineAccess` from the provider.
- **Machines behind NAT** (M5). `hostel helper connect --server <url> --token-file
  <file>` (or `HOSTEL_MACHINE_TOKEN`; never a token in the arguments, which other
  users can read) runs on the machine as a service. It runs as an account other than
  the machine user and the room users, and only that account can read the token: a
  guest runs as the machine user, and with the token it could replace the machine's
  connection (found in M6; the server pins the machine's host key, so it cannot read
  the server's sessions, but it can stop them). It reads the gateway address and
  host key from `GET /v1/info` (the only API call that accepts a `machine` token),
  connects to the gateway (TCP or WebSocket), and authenticates as user `machine`
  with the token as the password. A `machine` token is bound to one machine when it
  is created (`hostel admin token create --role machine --machine home/macbook`); the
  gateway accepts it only for a static machine with `connect = true`. The gateway
  keeps one connection for each machine (a new one replaces the old one) and opens
  a channel `hostel-machine-ssh` on it for each server connection; the machine connects
  the channel to its own SSH server (`--ssh`, default `127.0.0.1:22`). The machine
  cannot open channels. The static provider's `Dial` uses this for machines with
  `connect = true`; the user comes from `address` (its host is not used). The machine
  sends a keepalive every 30 s and connects again after a loss (backoff 1 s to
  30 s). Without the connection, the machine is unreachable: a probe fails, and guest
  commands of a ready lease fail until the connection comes back. When the machine
  connects, the server probes an unreachable machine at once (not at the next
  reconcile). Revoking a machine token closes the connections that use it; a
  connection checks its token again after it is registered, so a revoke during
  its handshake also ends it. A dial
  through the connection has the same 15 s limit as a TCP dial.

## 14. Port forwarding

Two directions, both TCP only (HTTP, WebSocket, gRPC; no UDP):

| Direction | Name | Use |
|---|---|---|
| Machine → guest | **tunnel** (reverse forward) | The app on the machine reaches the API on the guest. |
| Guest → machine | **forward** (local forward) | The guest reaches a server on the machine (for example a bundler or a debugger). |

### Names

Tunnel names match `^[a-z][a-z0-9-]*$`. The environment variable of a tunnel is
`HOSTEL_TUNNEL_<NAME>_PORT`, with the name in upper case and `-` replaced by `_`.

### Tunnels (machine → guest)

A tunnel has a name, a **local port** (on the guest) and a **machine port** (on the
machine's loopback interface).

- **Declaration.** In `hostel.toml` (section 9), or at runtime with `hostel tunnel
  add`:

  ```toml
  [tunnels.api]
  local_port_env = "PORT"   # or: local_port = 3000
  # machine_port = 3000        # optional; default: a port from the room's pool
  ```

- **Local port.** In this order: an explicit `local_port` (or `--local-port`);
  else the variable `local_port_env` in `.env` in the repository root, read again
  for each incoming connection; else the same variable in the environment of
  `hostel acquire`. So a guest can change its API port at any time (for example
  restart the API on another port, or `hostel tunnel set api --local-port 4001`).
  The next connection goes to the new port. No restart is necessary. The guest
  connects to `127.0.0.1:<port>` first and then to `[::1]:<port>`, because a local
  server can listen on either loopback address.
- **Machine port.** A tunnel can request a fixed `machine_port` (1024 to 65535), for
  example when an app has a hardcoded address. Else the server gives the lowest free
  port from the room's pool (10 ports for each room; section 9). At lease creation,
  the server gives pool ports to the tunnels in name order. The machine port stays the
  same for the whole lease unless the guest changes it, so a value that a build
  embeds (for example `APP_API_BASE_URL`) stays valid.
- **Assignment and registration.** `POST /v1/leases` contains the tunnels
  (`[{"name", "machine_port"}]`, `machine_port` 0 = from the pool). The scheduler uses
  them for candidate matching (section 5). The server records the machine ports in
  `lease_tunnels` when it assigns the room, and returns them with the `ready` lease.
  `PUT /v1/leases/{id}/tunnels` changes the list during a lease (and raises the
  lease revision). The server refuses a port that another lease on the machine uses
  (HTTP 409), and two tunnels with the same fixed machine port (also in `hostel.toml`).
  The gateway accepts a remote forward only for a registered port of that lease, and
  after each change it closes the forwards of ports that the lease no longer
  registers. At assignment, a lease does not get a machine where another lease uses one
  of its fixed machine ports; it waits. (Such a lease can wait for that machine although
  another machine is free; a later version can treat fixed machine ports as a resource of
  the machine.)
- **Changing the machine port.** `hostel tunnel set <name> --machine-port P` registers the
  new port, closes the old forward, and opens the new one. The guest updates
  `lease.env`, so new jobs get the new `HOSTEL_TUNNEL_<NAME>_PORT` and the new values
  of the repository `env`. Running jobs keep the old values. The command warns that
  a build that embedded the old address must be built again.
- iOS simulators use the machine's `localhost`. Android emulators reach the machine's
  `localhost` at `10.0.2.2` on every OS.

### Forwards (guest → machine)

- `hostel forward <machine-port> [--local-port N]` listens on the guest's
  `127.0.0.1:<N>` (default: the same number, or a free port if it is in use) and
  sends each connection through the gateway (`direct-tcpip`) to the machine's
  `127.0.0.1:<machine-port>`. It prints the local address.
- `hostel forward list` and `hostel forward stop <machine-port>` control forwards.
- The gateway allows `direct-tcpip` only to `127.0.0.1` on the lease's machine. On a
  machine with one room, any port is allowed (for example a bundler on 8081). On a machine
  with several rooms, only the ports of the lease's room, unless the lease holds
  all rooms (an exclusive lease) (M6). A tunnel cannot use a port of another
  room either. This keeps the gateway paths apart; it does not isolate the rooms
  on the machine: all room users share the machine's loopback interface, so a guest
  command on the machine can connect to a server of the other room's lease. Room
  users keep files and processes apart, not loopback ports.

### The port process

One detached local process for each lease (`hostel ports`, hidden from help)
handles all tunnels and forwards of the lease.

- **Start.** `hostel acquire` starts it before the sync and the setup job (section
  10), so setup commands can already reach guest services. Every lease command
  starts it again if it is not running.
- **Gateway connection.** It connects to the gateway with purpose `ports`. On this
  purpose the gateway allows remote forwards (registered ports only) and
  `direct-tcpip`. `ports` connections do not count as guest activity (section 16):
  a forgotten forward does not keep a lease alive.
- **Machine listeners.** The gateway asks the machine's SSH server to listen on
  `127.0.0.1:<machine port>` (this must succeed) and on `[::1]:<machine port>` (best
  effort; the machine can have no IPv6 loopback). It does not ask for `localhost`:
  OpenSSH accepts `localhost` when only one of the two addresses is free, and a
  machine service on `127.0.0.1` would then get the tunnel's connections.
- **Control.** The process listens on a local control socket. Unix socket paths
  are limited to 104 bytes on macOS, so the socket is
  `/tmp/hostel-<uid>/<first 16 hex digits of sha256(local state directory)>.sock`
  (not `$TMPDIR`, which is long on macOS). The directory must belong to the user
  and have mode 0700, because `/tmp` is shared. On Windows the socket is a Unix
  socket in `%LOCALAPPDATA%\hostel\` (Go supports Unix sockets on Windows 10 and
  later), so no named pipe is necessary.
- **Next lease.** All leases of a repository use the same socket. The process of
  an ended lease stops only at its next lease check (`tunnel_check`). Its ping
  answer has its lease ID; when the ID is not the current lease, a lease command
  stops that process, waits until the socket no longer answers, and starts its
  own. Only the listener's close removes the socket file.
- **Files.** The tunnels of a lease (local port source, requested and given machine
  port) are in `tunnels.json` in the local state directory; only `acquire` (before
  the process starts) and the process write it. The process writes its log to
  `ports.log` there (events only, no data). `hostel tunnel add|set|remove`
  and `hostel forward ...` send their request there. The process applies it and
  replies with the result. If the machine's SSH server refuses a remote forward (for
  example a machine service already uses a fixed port), the command fails with that
  message and the server registration is undone. `hostel acquire` waits until all
  tunnels of `hostel.toml` are confirmed; a refused tunnel makes it exit with code 1.
  The machine side alone does not make a tunnel work. The `check` request also finds the
  local port and connects to it, for `hostel tunnel`, the lease facts, and no other
  use. A missing local server does not stop a lease: it can start later, and a build
  may not need it.
- **Lease state.** Every `tunnel_check` (a value in the lease data) it reads the
  lease with `GET /v1/leases/{id}`. It never calls `touch`. It exits when the lease
  is no longer `ready`. `hostel release` stops it at once.
- It reconnects with backoff when the gateway connection drops, and requests the
  forwards again. It also survives a server restart.
- **Exit.** It exits when the lease is final or not found, on SIGTERM, or when the
  server is unreachable for `port_process_offline` (default 10 minutes; a value in
  the lease data). So a crashed test or a lost server never leaves it running.

## 15. Jobs

- `job start` starts a **supervisor** (`hostel helper job supervise`) in a new session
  (`setsid`) on macOS and Linux. On Windows, the SSH server puts each session in a
  job object that ends its processes when the session ends; the supervisor starts
  with `CREATE_BREAKAWAY_FROM_JOB`, and it starts the command suspended in a job
  object of its own (`Local\hostel-<user SID>-<room>-<job id>`, with
  `KILL_ON_JOB_CLOSE` and without breakaway), then resumes it. `job stop` and
  cleanup terminate that job by name. A guest process cannot leave it, and when
  the command ends, the rest of its job ends too (on Windows, a background process
  of a job does not outlive the job's command). Windows has no SIGTERM for
  processes without a console: stop and cleanup end them at once. The guest chooses
  the job ID (`j-<unix-ms>-<4 random hex>`; the helper makes one if the request has
  none). A start with an existing ID returns that job, so a guest whose connection
  dropped during the start repeats it with the same ID and gets one job, not two.
- The supervisor starts the command in its own process group, in `work/`, with the
  lease environment. Output (stdout and stderr together) goes to
  `jobs/<id>/output`. When the command ends, the supervisor writes the exit code to
  `jobs/<id>/exit` (write to a temporary file, then rename).
- `supervisor.json` records the PID and start time of the supervisor and of the
  command's process group. Before `job stop` or a liveness check, the helper checks
  the start time too, because PIDs are reused.
- `job attach <id> --offset N` streams the output from byte `N`. When `exit` exists,
  it sends the rest and a trailer line `\x00hostel-exit <code>`. If the supervisor is
  dead and `exit` does not exist, the trailer is `\x00hostel-lost`. The job output
  can contain the same bytes, so the guest accepts a trailer only as the last bytes
  of a stream that the helper ended with exit status 0.
- `hostel run` and `hostel attach` record in the local state how many output bytes
  they printed, so a later `hostel attach` continues there and does not print the
  same output again. With `--json`, the job output goes to stderr.
- `hostel run` = `job start` + `job attach`. When the connection drops, the guest
  attaches again from the last offset. If the guest process stops (for example an
  agent tool timeout), the job continues.
- Jobs do not use a terminal. `hostel ssh` gives an interactive terminal.

## 16. Activity and expiry

- `last_activity_at` changes when a guest calls `touch`, when a `cmd` connection
  opens, when a `cmd` connection closes, and when data goes through a tunnel or a
  forward of the lease, in either direction (M10h). The gateway records data at
  most once per 30 s, or per a quarter of `idle_timeout` when that is shorter. So a
  test on the guest that only uses a tunnel keeps its lease. Hostel's own checks
  send no data on these channels: a tunnel is confirmed by its remote listen.
- A lease is active while the gateway has an open `cmd` connection for it.
  An open `ports` connection, tunnel, or forward without data does not count.
- A `ready` lease expires when it has no open `cmd` connection and
  `last_activity_at` is older than `idle_timeout` (default 10 minutes).
- At server start, `last_activity_at` of each `ready` lease and `last_poll_at` of
  each lease before `ready` are set to the start time, so that a restart does not
  expire or cancel leases.
- A detached job does not keep a lease active. The agent must come back within the
  timeout.
- `expires_at = ready_at + max_lease`. It ends a lease in all cases.
- Before `ready`, `poll_timeout` applies: a lease in `queued`, `assigned`,
  `provisioning`, or `preparing` whose `last_poll_at` is older than `poll_timeout`
  is cancelled (through `cleaning` if it has a room).

## 17. Cleanup

The gateway has already closed the lease's connections (section 13). The helper
`cleanup` command then runs these steps. Each step runs even if an earlier step
failed; the result lists each step.

1. Stop all processes of the lease: job process groups from `supervisor.json`, and
   all processes of the machine user that either have `HOSTEL_LEASE=<lease id>` in
   their environment (macOS: `ps -E`, Linux: `/proc/<pid>/environ`) or are in a
   session of the lease. Processes that detach from their group (Gradle daemon,
   `nohup`) still have this variable. On macOS 27.2, `ps -E` does not show the
   environment of Apple's own binaries (for example `sh` and `sleep`; checked on
   2026-10-22), so the sessions are necessary: `exec` and `shell` record their
   session ID and the start time of its leader in `rooms/<room>/sessions`, and
   each job supervisor has its own session (`supervisor.json`). A process started
   with `&` or `nohup` stays in that session. A session ID is not reused while a
   process is in the session; when the leader runs with another start time, the
   number belongs to a new session, and cleanup ignores it. One `ps` call per
   scan reads the start times; if it fails, the scan fails (it does not guess
   that a leader ended). Each scan also takes
   all descendants of these processes (by parent PID), also in other sessions, and
   records their sessions in `rooms/<room>/sessions`: on macOS 26.6 the iOS runner
   of `agent-device` is an `xcodebuild` in a new session, without a visible
   environment; after its parent ends, its parent is PID 1, and later scans find it
   only by its session (found in M2, 2026-10-23). A scan also runs at the start of
   each guest command and each job. Only a process that starts a new session and
   hides its environment, and whose parent ended before the next guest command or
   cleanup, escapes; the `ios-simulator` `stop` also stops the processes whose
   command line has the UDID of a room simulator (the `agent-device` runner has it). Send
   SIGTERM, wait 10 seconds, then SIGKILL.

   On Windows (M6), the helper reads `HOSTEL_LEASE` from the environment block of
   each process of the machine user (through its PEB), and finds descendants through
   the parent PIDs of a Toolhelp snapshot; a child must be younger than its parent,
   because Windows gives the PID of an ended parent to new processes. Windows has
   no Unix sessions. A guest command (`exec`, `shell`) runs in a job object
   without breakaway, inside the SSH session's job, so its processes end with
   the session and cannot leave it; only the helper's own `job start` runs
   outside it (its supervisor must break away, section 15).
   The helper merges environment sources with upper-case names on Windows, which
   ignores the case of names (`Path` is `PATH`).
2. Run the `stop` script of each screen tool that started for the lease, in
   reverse order of `start`. If `rooms/<room>/` does not exist (a repeated cleanup
   after step 5), the helper creates it empty first; `stop` must then do nothing.
3. Restore `authorized_keys` from `authorized_keys.base`.
4. Check that no process of the lease (step 1) remains: for up to 10 seconds,
   end the ones that still run (a process can need time to end, or it started
   during step 2), then fail with their PIDs and names (M10h). Check that the room
   ports and the lease's fixed machine ports (`ports` in the input) are free: try to
   bind each port on `127.0.0.1` and on `::1` (skip `::1` if the machine has no IPv6
   loopback address), and retry for up to 10 seconds, because
   the machine's SSH server removes its listeners after the connections close.
   Before step 4 (a new unit or agent can hold a room port) and again after it
   (a process of the lease that step 4 ended can have added an entry),
   restore the persistence locations of the machine user (M11, macOS and Linux)
   from the copy that `begin` made when the machine had no other lease
   (`hostel/persistence/`): the crontab, `~/.config/systemd/user` and
   `~/.config/autostart` (Linux), `~/Library/LaunchAgents` (macOS), the shell
   startup files of sh, bash, zsh, and fish (and `.bash_aliases`), the git
   configuration (`core.fsmonitor` and hooks run commands), `~/.ssh/rc`, and
   `~/.ssh/environment`.
   A new or changed entry is stopped first (`systemctl --user disable --now`,
   `launchctl bootout` of the plist's label, by the name in the audited
   location also when the file is a link; the step fails when the unit or
   agent still runs afterwards, and all stops have one deadline of 2 minutes;
   a machine without a systemd user manager, whose private socket
   `/run/user/<uid>/systemd/private` does not exist, has no running unit; with
   it, a failed `systemctl` call fails the step), then
   deleted or restored,
   also when the guest removed the write rights or set `uchg`; a plist whose
   label cannot be read fails the step. For each link in these locations (also
   inside the directories, and inside linked directories, at most 8 deep), the
   copy also has the content behind the link and its path, and a changed target
   is restored too. The copy also lists the loaded user services (systemd
   services, timers, sockets, paths, and scopes of the user manager; launchd
   services of the gui and user domains) and which managers and domains existed;
   a service that was not loaded then is stopped (on macOS not Apple's own,
   which macOS starts at any time, and none of a domain that did not exist then),
   also one that changed its file or has none (`systemd-run --user`), and the
   step fails when it still runs. A systemd user manager that started during the
   lease is stopped as a whole (`systemctl --user exit`). `begin` and this step decide under a
   lock file (`hostel/machine.lock`) whether another room has a lease, so of two
   rooms that begin at the same time, the first makes the copy. The step runs a
   third time before step 5, in one hold of the lock, and only when it works the
   lease's marker (`lease.json`) goes: of two last rooms that clean at the same time,
   each saw the other's lease before, and the later one restores then; a
   `begin` cannot copy the leaving lease's persistence as the new base. The home
   directory comes from the helper's path and the commands from fixed system
   directories, because the startup files run before the helper and can change
   `HOME` and `PATH`. This runs only when no other room has a lease; the cleanup
   of the last lease does it. The result names the items, and the server records
   them in the event log ("removed persistence of the lease: ..."). A failed
   repair fails the step.

   Limit: the guest of a lease on the machine user is that user, so it can
   change Hostel's own files (the helper, the copy) and defeat this audit. The
   audit removes what a guest leaves by mistake or with a simple attack; against
   a deliberate attack, use lease users (section 18).
5. Delete the devbox task markers of the room's jobs whose supervisor was killed
   (if any), and the directory `rooms/<room>/`. This
   deletes `work/`, `.env`, job output, `tool.env`, and the manifest. The shared
   `cache/` stays. This step is last
   before `check`, so a repeated cleanup still finds `tool.env` until `stop` has
   run.
6. Run the `check` script of each tool that the machine has installed, and update
   the inventory.

If a step fails, the room and the machine become `broken` (R24). On success, the room
is `free`.

Warm policy after cleanup:

- If a machine has no lease for `warm_running`, the server stops it (the machine can
  stop) or destroys it (an on-demand machine that cannot stop). Static machines stay
  running.
- A stopped machine is destroyed after `warm_stopped`.
- With `reuse = false` (M11), the server drains a machine in the same transaction
  that assigns a lease to it, and destroys it when it is idle again, also before
  `warm_running`. The drain is a compare-and-set: of the leases that one
  scheduler pass gives to rooms of the machine, only the first gets it, and the
  others wait. An admin who undrains such a machine allows one more lease.
- Eviction (section 5) can destroy an idle or stopped machine earlier.

## 18. Security

- **Users and tokens** (M8). A user has a unique name and a role, `admin` or
  `member` (`multiplayer.md`, section 6). A user token belongs to a user and a
  computer; it comes only from an invite code (`hostel connect`). Machine tokens
  (only for `hostel helper connect`) have no user. Tokens and invite secrets are
  stored as SHA-256 only. The client's connection file
  (`~/.config/hostel/config.toml`, mode 0600) or `HOSTEL_TOKEN` holds the token.
  `POST /v1/connect` allows 10 failed attempts per source address and minute.
- **Lease keys.** The guest generates a key pair for each lease. The private key
  never leaves the guest.
- **Provider credentials.** Only the server has them.
- **Transport.** With `tls = "self-signed"`, the API and the WebSocket gateway are
  HTTPS with the server's own certificate; clients check its key against the
  key that they pinned from the invite code (`internal/tlspin`), not the name or a
  chain. With `tls = "off"`, the server is behind a proxy with TLS (for
  example Caddy), and clients use the system roots. Both SSH
  legs (guest to gateway, gateway to machine) are encrypted. The gateway sees the
  plaintext.
- **Revocation.** Guests have no machine credentials. Ending a lease closes its
  connections and invalidates its key at once (section 13).
- **Logs.** No tokens, keys, file contents, environment values, or output of guest
  jobs in server logs. Output tails of tool scripts (which the owner controls) go
  to the event log; the scripts must not print secrets.
- **Integrity between leases.** Helper, mise, and tool scripts are checked by
  hash at each `preparing`; `authorized_keys` is restored at cleanup (sections 8 and
  17).

### Backdoors (not in version 1)

The owner wants this later: a guest must not keep access to a machine after its lease,
even if it installed a backdoor during the lease. Version 1 does not guarantee it,
because the guest and the server use the same OS user and machines can be reused.

What version 1 already blocks:

- SSH keys: the machine trusts only the server key, `authorized_keys` is restored at
  cleanup, and the network rule (section 13) blocks direct SSH.
- Processes that keep `HOSTEL_LEASE`, and changed helper or scripts (hash checks).
- New or changed entries in the persistence locations of the machine user (M11,
  section 17, after step 4), when the guest does not change Hostel's own files.

What a guest can still do in version 1 (on a lease without a lease user):

- Start a process without `HOSTEL_LEASE` (for example with `env -i`) that connects
  out from the machine (a reverse shell). Cleanup does not find it.
- Add persistence for the machine user in a place that the M11 audit does not
  check. Before M11:
  `crontab`, `~/Library/LaunchAgents`, systemd user units, shell startup files, or
  `~/.ssh/rc`.
- Put a changed binary in a shared cache (`hostel/cache`, mise tools, package
  stores). The next lease runs it.

Hardening (milestone M11):

1. **One OS user for each lease** (`lease_users = true` on a macOS or Linux
   machine). A lease whose tools have no `machine_user` key gets the user
   `hostel-<first 12 characters of the lease ID>`.
   - **Root step.** `hostel machine join --lease-users` writes
     `/etc/sudoers.d/hostel` (the machine user may run only
     `/etc/hostel/hostel lease-user *` as root, the root-owned binary of the
     join) and `/etc/hostel/server_key.pub`. The caller can be a guest on the
     machine user, so the command takes only a name of that form, changes only
     users whose full name is "Hostel lease", reads the key only from the
     root-owned file, sets its own `PATH`, and never follows a path that another
     user controls: it writes only into a home directory that it has just made
     (through `os.Root`). `check` prints its version; the server checks it at
     provisioning (a missing step makes the machine `broken` with a message).
   - **Create** (preparing, before `begin`): the server records the name, then
     runs `create` as the machine user through sudo: an existing lease user of
     that name goes first; the new user gets a user ID that was never used
     before (`/etc/hostel/lease-uid`, written atomically; a damaged counter fails
     the creation), no administrator rights, the group
     `hostel-leases` (its primary group on macOS; also `com.apple.access_ssh`
     when Remote Login allows only some users), a home directory with mode
     0700, and the server key. The machine user joins the group. Then the
     machine user runs `helper share-cache` (item 2), and the server, as the
     lease user, fills its home: its own `hostel/` with copies of the machine's
     helper, mise, tools, and mise configuration, its own `cache/` with links to
     the machine's cache entries (not mise's own directories), and `rooms/`.
     The server records the home directory. The room steps and the guest's
     connections then use the lease user, as for room users (section 7).
   - **Delete** (cleanup, after the helper's cleanup as the lease user): end all
     processes of the user (`launchctl bootout`, `loginctl terminate-user`,
     `pkill -KILL -u` until none is left), remove its crontab, its `at` jobs (a
     failure stops the deletion, so the room stays blocked), and
     its files in the shared directories (`/tmp`, `/var/tmp`, `/dev/shm`; on
     macOS also `/private/var/folders` and `/Users/Shared`; `find -delete` as the user,
     after it gives its own directories back their rights and clears its own
     macOS flags; then root only looks with `find -print`, and a file that is
     left stops the deletion), then its home directory (after root
     clears the macOS flags `uchg` and `uappnd` without following links), then
     the user record. A repeated deletion that finds no user removes a home
     directory that is left. The lease user owns its helper and
     its login: when the helper's cleanup fails, or the server cannot log in as
     the user, the deletion runs anyway, and only a failed deletion makes the
     machine `broken`. The cleanup and each provisioning also delete lease users
     that no active room records (a failed preparing leaves one). One server
     lock for each machine orders creation and these sweeps. When the server cannot
     log in as the lease user (the user can break its own login), it deletes the
     user without the helper's cleanup; if tools started for the lease, their stop
     scripts did not run, and the cleanup fails.
   - **Limit.** A guest on the machine user (a lease with a `machine_user` tool,
     or any lease on a machine without `lease_users`) controls the machine
     user's files and shell: it can change what later lease users copy and run
     (the helper, tools, and the shared cache), fake the answers of the root
     calls that run through the machine user's shell, and end the lease users of
     other rooms. Lease users protect a machine fully only when all its leases
     use lease users: no `machine_user` tools on it.
   - Tools with `machine_user = true` need the machine user's account
     (`macos-desktop`, `ios-simulator`, `android-emulator`, `windows-desktop`):
     a lease with one of them uses the machine user and gets the audit
     (section 17).
2. **Read-only shared caches.** The machine user owns the caches and installs
   tools into them. `helper share-cache` (as the machine user) gives its home and
   `hostel/` the group `hostel-leases` with traverse rights only (`begin` keeps
   `hostel/rooms` at mode 0700), `hostel/bin`,
   `tools`, `mise`, and `cache` group read rights, and takes the rights of the
   group and of other users from all other entries there; so a lease user can
   read the cache but not write it, and cannot read the machine user's rooms or
   `machine-env.json`. Its
   mise uses `MISE_SHARED_INSTALL_DIRS` for the machine's tools; a repository
   tool that the machine does not have goes into its own cache, deleted with it.
   A tool that writes into the machine's cache during a lease needs
   `machine_user`.
3. **Egress control (optional).** A machine firewall allows outbound traffic only to
   the gateway and to configured hosts (package registries, GitHub).
4. **No reuse (strongest, M11).** On on-demand machines, `reuse = false` destroys
   the machine after its lease (section 17). No backdoor can survive, at the cost
   of a cold start for each lease.
5. **Audit.** Cleanup compares the known persistence locations with a snapshot and
   marks the machine `broken` if something changed.

Privilege escalation to root on a machine stays out of scope.

## 19. Observability

- Server logs: `log/slog` JSON lines with lease ID and machine ID.
- `events` table: one row for each state change and each provider action.
- `hostel status` shows the machines with their leases (user, task, time, lease
  ID), the templates and static machines that have no running machine, and the waiting line; `hostel status <machine>` adds
  the hardware, the jobs, and the last events (M10).

## 20. Testing

`AGENTS.md` has the rules. Summary:

- **E2E tests are the main gate.** They start the real server binary and run the
  real guest binary as a subprocess, with a temporary home directory and a
  temporary git repository. Machines are Docker containers with `sshd` and `git` only
  (`test/machineimage`). The image must not contain the helper or mise: the server
  uploads them, so the tests cover the upload path.
- **Isolation.** Each test server gets a unique random `server_id`. Every Docker
  object has the label `hostel-e2e=<run id>`. `test/fakeprovider list` returns only
  containers with the label of its run and the requested prefix. So tests that run in
  parallel never see each other's machines.
- **Static machines in tests.** The harness reserves a free host port, then starts the
  container with an explicit mapping `-p 127.0.0.1:<port>:22`. An explicit mapping
  and the container's host key survive `docker stop` and `docker start`, so a test
  can take a static host offline and bring it back. For "offline at start", the
  harness writes the port into `server.toml` and starts the container later.
- **Time.** Run the suite with `-timeout 60m`. Independent scenarios use
  `t.Parallel()`, each with its own server. At start, the harness removes Docker
  objects with a `hostel-e2e` label that are older than 2 hours (left by a crashed
  run), and stops `hostel ports` processes of earlier runs.
- Two providers in tests: `static` with fixed containers, and `external` with
  `test/fakeprovider`, which starts containers on demand. Environment variables
  control its delay (`FAKE_DELAY`), a limit (`FAKE_LIMIT`, counted over its
  containers, also ones without the prefix), failures (`FAKE_FAIL`), stop support
  (`FAKE_CAN_STOP`), and access through a proxy command (`FAKE_PROXY`). A lock
  file for each name makes `provision` idempotent when a killed server's call
  still runs.
- The harness gives a test server the test tools at its first start: it
  starts the server with the local keys of `server.toml`, pushes the tools
  (`hostel config tools push`), and then applies the providers and machines
  (`hostel config apply`). A test that checks the import of `server.toml`
  uses no test tools.
- The test tools in `test/tools/`: `fake-desktop` (a screen tool with
  `whole_machine = true`), `fake-sdk` (software; `check` prints a
  version) and `fake-screen` (a screen tool with `needs = ["fake-sdk"]` and the
  options `option-a`, `option-b`, `option-c`, `max_options = 2`; its scripts
  simulate a screen: they write
  `tool.env` and a fake screenshot), with scripts for `linux` and for `macos`.
  `fake-screen` reads markers in the machine cache: `slow-start`, `fail-start`,
  `slow-stop`, `fail-stop`. Tests create them with `docker exec` to drive
  failures and restarts at a known state. An `external` template with `os = "macos"` runs the `.macos.sh`
  scripts in a Linux container, so the tests cover script selection by OS.
- Tests use short durations (for example `idle_timeout = "3s"`). They wait for
  conditions with a deadline. They never use a fixed sleep as an assertion.
- `test/fakeprovider` under the name `devbox` (a link on the server's `PATH`) is a
  fake Namespace devbox CLI. It makes Docker machines and copies the output formats of
  the real CLI (the list JSON, the text line of an empty list, the error lines), so
  the E2E tests cover the `namespace` provider. Its machines set
  `NAMESPACE_DEVBOX_TASKS_DIR` for SSH sessions, as a real devbox does. As at
  Namespace, a devbox is in the list from the start of its create and boots on
  its own (`FAKE_DELAY`), also when the create call is killed; `ssh-proxy` fails
  until the boot ends. `FAKE_CREATE_ERROR` makes a create fail with a given
  text.
- Under the name `nsc`, it is a fake `nsc` CLI: an instance is a state file
  (destroyed when its `--duration` ends), `list` prints `null` when empty, a
  create blocks at `FAKE_LIMIT`, and `instance port-forward` prints `Listening on
  127.0.0.1:<port>`. Its `nsc ssh` runs the command with a shell and a `docker`
  shim (the same binary under the name `docker`), whose `run` starts the machine
  container from the test machine image. So the E2E tests run the provider's real
  start script.
- Real providers: opt-in smoke tests (`HOSTEL_SMOKE_NAMESPACE=1`) and manual tests
  on the spare MacBook.
- CI (GitHub Actions, Linux `amd64`): `make lint`, `make test-unit`, builds for all
  six targets, and one `make test-e2e` run. `make test-e2e` fetches only the Linux
  mise binaries; CI caches them.
- The stability gate (workflow "E2E gate", `.github/workflows/e2e-10.yml`): 10
  parallel `make test-e2e` jobs, without fail-fast. The label `e2e-10` on a pull
  request starts it.

## 21. Decisions in this design

1. All guest SSH goes to the gateway, which terminates it (sections 1 and 13).
   Guests never get a credential for a machine, so Hostel can end access at any time.
   There is no direct guest-to-machine access.
2. Configuration model: static providers contain machines, and on-demand providers
   contain templates; each machine or template lists tools (software and screen tools; mise tools or directories with scripts for each OS). The guest
   sees only tools and asks for the tools that it needs; a lease starts only the
   screen tools that it asked for (section 3; owner decision 2026-11-05).
3. Configuration format: TOML with strict decoding, defaults, a JSON Schema, and
   `hostel admin config check` (section 9).
4. The guest syncs files and starts the repository setup after `ready`. The server
   prepares only the machine and the lease's screen tools (section 10).
5. Warm policy: keep the toolchain and caches; always delete lease state; keep a machine
   running for `warm_running`, then stopped for `warm_stopped`; evict idle machines when
   a lease needs the capacity (sections 5 and 17).
6. Each lease gets clean screens from the screen tools' scripts (section 8). The method
   depends on the timing experiments in M2 and M4.
7. Version 1 calls the `devbox` and `nsc` CLIs on the server.
8. File sync uses manifests and `tar`, not `rsync`, so that it works on Windows
   (section 12).
9. Guests are trusted; version 1 uses one OS user on each machine (section 1).
