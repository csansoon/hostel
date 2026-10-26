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
                                                 room (slots, helper, devices)
```

- **Control path.** The guest lists platforms, creates a lease, waits, and releases
  it. The messages are small JSON documents over HTTP.
- **Data path.** The guest uses SSH to send files, run commands, and keep a reverse
  tunnel. All guest SSH traffic goes to the **gateway**, an SSH server inside the
  Hostel server.

Why all guest SSH goes to the gateway:

- Namespace rooms are reachable only through the Namespace SSH proxy, which needs a
  Namespace login. Guests must not have that login.
- A static room behind NAT (the home MacBook) cannot accept connections from a VPS
  or from cloud agents. It connects out to the gateway (section 13).
- When a lease leaves `ready`, the gateway closes all its connections at once.
- The gateway sees open sessions, so the server can measure guest activity.

**Access is always temporary and only through Hostel.** The gateway terminates the
guest's SSH connection and opens its own SSH connection to the room with the
server's credentials. The guest never holds a credential that the room accepts: the
lease key is valid only at the gateway, and only while the lease is `ready`. When
Hostel ends a lease (release, expiry, or an admin decision), access ends at once and
cannot come back without a new lease. This is true for every provider, including a
static VPS (section 13).

**Trust model.** Guests are the owner's own agents. Hostel protects against
accidents (state that goes from one lease to the next, forgotten machines, leaked
secrets in logs). Version 1 does not protect a room against a hostile guest
(section 18, "Backdoors"). In version 1 the guest and the server use the same OS user
on a room.

## 2. Binary and code layout

One Go binary, `hostel`, for darwin, linux, and windows on amd64 and arm64.

| Mode | Commands | Runs on |
|---|---|---|
| Guest | `hostel platforms`, `hostel acquire`, `hostel run`, ... | agent machines |
| Admin | `hostel admin ...` | any machine with an admin token; `config` commands run locally |
| Server | `hostel server init`, `hostel server run` | the owner's Mac, later a VPS |
| Helper | `hostel room ...` (hidden from help) | rooms; the server uploads it |

Rooms need no permanent daemon. Exception: a static room behind NAT runs
`hostel room connect` as a service (launchd, systemd, or a Windows service).

Dependencies. Use pure Go only, so that cross-compilation needs no C toolchain:

- `golang.org/x/crypto/ssh` (gateway, guest SSH, server-to-room SSH).
- `modernc.org/sqlite` (server state).
- `github.com/pelletier/go-toml/v2` (configuration, strict decoding).
- The standard library for HTTP, `log/slog`, `archive/tar`, and `os/exec`.

Add another dependency only when it removes a large amount of code.

Suggested layout:

```
cmd/hostel/            main: mode dispatch
internal/protocol/     API and helper request and response types
internal/config/       server, platform, repository, and guest configuration; defaults; schema
internal/server/       HTTP API, workers
internal/scheduler/    queue order and slot selection (pure functions)
internal/store/        SQLite schema, migrations, compare-and-set updates
internal/gateway/      SSH gateway
internal/provider/     Provider interface; static/, namespace/, external/
internal/roomops/      server-side operations on rooms (probe, upload, scripts, cleanup)
internal/guest/        CLI commands, local state, sync client, port process
internal/room/         helper commands
internal/syncfs/       manifests, sync plans, safe file writes
test/e2e/              E2E tests (build tag e2e)
test/fakeprovider/     external provider for tests; creates Docker rooms
test/roomimage/        Dockerfile for test rooms
test/platforms/        platform directories for tests
```

## 3. Configuration model

The owner configures two things: **providers** (with their **machines**) and
**platforms**. The guest sees only platforms.

### Providers and machines

- A **provider** is a source of machines, with its own settings: kind, login,
  account limits, and defaults for its machines.
- A **machine** is an entry inside a provider:
  - In a `static` provider, a machine is a real host. It gives exactly one room.
  - In an on-demand provider (`namespace`, `external`), a machine is a template. It
    gives up to its `max_rooms` rooms at the same time. The provider creates them when
    a lease needs one.
- A machine declares its OS (the `os` key, or from its `spec` for Namespace), the device
  platforms it serves, its labels, its machine-level mise tools, and its slots.
- Machine settings inherit from the provider and can override them. For maps
  (`env`, `tools`), the machine's keys are merged into the provider's keys and
  replace keys with the same name.
- A **room** is one concrete machine at runtime (a static host, or one machine that
  an on-demand provider created).

### Platforms

A platform is what a guest requests. There are two kinds:

| Kind | Names | Which machines serve it | Configuration |
|---|---|---|---|
| **OS platform** | `linux`, `macos`, `windows` | Every machine with that OS, automatically | None. An optional directory can add scripts and settings (section 8). |
| **Device platform** | owner-defined, for example `ios`, `android` | Machines that list it in `platforms` | A directory with `platform.toml` and scripts for each OS |

Tools such as Node, Java, or pnpm are not platforms. The repository's mise files
declare them, and Hostel installs them on any room (section 9, "mise on rooms").

## 4. Data model

The configuration (section 9) defines providers, machines, and platforms. The
database stores runtime state.

| Table | Main columns |
|---|---|
| `rooms` | id, name, provider, machine, provider_ref, state, os, arch, home, ssh_host_key, helper_sha256, provider_deadline, warm_until, drained, broken_reason, created_at, state_changed_at |
| `room_platforms` | room_id, platform, scripts_sha256, provisioned_at |
| `slots` | room_id, name, state, lease_id, cleanup_data |
| `leases` | id, token_id, began_at, state, revision, position, platform, devices, task, priority, required_labels, tunnels, fresh, public_key, guest_host, repo, worktree, provider, machine, room_id, slot_name, attempts, reason, created_at, last_poll_at, last_activity_at, ready_at, expires_at, ended_at, end_state, end_reason |
| `lease_tunnels` | lease_id, name, room_port |
| `tokens` | id, name, role, machine (for `room` tokens), sha256, created_at, revoked_at |
| `events` | id, time, lease_id, room_id, kind, message |

`rooms.home` is the room user's home directory from the probe. `leases.end_state`
is the final state that a lease in `cleaning` goes to. `leases.tunnels`
holds the requested tunnels (`[{"name", "room_port"}]`) until the server assigns
the room ports in `lease_tunnels`. Times are Unix milliseconds.

`room_platforms` records which platforms a room is ready for, and with which
version of the platform scripts. Jobs are not in the database. They exist only on
rooms (section 15).

**Transactions.** Each state change is a compare-and-set in one short SQLite
transaction (`UPDATE ... WHERE id = ? AND state = ?`). Never hold a transaction
during network I/O: read the state, do the I/O, then write the result with a
compare-and-set. Each worker step is idempotent, so a worker can repeat it after a
restart.

**Revision.** A change of a lease's state, reason, or queue position increments
`revision`. The scheduler stores `position` in the lease row and writes it only when
it changes. The estimated wait is computed at each response and does not change the
revision. Long polls use the revision (section 11).

### Lease states

```
queued ─► assigned ─► provisioning ─► preparing ─► ready ─► cleaning ─► final
   │          │             │              │          │
   │          └─────────────┴──────────────┴──────────┴──► cleaning (release, cancel, expiry, error)
   └──► final (cancelled before a slot was assigned)

assigned, provisioning, preparing ──► queued (room failure; section 6)
```

| State | Meaning |
|---|---|
| `queued` | Waits for a slot. |
| `assigned` | A slot is reserved for this lease. |
| `provisioning` | The room is in creation or resume, or the platform `provision` script runs. |
| `preparing` | The server checks the helper and scripts, writes the lease data, and runs the platform `prepare` script. |
| `ready` | The guest can connect. |
| `cleaning` | Cleanup runs (section 17). |
| final: `released`, `expired`, `cancelled`, `failed` | `end_reason` records why: guest release, idle, max duration, poll timeout, room lost, or error text. |

Rules:

- A lease with a slot always goes through `cleaning` before a final state.
- If the server has not yet called the helper `begin` command for the lease
  (`leases.began_at` is empty), cleaning only frees the slot. It does not call the
  helper, so a lease that ends while its room is still in creation does not break the
  room. The room continues its creation and becomes a warm room.
- Exception: if the room is lost (reconcile, section 7), cleanup is not possible.
  Leases on it in `ready` or `cleaning` go directly to `failed` with
  `end_reason = "room lost"`. Leases in `assigned`, `provisioning`, or `preparing` go
  back to `queued`.
- A lease that was not yet `ready` when its room failed goes back to `queued`
  (section 6, failures).

### Room states

| State | Meaning |
|---|---|
| `creating` | The row exists; the provider call runs. |
| `provisioning` | The server probes the room, uploads the helper and mise, and installs the machine-level tools. |
| `running` | Usable. Slots have their own states. |
| `stopping`, `stopped`, `resuming` | Warm stop and resume (providers that can stop). |
| `destroying`, `destroyed` | Removal. |
| `broken` | Not usable until repaired (static) or destroyed (on-demand). |
| `retired` | A static machine that was removed from the configuration. It takes no new lease. |

`drained` is a flag, not a state: a drained room takes no new lease.

### Slot states

`free` → `assigned` → `preparing` → `leased` → `cleaning` → `free`.

- A failed cleanup sets the slot to `broken` and the room to `broken`.
- **Dirty slot.** If a lease leaves a slot after `begin` (`began_at` is set) and
  cleanup cannot run now (the room is unreachable), the slot becomes `dirty` and
  records the lease ID, platform, devices, and ports in `slots.cleanup_data`. The
  room runs this cleanup before it takes a new lease: when a probe succeeds again,
  the server runs the cleanup of each dirty slot, then sets the room to `running`.
- When a lease returns to `queued`, its `began_at`, room, and slot are cleared. A new
  assignment calls `begin` again. Its old slot keeps no `lease_id` (a dirty slot has
  the lease data in `cleanup_data`), and every slot update for a lease is limited to
  the lease's current room, so a later step of the lease never changes the old slot.

An append-only `events` table records each change. `hostel status` and debug
output read from it.

## 5. Scheduling

The scheduler runs after each event (new lease, release, room ready, room failed)
and every `intervals.scheduler` (default 10 seconds). It is a pure function in
`internal/scheduler`: input = queued leases, rooms, slots, machines, limits, and
time; output = assignments and provider actions. Workers do the provider actions
after the pass. When an action fails in a way that changes the options (for example
a resume that returns "not found"), the worker updates the state and starts a new
pass at once. A unit test is allowed for the scheduler (see `AGENTS.md`).

**Effective priority.** `low` = 0, `normal` = 1 (default), `high` = 2. Effective
priority = `min(2, priority + floor(wait time / aging))`, with `aging` default 10
minutes. Order: effective priority (highest first), then request time. So an old
`low` lease can pass a newer `high` lease. This is intended: it prevents
starvation.

**Candidate machines.** A machine is a candidate for a lease if all of these are
true:

1. It serves the platform: for an OS platform, the machine has that OS; for a device
   platform, the machine lists it in `platforms`.
2. It has all required labels: the platform's `requires`, the repository's
   `requires`, and `--require`.
3. Its slots have at least as many pool ports as the lease has tunnels without a
   fixed `room_port`.
4. For `--fresh`: it is an on-demand machine.

The server checks the request at `POST /v1/leases`: the platform exists, each
requested device is in the platform's device list, the number of devices is at most
`max_devices`, and at least one candidate machine exists. Else the request fails at
once (R4).

**Assignment.** For each queued lease, in order, select the first possible option.
Inside each option, candidate machines are in configuration order (providers in file
order, then machines in file order):

1. a `free` slot in a `running`, not drained room that is ready for the platform
   (`room_platforms` is up to date) and has enough remaining provider lifetime
   (section 7, deadlines);
2. a `free` slot in a `running`, not drained room that is not yet ready for the
   platform (the platform `provision` script runs first);
3. a `stopped` room that the provider can resume;
4. a new room, if the machine and its provider are under their limits;
5. a new room after **eviction**: if each limit that blocks option 4 is full only
   because of Hostel rooms with no lease (`running` with all slots free, or
   `stopped`) in that limit's scope, destroy the oldest of them, then create a new
   room.

`--fresh` allows only options 4 and 5.

**Exclusive leases.** A lease is exclusive if its platform has `exclusive = true`,
or if it requires the label `desktop` (section 9, "The Mac desktop"). An exclusive
lease needs a room where all slots are free, and it reserves all of them.

If no option is possible, the lease **blocks** its candidate machines. A lease later
in the order cannot take a slot or a new room from a blocked machine. It can use
other machines. This prevents starvation and still lets other platforms continue.

**Limits.**

- A static machine always has one room.
- On-demand machine `max_rooms` (default 1) counts its rooms that are not
  `destroyed` (running, stopped, and broken).
- Provider `max_rooms` (default: no limit) counts all Hostel rooms of all its
  machines that are not `destroyed`.
- Provider `limits` count the rooms of a subset of its machines:
  `limits = [{ machines = ["mac", "linux-devbox"], max_rooms = 1 }]`. Use them for
  account limits that apply to some products only (for example "one devbox per
  user", which does not apply to instances).
- A provider "limit reached" error marks that **machine** as full. The flag clears
  when a Hostel room of that provider that existed at the provider is destroyed, or
  after `limit_retry` (default 2 minutes). The time-based retry is necessary because
  machines that Hostel did not create (for example the owner's own devbox) can use
  the limit.
- After a "limit reached" error, if the provider has Hostel rooms with no lease, the
  server evicts the oldest of them and tries again at once (not an attempt). This
  covers limits that Hostel cannot count, for example a vCPU limit.
- A `creating` row whose provider call failed (limit or other error) is set to
  `destroyed` after a `Destroy` by name (the provider returns `not_found` if nothing
  exists). This does not clear the full flag, because the room never existed at the
  provider.
- Room rows are never deleted. `rooms.id` uses SQLite `AUTOINCREMENT`, so an ID and
  its provider name are never used twice.

**Estimated wait.** Position in the queue for that platform × median lease duration
of the platform (from the last 50 leases) ÷ number of slots on candidate machines,
plus the median creation time when a new room is necessary. Show it as an estimate.

## 6. Failures

| Failure | Result |
|---|---|
| Provider "limit reached" | Machine full (section 5). The lease returns to `queued`. Not an attempt. |
| Other provider error at create or resume | Retry with backoff. **3 attempts in total**, then the lease fails with the provider message. |
| Room provisioning fails (probe, upload, machine-level tools) | The room becomes `broken`. The lease returns to `queued`. This is an attempt. |
| SSH connection to a room fails (dial or handshake), in any step | Not a script failure. A static room becomes `broken` with reason "unreachable"; its slot becomes `dirty` if the lease had called `begin` (section 4, slot states); the room recovers by itself when a later probe succeeds (each reconcile) and its dirty slots are cleaned; the lease returns to `queued` (not an attempt). An on-demand room is destroyed; the lease returns to `queued`, and this **is** an attempt, so a machine with a bad SSH setup cannot create rooms without limit. A lease that was `ready` fails with "room unreachable". |
| Platform `provision` script fails | The room becomes `broken`. The lease returns to `queued`. This is an attempt. |
| Platform `prepare` script fails | Run cleanup for the slot. The room becomes `broken`. The lease returns to `queued`. This is an attempt. |
| A lease reaches 3 attempts | The lease fails with the last error message. |
| Cleanup fails | Slot and room become `broken` (R24). |
| `mise install` or repository setup fails (guest side) | The lease stays `ready`. `acquire` exits with code 1 and prints the job ID and the end of its output. The agent can fix the code and run `hostel sync --setup`. |
| Room lost (reconcile) | Leases in `ready` or `cleaning` fail with "room lost". Leases in `assigned`, `provisioning`, or `preparing` return to `queued`. |
| Resume fails with "not found" | Mark the room `destroyed` and start a new scheduler pass at once. |

Broken on-demand rooms are destroyed. Broken static rooms stay until the owner runs
`hostel admin room repair <room> [--accept-host-key]`. Repair runs the full cleanup
(section 17) of each `broken` or `dirty` slot again (this also restores
`authorized_keys`), then the `health` scripts of the platforms that the room is
ready for, and sets the room to `running` if all
pass. It does this through the probe: a broken slot (with its `cleanup_data`)
becomes `dirty`, and the room goes to `provisioning`; the probe cleans each dirty
slot before the room is `running`.

**Workers and retries.** A room worker ends when a step does not change the room
state (for example a failed destroy); the next loop pass starts it again, so such
a retry follows `intervals.scheduler`. The provider `provision` call is retried
inside the worker with backoff (2 s, 4 s), three attempts in total.

**Room host key.** The server pins a room's SSH host key at the first connection. A
different key later makes the room `broken` with reason "host key changed". Only
`admin room repair --accept-host-key` accepts the new key (for example after the
owner reinstalls the machine).

## 7. Providers

```go
type Provider interface {
    Kind() string
    Capabilities(machine string) Capabilities // OnDemand, CanStop, CanExtend, MaxLifetime
    Provision(ctx context.Context, req ProvisionRequest) (RoomRef, error)
    Resume(ctx context.Context, room RoomRef) error
    Stop(ctx context.Context, room RoomRef) error
    Extend(ctx context.Context, room RoomRef, deadline time.Time) error
    Destroy(ctx context.Context, room RoomRef) error
    List(ctx context.Context) ([]RoomRef, error) // only rooms with this server's name prefix
    Access(ctx context.Context, room RoomRef) (RoomAccess, error)
}

type ProvisionRequest struct {
    Name      string         // hostel-<server_id>-<room id>; chosen before the call
    Machine   string         // machine name in the provider configuration
    Spec      map[string]any // the machine's spec
    Deadline  time.Time
    ServerKey string         // server's SSH public key (authorized_keys format)
}

type RoomAccess struct {
    User    string
    Dial    func(ctx context.Context) (net.Conn, error) // stream to the room's SSH server
    Signers []ssh.Signer                                 // keys the server uses
}
```

- `Dial` with a proxy command runs the command and uses its stdin and stdout as
  the connection. A proxy connection has no deadlines, so a timer bounds the SSH
  handshake.
- The server asks an external provider for the capabilities of each machine at
  start; a failure stops the start with the provider's message.
- `rooms.os` is the probed OS: it selects the helper and mise binaries. The
  machine's configured `os` selects the platform scripts; the server writes it to
  `room-env.json` (`"os"`), and the helper uses it for script names and
  `HOSTEL_ROOM_OS`. For static machines both must be the same.
- `Signers` is the server key for `static` and `external` rooms. For a Namespace
  devbox it is the key that the `devbox` CLI manages
  (`~/.namespace/ssh/<name>.devbox.namespace.key`): the devbox SSH server accepts
  only that key (M3, 2026-10-26).

| Kind | Machines | Provision | Access | Stop | Deadline |
|---|---|---|---|---|---|
| `static` | real hosts (`address`) | Not called. Each machine is a room from server start. | TCP to the host with the server key, or the `room connect` tunnel | no | none |
| `namespace`, `spec.product = "devbox"` | templates | `devbox create --name <Name> --ephemeral ...`; a devbox with the name that another call made (for example before a restart) is taken when it accepts SSH | proxy command `devbox ssh-proxy <Name>`, user `main_user` | no (ephemeral: a stop deletes it) | idle stop (it deletes the devbox); plan maximum duration (3 h on the Developer plan) |
| `namespace`, `spec.product = "instance"` | templates | `nsc create --duration ... --label hostel=<Name> --ssh_key ...` | verify in M4 | no | `--duration`; `nsc extend` |
| `external` | templates | the configured command | from the command's response | if the command declares it | the command must apply it |

Version 1 calls the `devbox` and `nsc` CLIs on the server. They must be installed
and logged in on the server machine. One `namespace` provider uses one login and
can mix devbox and instance machines.

**Machine OS.**

- `static`: the `os` key is required, so that `admin config check` and request
  validation work without a connection. At start, and at each reconcile while a
  room is `broken` with reason "unreachable", the server probes the host (section 8, "Probe and upload"). An
  unreachable host is `broken` with reason "unreachable" and becomes `running` when
  a probe succeeds. A probe result that differs from `os` makes the room `broken`.
- `namespace`: from `spec` (`platform` for a devbox, the prefix of `machine_type` for
  an instance).
- `external`: the `os` key is required. Platform matching and script selection use
  it. The helper binary follows the probe. (The E2E tests use this to run `.macos.sh`
  scripts in a Linux container.)

### Ownership and reconcile

- `server_id` must match `^[a-z0-9]{1,20}$` (no hyphen). Room IDs are integers.
- Lease IDs match `^[a-z0-9]{10,26}$` (for example a lowercase ULID). Provider,
  machine, platform, and device names match `^[a-z][a-z0-9-]*$`. No name contains
  `.`, so gateway user names (section 13) parse without ambiguity.
- Before a provider call, the server inserts the `rooms` row in state `creating`
  with a name it chooses: `hostel-<server_id>-<room id>`. It passes this name to
  the provider. Providers that cannot set a name use a label with the same value.
- **`Provision` is idempotent by name, also under concurrent calls.** After a
  server crash, an old provider process can still run when the restarted server calls
  `Provision` again. If a room with that name exists or is in creation, the provider
  waits for it or returns it. A "name already exists" error from the cloud counts as
  success: the provider then returns the existing room. It never creates a second
  room with the same name. (The external fake provider uses a lock file for each
  name.)
- After a restart, the worker calls `Provision` again for each room in `creating`.
- At start and every `intervals.reconcile` (default 5 minutes), reconcile calls
  `List` on each on-demand provider. It parses each name with
  `^hostel-([a-z0-9]+)-([0-9]+)$` and ignores names that do not match or that have
  another `server_id`. Then:
  - A listed room with no database row, or with a `destroyed` row: destroy it.
  - A database room in `creating`: skip it (its worker owns it).
  - Another database room that is not listed: mark it `destroyed` (room lost).
- A provider never acts on machines without the prefix. The owner uses Namespace for
  other work.

### Static machines

- At server start, each static machine gets a `rooms` row (if it has none). A machine
  that is no longer in the configuration becomes `retired`; its active lease
  continues, and the row stays `retired` after that lease ends.
- Reconcile never destroys a static room.
- Static rooms are provisioned for all their device platforms at server start, so the
  first lease is fast. On-demand rooms are provisioned for a platform when a lease
  for it first uses the room (option 2 in section 5).
- Configuration changes, including changes in platform directories, need a server
  restart in version 1. The server reads all configuration at start.

### Deadlines

- Machines that cannot stop (`instance`):
  - At creation and at each assignment, the deadline is at least
    `now + creation timeout + platform provision timeout + platform prepare timeout
    + max_lease + warm_running + 10 minutes`. The creation timeout is the provider's
    timeout for `provision` (external `timeouts.provision`; Namespace default 10
    minutes). The script timeouts are those of the lease's platform.
  - At `ready`, the server extends it to `ready_at + max_lease + warm_running + 10
    minutes` if that is later.
- Machines that can stop: the server destroys stopped rooms after
  `warm_stopped`. If the server is down, stopped rooms stay; reconcile destroys them
  after the restart if they are too old.
- Namespace devboxes are ephemeral (M3, 2026-10-26: a stopped macOS devbox keeps
  only its workspace volume, so a stop has no value). The provider-side bounds
  for compute cost (N2) are the idle stop (`--auto_stop_idle_timeout 15m`; it
  deletes an ephemeral devbox; measured: about 15 minutes after the last
  activity; 972 s for an idle ephemeral devbox) and the plan's maximum duration.
  While a job runs, its supervisor keeps the task marker
  `$NAMESPACE_DEVBOX_TASKS_DIR/hostel-<slot>.<job>`, so a long job without an SSH
  connection does not stop the devbox. Only jobs have a marker: a devbox that a dead
  server leaked still stops on idle when its jobs end. Between commands, a lease
  without jobs expires (`idle_timeout`, 10 minutes) before the idle stop.
  `max_lifetime` is required for a devbox machine.
- A room takes a new lease only if its remaining provider lifetime (if the machine
  has a `max_lifetime`, counted from the room's creation) is at least
  `max_lease + 5 minutes`. Else the server destroys it
  when it has no lease, and a new room is created.
- At config load, the server rejects a machine whose `max_lease + 5 minutes` is longer
  than its `max_lifetime`. The owner sets `max_lifetime` from the provider's plan (for
  example 3 hours for a devbox on the Namespace Developer plan).

### External provider contract

```toml
[providers.docker]
kind = "external"
command = ["/path/to/provider", "--flag"]
command_env = { DOCKER_HOST = "unix:///var/run/docker.sock" }
timeouts = { provision = "5m", default = "1m" }

[providers.docker.machines.small]
os = "linux"
spec = { image = "debian:13", cpus = 2 }
platforms = ["android"]
max_rooms = 2
```

The server runs `command... <operation>`, writes one JSON object to stdin, and reads
one JSON object from stdout. Exit code 0 = success. A non-zero exit code = failure;
stdout then contains `{"error": "limit" | "not_found" | "other", "message": "..."}`.
stderr goes to the server log at debug level. Provider commands must not print
secrets.

| Operation | Request | Response |
|---|---|---|
| `capabilities` | `{"machine"}` | `{"can_stop": bool, "can_extend": bool}`. External machines are always on demand. |
| `provision` | `{"name", "machine", "spec", "deadline" (RFC 3339), "server_key"}` | `{"id", "user", "ssh": {"host", "port"}}` or `{"id", "user", "proxy_command": [...]}`. Idempotent by `name`. |
| `resume`, `stop` | `{"id"}` | `{}` |
| `destroy` | `{"id", "name"}`; `id` can be empty, then the provider finds the room by `name` | `{}`, or the `not_found` error |
| `extend` | `{"id", "deadline"}` | `{}` |
| `list` | `{"prefix"}` | `{"rooms": [{"id", "name", "state"}]}` |
| `access` | `{"id"}` | same as the `provision` response |

`provision` must install `server_key` in the user's `authorized_keys`. The E2E tests
use this contract with Docker (`test/fakeprovider`).

## 8. Rooms: layout, helper, and scripts

### Layout

Paths are relative to the room user's home directory. On Windows, use the same
structure under `%USERPROFILE%`.

```
hostel/
  bin/hostel                 helper binary (hostel.exe on Windows)
  bin/mise                   mise binary (mise.exe on Windows; "mise on rooms" below)
  mise/config.toml           machine-level tools (MISE_CONFIG_DIR)
  room-env.json              provider, machine, and slot env and the MISE_* variables (mode 0600)
  platforms/<platform>/      platform scripts for this room's OS
  authorized_keys.base       authorized_keys at the first lease (restored at cleanup)
  cache/                     shared caches; kept between leases (HOSTEL_CACHE_DIR)
  slots/<slot>/
    work/                    the lease's files; a stable path for each slot
    baseline.git/            git directory for the baseline commits
    lease.json               lease ID, platform, devices
    device.env               KEY=VALUE lines that prepare writes (for example SIMULATOR_UDID)
    lease.env                KEY=VALUE lines that the guest sets (section 10)
    manifest.json            files of the last sync
    plans/<plan-id>.json     pending sync plans
    jobs/<job-id>/           output, exit, supervisor.json
```

The `work` path is the same for each lease in a slot. Caches that use the project
path as a key (for example Xcode DerivedData) are therefore reused.

### Probe and upload

1. The server runs `uname -s -m` on the room. If that fails, it runs
   `cmd /c echo windows %PROCESSOR_ARCHITECTURE%`. The result selects
   `hostel-<goos>-<goarch>` and `mise-<goos>-<goarch>` from `helper_dir` (Go names:
   `darwin`, `linux`, `windows`; `amd64`, `arm64`; `.exe` on Windows). The Hostel OS
   names in configuration and script files are `macos`, `linux`, and `windows`;
   `darwin` maps to `macos`.
2. The server compares SHA-256 hashes (from the helper `info` command) and uploads
   what differs. The hash of a file is the SHA-256 of its bytes. The hash of a
   platform directory is the SHA-256 of a text with one line `<path> <mode>
   <sha256>\n` for each included file, sorted by path. Included files are
   `platform.toml` and the scripts for the room's OS. Upload candidates: the helper, mise,
   `hostel/mise/config.toml`, `hostel/room-env.json`, and each platform directory that the room serves
   (`platform.toml` and the scripts for the room's OS; one hash for each directory,
   which is also `room_platforms.scripts_sha256`).
3. The server repeats step 2 at each `preparing`, so that a guest cannot leave a
   changed helper or script for the next lease.
4. Transfer. The helper itself goes first, with `sh -c 'cat > tmp && chmod 755 tmp
   && mv tmp hostel/bin/hostel'`. All other items go through the helper `install`
   command, which writes a temporary file, checks the SHA-256, and renames it. mise
   goes compressed with gzip (151 MB → about 52 MB for Linux). An OS platform without
   scripts for the room's OS has an empty upload, which removes old scripts.

### Helper commands

All helper commands take `--slot <name>` where it applies. Control commands read
one JSON object on stdin and write one JSON object on stdout. Exit code 0 =
success; else stdout has `{"error": "..."}`.

| Command | Caller | Input | Output |
|---|---|---|---|
| `info` | server | — | `{"os", "arch", "version", "hashes": {"helper", "mise", "mise_config", "room_env", "platforms": {"<name>": "<hash>"}}}` (empty string for a missing item) |
| `install mise\|mise-config\|room-env --sha256 H` / `install platform <name> --sha256 H` | server | gzip stream (mise), bytes, or a `tar` stream (platform) | `{}`; refuses data with another hash |
| `mise-install` | server | `{"timeout_seconds"}` | `{"exit_code", "output_tail", "timed_out"}`; installs the machine-level tools of `hostel/mise/config.toml` |
| `begin` | server | `{"lease_id", "platform", "devices"}` | `{}`. Writes `slots/<slot>/lease.json`. Saves the current `authorized_keys` as `authorized_keys.base` when no other slot of the room has a lease: the previous cleanup restored it, so it holds the owner's latest keys (for example a new server key). With a lease in another slot, it keeps the base, because `authorized_keys` can then have a key that that lease added. |
| `run-script <platform> <step>` | server, guest (`screenshot`) | `{"args": [...], "env": {...}, "timeout_seconds"}` | `{"exit_code", "output_tail", "missing", "timed_out"}`; `missing` when the platform has no script for this step and OS |
| `cleanup` | server | `{"lease_id", "platform", "devices", "ports", "health"}` (from the database, so a repeated cleanup does not need `lease.json`; `health` lists the platforms that the room is ready for) | `{"steps": [...]}` (section 17) |
| `set-env` | guest | `{"env": {"K": "V"}}` | `{}`. Writes `lease.env`. |
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
| `connect` | static room service | flags | runs until stopped (section 13) |

The `ready` lease data contains the absolute helper path, so the guest can call it.

### Platform directories

The server reads platforms from `platforms_dir` (default
`~/.config/hostel/platforms`). Each device platform has one directory; OS platforms
can have one.

```
platforms/
  ios/
    platform.toml
    provision.macos.sh   prepare.macos.sh   screenshot.macos.sh   reset.macos.sh   health.macos.sh
  android/
    platform.toml
    provision.linux.sh   prepare.linux.sh   screenshot.linux.sh   reset.linux.sh
    provision.macos.sh   prepare.macos.sh   screenshot.macos.sh   reset.macos.sh
  macos/
    screenshot.macos.sh  # desktop screenshot (optional OS platform directory)
```

`platform.toml`:

```toml
description = "iOS Simulator"
devices = ["iphone-17-pro", "ipad-pro-13"]   # the first is the default
max_devices = 2                              # default 1
requires = ["xcode-26.5"]                    # labels that each machine must have
exclusive = false                            # true: the lease takes the whole room
script_timeouts = { provision = "20m" }      # defaults: provision 20m, others 3m
```

**OS platform directories** (`linux/`, `macos/`, `windows/`) are optional. Their
`platform.toml` can have `description`, `requires`, `exclusive`, and
`script_timeouts`, but not `devices` or `max_devices`. All steps are optional. A
machine must not list an OS platform in `platforms`; the OS gives it automatically.
Each room has a `room_platforms` row for its OS platform too (with no scripts, the
row has an empty hash).

**Script files.** `<step>.<os>.sh` for `macos` and `linux`, `<step>.<os>.ps1` for
`windows`. The room's OS selects the file. A `.sh` or `.ps1` file with another
name is a configuration error (it catches typing errors such as `prepar.linux.sh`).
Other files in the directory are ignored and not uploaded. An OS platform directory
can only have scripts for its own OS. Steps:

| Step | When | Example for `ios` on macOS |
|---|---|---|
| `provision` | once for each room and platform, and again when the platform scripts change | check Xcode; create one golden simulator for each device in `devices` |
| `prepare` | each lease, before `ready` | clone the golden simulator of each requested device; boot it; write device facts to `device.env` |
| `screenshot` | `hostel screenshot`; `$1` is the output path, `$2` the device (default: the first) | `xcrun simctl io "$udid" screenshot "$1"` |
| `reset` | cleanup | stop the processes that use the slot's simulator clones (by UDID), then shut down and delete the clones |
| `health` | after cleanup, at `repair`, and before a warm room takes a lease; only for a platform that the room is ready for (on an on-demand room, after its first `provision`) | Xcode works; enough free disk |

Contract:

- Each step is optional, except `prepare` for device platforms: a device platform
  must have `prepare.<os>` for each OS of the machines that list it.
- Each step must be idempotent. A restart can run it again.
- `provision` and `health` run in `hostel/`. `prepare`, `screenshot`, and `reset` run
  in `slots/<slot>/`. The server creates `slots/<slot>/` before `prepare`.
- Environment: `HOSTEL_PLATFORM`, `HOSTEL_PLATFORM_DEVICES` (all devices in
  `platform.toml`, comma-separated), `HOSTEL_SLOT`, `HOSTEL_SLOT_DIR`,
  `HOSTEL_WORK_DIR`, `HOSTEL_CACHE_DIR`, `HOSTEL_LEASE_ID`, `HOSTEL_DEVICES` (the
  lease's devices, comma-separated; `prepare`, `screenshot`, `reset`), the `MISE_*`
  variables, the provider, machine, and slot `env`, and `device.env` (for
  `screenshot` and `reset`). Machine `env` is also the way to vary a script between
  machines with the same OS. Scripts get `HOSTEL_LEASE_ID`, not `HOSTEL_LEASE`: device
  processes that `prepare` starts are therefore not stopped by cleanup step 1, and
  `reset` can shut them down cleanly.
- **Device facts.** `prepare` writes them to `device.env`. Each platform documents
  its variable names. The `ios` scripts write `SIMULATOR_UDID` (first device) and
  `SIMULATOR_UDIDS` (all, comma-separated). The `ios` scripts (`platforms/ios`)
  act only on simulators with the name prefix `hostel-`: the golden simulators
  `hostel-golden-<device>-<iOS version>` (from `provision`) and the lease's
  simulators `hostel-<slot>-<device>`. The machine `env` value `HOSTEL_IOS_RUNTIME`
  (for example `26.5`) selects the runtime; the default is the newest iOS runtime.
  The `android` scripts write
  `ANDROID_SERIAL` and `ANDROID_SERIALS`.
- Before a platform `provision` script, the server runs `mise install` for the
  machine-level tools (helper `mise-install`, at each room provisioning), so
  scripts can use them: the helper adds `hostel/bin` and the output of
  `mise env --json` (in the script's directory) to the script environment.
- A non-zero exit code is a failure. The last 4 KB of output go to the event log.
- The helper runs a script with `/bin/sh <script>` (so the file needs no execute
  bit) in its own process group, and stops the group at the step timeout. A
  background process that the script starts (for example a simulator) can keep
  running after the script ends.

`hostel admin config check` (section 9) fails if a device platform lacks `prepare`
for an OS that serves it, if a machine lists an unknown platform or an OS platform,
or if an OS platform directory declares devices.

## 9. Configuration files

All configuration uses TOML: the same format as `mise.toml`. The server decodes
strictly: an unknown key or a wrong type is an error with the file name and line.
Durations are strings with a unit (`"10m"`); a number is a type error. In
`server.toml`, a path that starts with `~/` is relative to the home directory, and
another relative path is relative to the directory of `server.toml`.

### Server (`server.toml`)

`hostel server init` writes a commented template, a gateway host key, the server's
own SSH key pair (`data_dir/server_ed25519`), and an empty `platforms_dir`. It
creates the first admin token in the database and prints it once. If the guest
config does not exist on the same machine, it writes it with this admin token (the
owner's own machine). `hostel admin pubkey` prints the server's public key.

```toml
#:schema ./hostel.schema.json
server_id = "sansmac"               # [a-z0-9]{1,20}; part of provider resource names

[providers.home]
kind = "static"

[providers.home.machines.macbook]
address = "hostel@macbook.local"    # user@host[:port]; default user hostel, port 22
os = "macos"                        # required for static machines
platforms = ["ios", "android"]      # device platforms; "macos" is automatic
labels = ["xcode-26.5", "desktop"]

[providers.namespace]
kind = "namespace"
max_rooms = 3                       # limit for all its machines
warm_running = "5m"                 # default for its machines
# limits = [{ machines = ["mac"], max_rooms = 1 }]   # for example "1 devbox per user"

[providers.namespace.machines.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
platforms = ["ios"]
labels = ["xcode-26.5", "desktop"]
# tahoe-slim has several Xcode versions; select one.
env = { DEVELOPER_DIR = "/Applications/Xcode_26.5.app/Contents/Developer", HOSTEL_IOS_RUNTIME = "26.5" }
max_rooms = 1
max_lifetime = "3h"                 # required for a devbox: the plan's maximum duration

[providers.namespace.machines.linux]
spec = { product = "instance", machine_type = "linux/amd64:8x16" }
platforms = ["android"]
labels = ["kvm"]
max_rooms = 2
```

Server-level keys and defaults:

| Key | Default |
|---|---|
| `listen` | `127.0.0.1:7410` (HTTP API) |
| `gateway_listen` / `gateway_public` | `127.0.0.1:7422` / same as `gateway_listen` |
| `data_dir` | `~/.local/share/hostel` |
| `helper_dir` | `<data_dir>/helpers` (`hostel-<goos>-<goarch>` and `mise-<goos>-<goarch>`, section 8) |
| `platforms_dir` | `~/.config/hostel/platforms` |
| `idle_timeout`, `aging`, `poll_timeout`, `limit_retry` | `10m`, `10m`, `5m`, `2m` |
| `port_process_offline` | `10m` (section 14) |
| `intervals` | `{ scheduler = "10s", reconcile = "5m", tunnel_check = "30s" }` (short values in tests) |
| `[mise]` | `idiomatic_tools = ["node", "python"]` (also when `[mise]` sets only other keys; `[]` turns idiomatic files off); `github_token` unset; `agent_device = true` |

Provider and machine keys. A machine inherits only these provider keys: `tools`,
`env`, `max_lease`, `max_lifetime`, `warm_running`, `warm_stopped`. `max_rooms` is
not inherited: on a provider it is a limit for all machines together.

| Key | Where | Default |
|---|---|---|
| `kind` | provider | required: `static`, `namespace`, `external` |
| `max_rooms` | provider: limit for all its machines; on-demand machine: its own limit | provider: none; machine: 1 |
| `limits` | provider | `[]`; each entry `{ machines = [...], max_rooms = N }` |
| `max_lifetime` | provider, machine | none (the provider's maximum room lifetime) |
| `address`, `connect` | static machine | `connect = false` (true: the host is behind NAT; section 13) |
| `os` | machine | required for `static` and `external`; from `spec` for `namespace` |
| `spec` | on-demand machine | required |
| `platforms` | machine | `[]` (OS platform only) |
| `labels` | machine | `[]` |
| `tools` | provider, machine | `{}` (machine-level mise tools, for example `{ java = "temurin-21" }`) |
| `slots` | machine | one slot `a`; slot `n` (from 0) gets ports `7500 + 10n` to `7509 + 10n` |
| `env` | provider, machine, slot | `{}` |
| `max_lease` | provider, machine | `2h` |
| `warm_running`, `warm_stopped` | provider, machine | `5m`, `24h` (on-demand only) |
| `command`, `command_env`, `timeouts` | external provider | `command` required; `timeouts = { provision = "5m", default = "1m" }` |

Explicit slots: `slots = [{ name = "a", ports = [7500, 7501], env = { ANDROID_SERIAL = "emulator-5554" } }]`.
From M6, a slot can have its own OS `user`.

**Commands for the owner:**

- The server reads `~/.config/hostel/server.toml` by default; `--config` selects
  another file.
- `hostel admin config check [--config server.toml]` validates `server.toml` and all
  platform directories, and prints the effective configuration with all defaults.
  The server runs the same check at start and refuses to start on an error.
- Error positions: TOML syntax errors, type errors, and unknown keys give the file,
  line, and column (from the decoder). Semantic errors (for example an unknown
  platform) give the file and the key path, for example
  `server.toml: providers.home.machines.macbook.platforms[1]: unknown platform
  "andriod"`. A missing script names the platform directory and the missing file.
- `hostel admin config schema` writes `hostel.schema.json` (JSON Schema from the Go
  types). Editors use it through the `#:schema` line for completion and checks.
  Generate it with a small reflection over the configuration structs. If that code
  grows beyond about 200 lines, `github.com/invopop/jsonschema` is an allowed
  dependency.

### Repository (`hostel.toml`)

The repository declares what it needs. The file goes to the room with the other
files. Sections are keyed by platform name.

```toml
include = [".env"]                  # extra files that git ignores; glob patterns

[tunnels.api]                       # room → guest (section 14)
local_port_env = "PORT"

[platforms.ios]
devices = ["iphone-17-pro"]         # default devices for this repository
# requires = ["xcode-26.5"]         # labels that this repository needs on this platform
setup = ["cd apps/ios && tuist install && tuist generate --no-open"]
env = { EXAMPLEAPP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }

[platforms.android]
env = { EXAMPLEAPP_API_BASE_URL = "http://10.0.2.2:{tunnels.api.port}" }

```

- The platform's `requires`, the repository platform `requires`, and `--require`
  are merged. `--device` replaces `devices`.
- The setup job always starts with `mise install` in `work/` ("mise on rooms" below).
  Then the `setup` commands run in order, with `sh -c` (or PowerShell on Windows):
  the setup job is `mise install && PATH="$(mise bin-paths | tr '\n' ':')$PATH" &&
  (command 1) && (command 2)`. The `PATH` step is necessary because the job
  environment comes from `mise env` at the job start, before this `mise install`
  (found in M2). The first failure stops the job.
- `hostel admin config check --repo hostel.toml` validates a repository file.

**Caches.** mise installs, package stores, DerivedData, and Gradle caches are outside
`slots/<slot>/work`. Cleanup does not delete them.

### The Mac desktop (Mac Catalyst)

A Mac Catalyst app runs on the macOS desktop, not in a simulator. Rules:

- It uses the OS platform `macos`. The guest requests the desktop for each lease:
  `hostel acquire macos --require desktop`. A repository must not put `desktop` in
  its `requires`, because then every `macos` lease of that repository is exclusive.
- The owner gives the label `desktop` only to machines with a logged-in graphical
  session: a static Mac with automatic login, or a Namespace devbox (it has a
  desktop).
- A lease that requires `desktop` is exclusive (section 5): two leases must not
  share one desktop. A `macos` lease without `desktop` (for example a headless
  build) is not exclusive.
- `platforms/macos/screenshot.macos.sh` uses `screencapture` on the room. Over SSH,
  macOS can refuse this without the Screen Recording permission. On the static Mac
  it needs the permission for `sshd-keygen-wrapper` (M2); on a devbox it works
  without a prompt (M3, 2026-10-26).

### mise on rooms

Hostel uses [mise](https://mise.jdx.dev) to install the package managers,
compilers, and runtimes on rooms. Rooms do not need mise before Hostel uses them,
and Hostel does not use another mise that the room has.

- **Binary.** The server uploads a pinned mise binary to `hostel/bin/mise`. The pin
  is `MISE_VERSION` in the `Makefile`. `make dist` downloads the pinned version for each OS and architecture into `dist/`
  (point `helper_dir` at it) and verifies the release checksums. The pin is in the
  Hostel repository, so a Hostel release fixes the mise version.
- **Two levels of tools.**
  - Machine level: `tools` in the provider or machine. The server writes them to
    `hostel/mise/config.toml` and installs them when it provisions the room.
  - Repository level: the repository's own `mise.toml`, `.mise.toml`, or
    `.tool-versions`, and idiomatic files (`.nvmrc`, `.python-version`). The setup
    job runs `mise install` in `work/` first. Repository versions override machine
    versions (normal mise precedence).
- **What mise does not install.** Xcode, simulator runtimes, Android system images,
  and other heavy toolchains come from the machine image or the platform `provision`
  script. Machines declare them as labels.
- **Environment.** All room-side processes of Hostel (scripts, jobs, shells) get:

  | Variable | Value |
  |---|---|
  | `MISE_DATA_DIR` | `hostel/cache/mise` (installed tools; kept between leases) |
  | `MISE_CACHE_DIR` | `hostel/cache/mise-cache` |
  | `MISE_CONFIG_DIR` | `hostel/mise` |
  | `MISE_STATE_DIR` | `hostel/mise-state` |
  | `MISE_TRUSTED_CONFIG_PATHS` | `hostel/slots` (the repository config is trusted without a prompt) |
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
- **agent-device.** Each room gets [agent-device](https://github.com/callstack/agent-device)
  (owner decision, 2026-10-23): agents use it to tap, type, and read the UI of
  devices on the room. Hostel pins its version and the Node version that runs it
  (`internal/config/agentdevice.go`; T3 Code pins the same version). At
  provisioning, `mise-install` installs both and writes `hostel/bin/agent-device`
  (`agent-device.cmd` on Windows), which runs `mise exec node@<pin>
  npm:agent-device@<pin> -- agent-device`. The job `PATH` therefore gets no Node, and
  a repository's Node pin does not change it. Jobs and shells get
  `AGENT_DEVICE_STATE_DIR=slots/<slot>/agent-device`: each slot has its own daemon
  and sessions, and cleanup deletes them with the slot. The iOS runner build stays
  in `~/.agent-device/apple-runner` and is reused by later leases. The server
  option `mise.agent_device = false` turns it off.

### Guest (`~/.config/hostel/config.toml`)

```toml
server = "http://127.0.0.1:7410"
token = "..."
```

`HOSTEL_SERVER` and `HOSTEL_TOKEN` override the file.

## 10. Guest flow

### Local state

- Directory: `$(git rev-parse --git-dir)/hostel/` (the per-worktree git directory),
  one subdirectory for each platform. It holds the lease ID, the private key, the
  pinned gateway host key, the port process PID and control socket, and the step that `acquire` finished last.
- A platform has at most one active lease in a worktree.
- Commands select the lease with `--platform`. If the worktree has only one active
  lease, they use it.

### `hostel acquire <platform> --task "..."`

The command is resumable. Each step records its result in the local state.

1. If the state has a lease that is not final, continue it. If `--task`,
   `--priority`, `--device`, or `--require` differ, print a warning and continue the
   existing lease (release it first to change them). If the stored lease is final,
   start a new one.
2. Read `hostel.toml`. Generate an ed25519 key pair for the lease.
3. `POST /v1/leases` with platform, devices, task, priority, required labels,
   tunnels (names and fixed room ports), `fresh`, public key, repository name,
   worktree path, and host name.
4. Long-poll `GET /v1/leases/{id}?since=<revision>&wait=50s`. Print one line for
   each change: state, position, reason, estimated wait.
5. When the lease is `ready`: compute the environment (below) and call `set-env`.
6. Start the port process and wait until all tunnels are confirmed (section 14).
7. Sync the files (section 12).
8. Start the setup job and attach to it.
9. Print a summary: platform, devices, provider and machine, work directory,
   environment, expiry, and the next commands.

If `--wait` (default 100 seconds) ends before step 9, the command prints
`hostel: not finished; run the same command again` and exits with code 75.

### Environment of lease commands

The helper builds the environment of each job and shell in this order (a later
source overrides an earlier one):

1. the room user's login environment;
2. the provider, machine, and slot `env`, and the `MISE_*` variables (section 9);
3. the output of `mise env --json` in `work/` (tool paths and the repository's
   mise `[env]`);
4. `device.env` (from `prepare`);
5. `lease.env`: the repository platform `env` after placeholder substitution by
   the guest, plus `HOSTEL_TUNNEL_<NAME>_PORT` for each tunnel;
6. `HOSTEL_LEASE`, `HOSTEL_PLATFORM`, `HOSTEL_DEVICES`, `HOSTEL_SLOT`,
   `HOSTEL_WORK_DIR`, `HOSTEL_CACHE_DIR`, `HOSTEL_ROOM_OS`, `HOSTEL_ROOM_ARCH`, and
   `AGENT_DEVICE_STATE_DIR` (section 9, "mise on rooms"). The helper reads the lease
   data from `slots/<slot>/lease.json`.

`{tunnels.<name>.port}` is the room port of the tunnel (section 14). Tunnels without
a fixed `room_port` get ports from the slot's pool in name order.

### Guest commands

| Command | Effect |
|---|---|
| `hostel platforms` | Each platform: kind, description, devices, free slots, queue length, estimated wait (R1). |
| `--platform <name>` | On all lease commands below: selects the lease when the worktree has more than one. |
| `hostel acquire <platform> --task ... [--device d]... [--priority low\|normal\|high] [--require label]... [--fresh] [--wait 100s]` | Creates or continues a lease. |
| `hostel sync [--setup]` | Sends the changed files again. `--setup` runs the setup job again. |
| `hostel run [--detach] [--wait 100s] [--json] -- <command>` | Starts a job in the work directory and streams its output. One argument runs with `sh -c` (`hostel run -- "make && make test"`); several arguments run without a shell. The command is found in the job's PATH (with the mise tools). |
| `hostel attach <job> [--wait 100s]` / `hostel jobs` / `hostel stop <job>` | Job control. |
| `hostel screenshot [--device d] [-o file.png]` | Runs the platform `screenshot` script and downloads the PNG. |
| `hostel cp <from> <to>` | Copies files with `tar-out` or `tar-in`. A `:` prefix marks a path in the work directory. The target is the full new path; a target that ends with `/` (or an existing local directory) receives the source with its own name. Both sides refuse to write below a symbolic link and replace a link at a file path instead of following it. |
| `hostel diff` | Shows the temporary changes against the last baseline commit. |
| `hostel ssh [-- command]` | Interactive shell, or one command, with the system `ssh` client and the file of `ssh-config`. |
| `hostel ssh-config` | Writes an `ssh_config` file for `ssh -F` (gateway address, lease user, lease key, pinned gateway host key) and prints its path. |
| `hostel tunnel list` | Lists the tunnels with their local and room ports. |
| `hostel tunnel add <name> (--local-port N \| --local-port-env VAR) [--room-port P]` / `tunnel set <name> [--local-port N] [--room-port P]` / `tunnel remove <name>` | Changes tunnels during a lease (section 14). |
| `hostel forward <room-port> [--local-port N]` / `forward list` / `forward stop <room-port>` | Guest → room forwards (section 14). |
| `hostel status` | All leases, rooms, and the queue. |
| `hostel release` | Ends the lease (or cancels it if it is still queued) and stops the port process. |
| `hostel agent-guide` | Prints short usage instructions for coding agents. |
| `hostel screenshot [--device d] [-o file.png]` | (see above) The script writes to a file in the slot directory; the guest downloads it, deletes it on the room, and prints the local path. |

Admin commands: `hostel admin token create|revoke`, `admin pubkey`,
`admin lease release <lease-id>`, `admin room drain|undrain|repair|destroy <room>`,
`admin config check|schema`. `hostel status` shows lease and room IDs.

All guest commands except `platforms`, `status`, and `agent-guide` call
`POST /v1/leases/{id}/touch`. When a lease command fails (for example because
the gateway closed the connection at expiry), the guest calls `touch` again; if
the lease ended, it reports the end reason instead of the connection error. An
attach that cannot connect again stops at once when the lease ended.

Example:

```
$ hostel platforms
PLATFORM  KIND    DEVICES                     FREE  QUEUE  EST. WAIT
ios       device  iphone-17-pro, ipad-pro-13  1     0      now
android   device  pixel-9-api-35              0     1      ~6 min
macos     os      -                           1     0      now
linux     os      -                           -     0      ~1 min (new room)

$ hostel acquire ios --device ipad-pro-13 --task "QA the sidebar on iPad"
$ hostel acquire linux --task "run the API test suite"
```

### Exit codes and output

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | Error. The message starts with `hostel:`. |
| 2 | Usage error. |
| 69 | No machine can satisfy the request, or an unknown platform or device (R4). |
| 75 | Not finished. The last line on stderr names the command to run next. For `acquire` it is the same command; for `run` and `attach` it is `hostel attach <id>`. Never run `hostel run` again for this reason: that starts a second job. |
| other | `run` and `attach`: the exit code of the remote command. |

Because a remote command can also exit with 1, 2, 69, or 75, `run` and `attach`
always print a last line on stderr: `hostel: job <id> exited with code N`,
`hostel: job <id> is still running; run "hostel attach <id>"`, or
`hostel: job <id> was lost`. Agents must read that line. `--json` prints one JSON
object on stdout with an explicit `status` field.

Progress goes to stderr. The final result goes to stdout.

## 11. Server API

All requests use `Authorization: Bearer <token>`. JSON in and out. A guest token
can read and change only the leases that it created. An admin token can do
everything. Any token can read `status` and `platforms`.

| Method and path | Effect |
|---|---|
| `GET /v1/info` | Server version, gateway address, gateway host key, server SSH public key (for `hostel admin pubkey`). |
| `GET /v1/platforms` | Platforms: name, kind, description, devices, `max_devices`, free slots, queue length, estimated wait. |
| `POST /v1/leases` | Creates a lease. Returns its ID, state, and revision. HTTP 422 with `{"code": "unknown_platform" \| "unknown_device" \| "too_many_devices" \| "no_machine"}` for R4. |
| `GET /v1/leases/{id}?since=R&wait=50s` | Returns the lease when its revision is greater than `R`, or after `wait` (at most 60 s and half of `poll_timeout`, so the guest polls again before the timeout). Updates `last_poll_at` when the token is the lease owner (an admin can look without keeping a lease alive). |
| `POST /v1/leases/{id}/touch` | Records guest activity. HTTP 409 with `{"code": "lease_ended"}` and the end reason for a lease that ended; the guest then marks its local state as final. |
| `PUT /v1/leases/{id}/tunnels` | Registers the lease's tunnels: `[{"name", "room_port"}]` (`room_port` 0 = select from the slot pool). Returns the room port of each tunnel, or HTTP 409 for a port that is not available. |
| `POST /v1/leases/{id}/release` | Releases or cancels. |
| `GET /v1/status` | Leases, rooms, queue. |
| `POST /v1/rooms/{id}/drain`, `/undrain`, `/repair`, `/destroy` (admin) | Room control. `repair` takes `{"accept_host_key": bool}` and is for broken static rooms; `destroy` is for on-demand rooms without a lease. |
| `POST /v1/tokens`, `DELETE /v1/tokens/{id}` (admin) | Token control. Request: `{"name", "role", "machine"}` (`machine` = `<provider>/<machine>`, only for `room`). A new token is returned once. |

A `ready` lease includes the access data: gateway address, gateway host key,
gateway user, helper path, slot name, work directory, tunnel ports, provider and
machine names, room OS and architecture, `tunnel_check`, `expires_at`, and
`idle_expires_at`.

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
   - `overwrite`: paths in `send` that a room command changed after the last sync.
     It also lists room objects that block a `send` path (step 2 of
     `sync-apply`).
   The helper stores the plan and the new manifest in `plans/<plan-id>.json`.
5. The guest sends a `tar` stream with exactly the `send` paths to
   `sync-apply <plan-id>`. The helper applies the plan in this order:
   1. Delete the `delete` paths, deepest first. Then remove directories that became
      empty, up to `work/`.
   2. For each `send` path: if any existing parent component is not a directory (a
      file or a symbolic link), or the path itself has another type (for example a
      directory where a file must go), remove that object first, but only if the
      plan lists it in `overwrite`; else stop with an error (a room job made it after
      the plan).
   3. Write the files, and create missing parent directories.
   4. Replace `manifest.json` and make a baseline commit.
6. `hostel sync` prints the `overwrite` paths.

Files that room commands created (build output, generated projects) are never in
`delete`, so they stay.

### Path safety

The helper must not write or delete outside `work/`:

- Reject absolute paths, `..` components, and paths with `\` or NUL.
- Before each write or delete, check each parent directory with `Lstat`. Reject the
  operation if a parent is a symbolic link.
- Write a symbolic link as a link. Never follow it.
- Delete only paths that the old manifest lists, and room objects that block a
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
- If `git` is not on the room, the helper skips the baseline and `diff` reports that
  it is not available.
- A build step that needs git metadata (`git describe`) does not work in `work/`.
  Check this for ExampleApp in M2.

This design works on all room OSes without `rsync`.

## 13. Gateway and access

### Principle

The guest connects only to the gateway. The gateway is a full SSH server and SSH
client (`golang.org/x/crypto/ssh`), not a relay of encrypted bytes:

```
guest ──SSH (lease key)──► gateway ──SSH (server key)──► room
```

- The lease key exists only on the guest (private part) and in the server database
  (public part). It is never installed on a room.
- The room's `authorized_keys` contains only the server key (and the provider's own
  key, if any). Cleanup restores it from `authorized_keys.base`, so a key that a
  guest added during a lease is removed.
- The server verifies the room host key (recorded at the first connection). The
  guest verifies only the gateway host key.
- Because the gateway terminates SSH, it sees the plaintext of the sessions. This is
  acceptable: the gateway is the owner's own server.

### Gateway behavior

- **Gateway host key.** `hostel server init` generates it. Guests get it from
  `GET /v1/info` over the authenticated API and pin it.
- **Gateway user.** `<lease-id>.<purpose>`. Purpose is `cmd` (commands, sync,
  shell) or `ports` (the port process; section 14). The gateway accepts only the lease's public key and only while
  the lease is `ready`.
- **Upstream connection.** For each guest connection, the gateway opens one SSH
  connection to the lease's room with `RoomAccess` (dial and server signers).
- **Sessions.** The gateway supports `exec`, `shell`, `pty-req`, `env`,
  `window-change`, `signal`, and `exit-status`. It wraps each session in the
  helper: `shell` becomes `hostel room shell --slot <slot>`, and `exec <string>`
  becomes `hostel room exec --slot <slot> --command-b64 <base64 of the string>`. The
  helper decodes the string and runs it with `sh -c` (macOS, Linux) or
  `powershell -NoProfile -Command` (Windows). Base64 avoids all quoting problems.
  Guest calls of the helper are wrapped the same way; there is no exception. So every
  guest process has the lease environment and `HOSTEL_LEASE`, and cleanup can find
  it (section 17).
- **Port forwarding.** `direct-tcpip` from the guest goes to the same port on the
  room's loopback interface. `tcpip-forward` from a `ports` connection is sent to
  the room only for room ports that the lease registered, and the gateway relays the
  `forwarded-tcpip` channels back (section 14). Forwarding is allowed only to and
  from `127.0.0.1` on the room.
- **End of access.** When a lease leaves `ready` for any reason, the gateway closes
  all guest connections and upstream connections of that lease at once, before
  cleanup starts. An admin can end a lease at any time with
  `POST /v1/leases/{id}/release`.

### Network rule for rooms

The gateway can enforce access only if the room accepts SSH from nothing else.

| Provider | How the rule holds |
|---|---|
| `namespace` | The room is reachable only through the Namespace proxy, which needs the server's Namespace login. |
| `static`, behind NAT (`connect = true`) | The room has no inbound SSH from the internet. It connects out to the gateway. |
| `static`, public (VPS) | The owner must allow SSH only from the server's address (firewall), or use `connect = true` and close inbound SSH. `plan.md` (M5) includes this setup. |
| `external` | The provider must make the room reachable only from the server. |

Even if a room breaks this rule, a guest has no key that the room accepts. The rule
is defense in depth.

### Server access to rooms

- Static machines: the owner adds the server public key once (`hostel admin pubkey`).
- External machines: the provider installs `server_key`.
- Namespace: `RoomAccess` from the provider.
- **Rooms behind NAT.** `hostel room connect --gateway <address> --token <token>`
  keeps an outbound SSH connection to the gateway (user
  `room.<provider>.<machine>`, a token with the role `room` as the password). A
  `room` token is bound to one machine when it is created
  (`hostel admin token create --role room --machine home/macbook`); the gateway
  refuses it for another machine. The gateway uses that connection to
  open streams to the room's local SSH server. The `static` provider's `Dial` uses
  it for machines with `connect = true`.

## 14. Port forwarding

Two directions, both TCP only (HTTP, WebSocket, gRPC; no UDP):

| Direction | Name | Use |
|---|---|---|
| Room → guest | **tunnel** (reverse forward) | The app on the room reaches the API on the guest. |
| Guest → room | **forward** (local forward) | The guest reaches a server on the room (for example a bundler or a debugger). |

### Names

Tunnel names match `^[a-z][a-z0-9-]*$`. The environment variable of a tunnel is
`HOSTEL_TUNNEL_<NAME>_PORT`, with the name in upper case and `-` replaced by `_`.

### Tunnels (room → guest)

A tunnel has a name, a **local port** (on the guest) and a **room port** (on the
room's loopback interface).

- **Declaration.** In `hostel.toml` (section 9), or at runtime with `hostel tunnel
  add`:

  ```toml
  [tunnels.api]
  local_port_env = "PORT"   # or: local_port = 3000
  # room_port = 3000        # optional; default: a port from the slot's pool
  ```

- **Local port.** In this order: an explicit `local_port` (or `--local-port`);
  else the variable `local_port_env` in `.env` in the repository root, read again
  for each incoming connection; else the same variable in the environment of
  `hostel acquire`. So a guest can change its API port at any time (for example
  restart the API on another port, or `hostel tunnel set api --local-port 4001`).
  The next connection goes to the new port. No restart is necessary. The guest
  connects to `127.0.0.1:<port>` first and then to `[::1]:<port>`, because a local
  server can listen on either loopback address.
- **Room port.** A tunnel can request a fixed `room_port` (1024 to 65535), for
  example when an app has a hardcoded address. Else the server gives the lowest free
  port from the slot's pool (10 ports for each slot; section 9). At lease creation,
  the server gives pool ports to the tunnels in name order. The room port stays the
  same for the whole lease unless the guest changes it, so a value that a build
  embeds (for example `EXAMPLEAPP_API_BASE_URL`) stays valid.
- **Assignment and registration.** `POST /v1/leases` contains the tunnels
  (`[{"name", "room_port"}]`, `room_port` 0 = from the pool). The scheduler uses
  them for candidate matching (section 5). The server records the room ports in
  `lease_tunnels` when it assigns the slot, and returns them with the `ready` lease.
  `PUT /v1/leases/{id}/tunnels` changes the list during a lease (and raises the
  lease revision). The server refuses a port that another lease on the room uses
  (HTTP 409), and two tunnels with the same fixed room port (also in `hostel.toml`).
  The gateway accepts a remote forward only for a registered port of that lease, and
  after each change it closes the forwards of ports that the lease no longer
  registers. At assignment, a lease does not get a room where another lease uses one
  of its fixed room ports; it waits. From M6, the scheduler treats fixed room ports as
  a resource of the room (today such a lease can wait for that room although another
  room is free).
- **Changing the room port.** `hostel tunnel set <name> --room-port P` registers the
  new port, closes the old forward, and opens the new one. The guest updates
  `lease.env`, so new jobs get the new `HOSTEL_TUNNEL_<NAME>_PORT` and the new values
  of the repository `env`. Running jobs keep the old values. The command warns that
  a build that embedded the old address must be built again.
- iOS simulators use the room's `localhost`. Android emulators reach the room's
  `localhost` at `10.0.2.2` on every OS.

### Forwards (guest → room)

- `hostel forward <room-port> [--local-port N]` listens on the guest's
  `127.0.0.1:<N>` (default: the same number, or a free port if it is in use) and
  sends each connection through the gateway (`direct-tcpip`) to the room's
  `127.0.0.1:<room-port>`. It prints the local address.
- `hostel forward list` and `hostel forward stop <room-port>` control forwards.
- The gateway allows `direct-tcpip` only to `127.0.0.1` on the lease's room. In
  version 1 any port is allowed (one slot for each machine). From M6, only ports of
  the lease's slot.

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
- **Room listeners.** The gateway asks the room's SSH server to listen on
  `127.0.0.1:<room port>` (this must succeed) and on `[::1]:<room port>` (best
  effort; the room can have no IPv6 loopback). It does not ask for `localhost`:
  OpenSSH accepts `localhost` when only one of the two addresses is free, and a
  room service on `127.0.0.1` would then get the tunnel's connections.
- **Control.** The process listens on a local control socket. Unix socket paths
  are limited to 104 bytes on macOS, so the socket is
  `/tmp/hostel-<uid>/<first 16 hex digits of sha256(local state directory)>.sock`
  (not `$TMPDIR`, which is long on macOS). The directory must belong to the user
  and have mode 0700, because `/tmp` is shared. On Windows the socket is a Unix
  socket in `%LOCALAPPDATA%\hostel\` (Go supports Unix sockets on Windows 10 and
  later), so no named pipe is necessary.
- **Files.** The tunnels of a lease (local port source, requested and given room
  port) are in `tunnels.json` in the local state directory; only `acquire` (before
  the process starts) and the process write it. The process writes its log to
  `ports.log` there (events only, no data). `hostel tunnel add|set|remove`
  and `hostel forward ...` send their request there. The process applies it and
  replies with the result. If the room's SSH server refuses a remote forward (for
  example a room service already uses a fixed port), the command fails with that
  message and the server registration is undone. `hostel acquire` waits until all
  tunnels of `hostel.toml` are confirmed; a refused tunnel makes it exit with code 1.
- **Lease state.** Every `tunnel_check` (a value in the lease data) it reads the
  lease with `GET /v1/leases/{id}`. It never calls `touch`. It exits when the lease
  is no longer `ready`. `hostel release` stops it at once.
- It reconnects with backoff when the gateway connection drops, and requests the
  forwards again. It also survives a server restart.
- **Exit.** It exits when the lease is final or not found, on SIGTERM, or when the
  server is unreachable for `port_process_offline` (default 10 minutes; a value in
  the lease data). So a crashed test or a lost server never leaves it running.

## 15. Jobs

- `job start` starts a **supervisor** (`hostel room job supervise`) in a new session
  (`setsid`) on macOS and Linux, or in a job object on Windows. The guest chooses
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
  opens, and when a `cmd` connection closes.
- A lease is active while the gateway has an open `cmd` connection for it.
  `ports` connections do not count.
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
  is cancelled (through `cleaning` if it has a slot).

## 17. Cleanup

The gateway has already closed the lease's connections (section 13). The helper
`cleanup` command then runs these steps. Each step runs even if an earlier step
failed; the result lists each step.

1. Stop all processes of the lease: job process groups from `supervisor.json`, and
   all processes of the room user that either have `HOSTEL_LEASE=<lease id>` in
   their environment (macOS: `ps -E`, Linux: `/proc/<pid>/environ`) or are in a
   session of the lease. Processes that detach from their group (Gradle daemon,
   `nohup`) still have this variable. On macOS 27.2, `ps -E` does not show the
   environment of Apple's own binaries (for example `sh` and `sleep`; checked on
   2026-10-22), so the sessions are necessary: `exec` and `shell` record their
   session ID and the start time of its leader in `slots/<slot>/sessions`, and
   each job supervisor has its own session (`supervisor.json`). A process started
   with `&` or `nohup` stays in that session. A session ID is not reused while a
   process is in the session; when the leader runs with another start time, the
   number belongs to a new session, and cleanup ignores it. One `ps` call per
   scan reads the start times; if it fails, the scan fails (it does not guess
   that a leader ended). Each scan also takes
   all descendants of these processes (by parent PID), also in other sessions, and
   records their sessions in `slots/<slot>/sessions`: on macOS 26.6 the iOS runner
   of `agent-device` is an `xcodebuild` in a new session, without a visible
   environment; after its parent ends, its parent is PID 1, and later scans find it
   only by its session (found in M2, 2026-10-23). A scan also runs at the start of
   each guest command and each job. Only a process that starts a new session and
   hides its environment, and whose parent ended before the next guest command or
   cleanup, escapes; the `ios` reset also stops the processes whose command line
   has the UDID of a slot simulator (the `agent-device` runner has it). Send
   SIGTERM, wait 10 seconds, then SIGKILL.
2. Run the `reset` script of the lease's platform. If `slots/<slot>/` does not exist
   (a repeated cleanup after step 5), the helper creates it empty first; `reset` must
   then do nothing.
3. Restore `authorized_keys` from `authorized_keys.base`.
4. Check that no process of the lease (step 1) remains, and that the slot
   ports and the lease's fixed room ports (`ports` in the input) are free: try to
   bind each port on `127.0.0.1` and on `::1` (skip `::1` if the room has no IPv6
   loopback address), and retry for up to 10 seconds, because
   the room's SSH server removes its listeners after the connections close.
5. Delete the devbox task markers of the slot's jobs whose supervisor was killed
   (if any), and the directory `slots/<slot>/`. This
   deletes `work/`, `.env`, job output, `device.env`, and the manifest. The shared
   `cache/` stays. This step is last
   before `health`, so a repeated cleanup still finds `device.env` until `reset` has
   run.
6. Run the `health` script of each platform that the room is ready for.

If a step fails, the slot and the room become `broken` (R24). On success, the slot
is `free`.

Warm policy after cleanup:

- If a room has no lease for `warm_running`, the server stops it (the machine can
  stop) or destroys it (an on-demand machine that cannot stop). Static rooms stay
  running.
- A stopped room is destroyed after `warm_stopped`.
- Eviction (section 5) can destroy an idle or stopped room earlier.

## 18. Security

- **Tokens.** Stored in the `tokens` table as SHA-256 only. Roles: `admin`,
  `guest`, and `room` (only for `hostel room connect`). `HOSTEL_TOKEN` or the guest
  config file holds the token.
- **Lease keys.** The guest generates a key pair for each lease. The private key
  never leaves the guest.
- **Provider credentials.** Only the server has them.
- **Transport.** On a Mac, the server listens on `127.0.0.1` only. On a VPS, the
  HTTP API needs TLS (a reverse proxy such as Caddy, or Go's `autocert`). Both SSH
  legs (guest to gateway, gateway to room) are encrypted. The gateway sees the
  plaintext.
- **Revocation.** Guests have no room credentials. Ending a lease closes its
  connections and invalidates its key at once (section 13).
- **Logs.** No tokens, keys, file contents, environment values, or output of guest
  jobs in server logs. Output tails of platform scripts (which the owner controls) go
  to the event log; the scripts must not print secrets.
- **Integrity between leases.** Helper, mise, and platform scripts are checked by
  hash at each `preparing`; `authorized_keys` is restored at cleanup (sections 8 and
  17).

### Backdoors (not in version 1)

The owner wants this later: a guest must not keep access to a room after its lease,
even if it installed a backdoor during the lease. Version 1 does not guarantee it,
because the guest and the server use the same OS user and rooms can be reused.

What version 1 already blocks:

- SSH keys: the room trusts only the server key, `authorized_keys` is restored at
  cleanup, and the network rule (section 13) blocks direct SSH.
- Processes that keep `HOSTEL_LEASE`, and changed helper or scripts (hash checks).

What a guest can still do in version 1:

- Start a process without `HOSTEL_LEASE` (for example with `env -i`) that connects
  out from the room (a reverse shell). Cleanup does not find it.
- Add persistence for the room user: `crontab`, `~/Library/LaunchAgents`, systemd
  user units, shell startup files, or `~/.ssh/rc`.
- Put a changed binary in a shared cache (`hostel/cache`, mise tools, package
  stores). The next lease runs it.

Planned hardening (milestone M7):

1. **One OS user for each lease.** `prepare` creates a new user without
   administrator rights; cleanup stops all its processes (`pkill -u`), removes its
   crontab, and deletes the user and its home directory. Most persistence lives in
   that home directory, so it goes too.
2. **Read-only shared caches.** The Hostel user owns the caches and installs tools
   into them. Lease users can read but not write them. A lease user has its own
   writable cache, deleted at cleanup.
3. **Egress control (optional).** A room firewall allows outbound traffic only to
   the gateway and to configured hosts (package registries, GitHub).
4. **No reuse (strongest).** On on-demand machines, `warm_running = "0s"` destroys
   the room after each lease. No backdoor can survive, at the cost of a cold start
   for each lease.
5. **Audit.** Cleanup compares the known persistence locations with a snapshot and
   marks the room `broken` if something changed.

Privilege escalation to root on a room stays out of scope.

## 19. Observability

- Server logs: `log/slog` JSON lines with lease ID and room ID.
- `events` table: one row for each state change and each provider action.
- `hostel status` shows the queue, leases (task, platform, devices, guest, state,
  age, expiry), and rooms (provider, machine, OS, state, slots, ready platforms,
  deadline).

## 20. Testing

`AGENTS.md` has the rules. Summary:

- **E2E tests are the main gate.** They start the real server binary and run the
  real guest binary as a subprocess, with a temporary home directory and a
  temporary git repository. Rooms are Docker containers with `sshd` and `git` only
  (`test/roomimage`). The image must not contain the helper or mise: the server
  uploads them, so the tests cover the upload path.
- **Isolation.** Each test server gets a unique random `server_id`. Every Docker
  object has the label `hostel-e2e=<run id>`. `test/fakeprovider list` returns only
  containers with the label of its run and the requested prefix. So tests that run in
  parallel never see each other's rooms.
- **Static rooms in tests.** The harness reserves a free host port, then starts the
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
- The test platform `fakedevice` reads markers in the room cache: `slow-prepare`,
  `fail-prepare`, `slow-reset`, `fail-reset`. Tests create them with `docker exec`
  to drive failures and restarts at a known state.
- Test platforms in `test/platforms/`: a device platform whose scripts simulate a
  device (they write `device.env` and a fake screenshot), with scripts for `linux`
  and for `macos`. An `external` machine with `os = "macos"` runs the `.macos.sh`
  scripts in a Linux container, so the tests cover script selection by OS.
- Tests use short durations (for example `idle_timeout = "3s"`). They wait for
  conditions with a deadline. They never use a fixed sleep as an assertion.
- `test/fakeprovider` under the name `devbox` (a link on the server's `PATH`) is a
  fake Namespace devbox CLI. It makes Docker rooms and copies the output formats of
  the real CLI (the list JSON, the text line of an empty list, the error lines), so
  the E2E tests cover the `namespace` provider. Its rooms set
  `NAMESPACE_DEVBOX_TASKS_DIR` for SSH sessions, as a real devbox does.
- Real providers: opt-in smoke tests (`HOSTEL_SMOKE_NAMESPACE=1`) and manual tests
  on the spare MacBook.
- CI (GitHub Actions, Linux `amd64`): `make lint`, `make test-unit`, builds for all
  six targets, and one `make test-e2e` run. `make test-e2e` fetches only the Linux
  mise binaries; CI caches them.

## 21. Decisions in this design

1. All guest SSH goes to the gateway, which terminates it (sections 1 and 13).
   Guests never get a credential for a room, so Hostel can end access at any time.
   There is no direct guest-to-room access.
2. Configuration model: providers contain machines; platforms are OS platforms
   (automatic) or device platforms (directories with scripts for each OS). The guest
   sees only platforms (section 3).
3. Configuration format: TOML with strict decoding, defaults, a JSON Schema, and
   `hostel admin config check` (section 9).
4. The guest syncs files and starts the repository setup after `ready`. The server
   prepares only the room and its devices (section 10).
5. Warm policy: keep the toolchain and caches; always delete lease state; keep a room
   running for `warm_running`, then stopped for `warm_stopped`; evict idle rooms when
   a lease needs the capacity (sections 5 and 17).
6. Each lease gets clean devices from the platform scripts (section 8). The method
   depends on the timing experiments in M2 and M4.
7. Version 1 calls the `devbox` and `nsc` CLIs on the server.
8. File sync uses manifests and `tar`, not `rsync`, so that it works on Windows
   (section 12).
9. Guests are trusted; version 1 uses one OS user on each room (section 1).
