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
| Guest | `hostel connect`, `hostel platforms`, `hostel acquire`, `hostel run`, ... | agent machines (a user's computer) |
| Config | `hostel config users ...` | any connected computer; changes need an admin (M8; `multiplayer.md`) |
| Admin | `hostel admin ...` | room tokens, room control; `config check|schema` run locally |
| Server | `hostel server init`, `hostel server invite`, `hostel server run` | the server machine |
| Helper | `hostel room ...` (hidden from help) | rooms; the server uploads it |

Rooms need no permanent daemon. Exception: a static room behind NAT runs
`hostel machine connect` (before M9: `hostel room connect`) as a service (launchd, systemd, or a Windows service).

Distribution. A tag `v*` makes a GitHub release (`make release`): the binary for
each OS and architecture, the pinned `mise` binaries, `hostel-platforms.tar.gz`,
and `SHA256SUMS`. `install.sh` installs the binary for the machine, or with
`--server DIR` also the helper binaries and the platform scripts; it checks each
file against `SHA256SUMS`. `docs/deploy-vps.md` shows a server on a VPS.

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
| `rooms` | id, name, provider, machine, provider_ref, state, os, arch, home, shell, ssh_host_key, helper_sha256, provider_deadline, warm_until, drained, broken_reason, created_at, state_changed_at |
| `room_platforms` | room_id, platform, scripts_sha256, provisioned_at |
| `slots` | room_id, name, state, lease_id, cleanup_data, home |
| `leases` | id, token_id, manual_position, began_at, state, revision, position, platform, devices, task, priority, required_labels, tunnels, fresh, public_key, guest_host, repo, worktree, provider, machine, room_id, slot_name, attempts, reason, created_at, last_poll_at, last_activity_at, ready_at, expires_at, ended_at, end_state, end_reason |
| `lease_tunnels` | lease_id, name, room_port |
| `users` | id, name (unique, not case-sensitive), role (`admin` or `member`), created_at, removed_at (M8) |
| `tokens` | id, name, role (`user` or `room`), machine (for `room` tokens), sha256, created_at, revoked_at, user_id, computer |
| `invites` | id, kind (`user` or `computer`), secret_sha256, role, name, user_id, created_by, created_at, expires_at, used_at (M8) |
| `events` | id, time, lease_id, room_id, kind, message, user_id (who caused it; 0: the server) |

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

**Order** (M8, owner decision 2026-10-30). Each lease has a lane: `low` = 0,
`normal` = 1 (default), `high` = 2; an agent sets a lane only when the user asks
for it. Order: leases that an admin placed (`manual_position` > 0, in that
order), then the lane (high first), then the request time. There is no aging: an
old `low` lease stays behind a new `normal` one. An admin places a queued lease
first or before another lease of its platform (`POST /v1/leases/{id}/move`): the
lease and the leases before it get manual positions, so they keep that order
until they start. An admin can also change a queued lease's lane.

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
- `Signers` is the server key for `static`, `external`, and Namespace instance
  rooms. For a Namespace
  devbox it is the key that the `devbox` CLI manages
  (`~/.namespace/ssh/<name>.devbox.namespace.key`): the devbox SSH server accepts
  only that key (M3, 2026-10-26).

| Kind | Machines | Provision | Access | Stop | Deadline |
|---|---|---|---|---|---|
| `static` | real hosts (`address`) | Not called. Each machine is a room from server start. | TCP to the host with the server key, or the `room connect` tunnel | no | none |
| `namespace`, `spec.product = "devbox"` | templates | `devbox create --name <Name> --ephemeral ...`; a devbox with the name that another call made (for example before a restart) is taken when it accepts SSH | proxy command `devbox ssh-proxy <Name>`, user `main_user` | no (ephemeral: a stop deletes it) | idle stop (it deletes the devbox); plan maximum duration (3 h on the Developer plan) |
| `namespace`, `spec.product = "instance"` (Linux) | templates | `nsc create --bare --machine_type ... --duration ... --ssh_key <file> --label hostel.name=<Name> --label hostel.machine=<machine>`, then `nsc ssh <id> -- docker run ... <spec.image>`: the room container (below). Idempotent: an instance with the label and an existing container are taken; the room is ready when its SSH server accepts the server key | `nsc instance port-forward <id> --target_port 2222` for each connection; user `hostel`, server key | no | `--duration`; `nsc instance extend-duration --ensure_minimum` |
| `external` | templates | the configured command | from the command's response | if the command declares it | the command must apply it |

**Namespace instances** (M4, 2026-10-27). The SSH server of an instance (the
Namespace agent) refuses remote forwards, which tunnels need. So the room is a
container on the instance: `docker run -d --name hostel-room --restart
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
  - A listed room with the name of an active database room but another provider
    ID (not in `creating`): destroy it. A Namespace instance has no unique name,
    so a create call of a killed server can make a second instance late
    (provider IDs are set only at creation).
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
   `cmd /c echo windows %PROCESSOR_ARCHITECTURE% %USERPROFILE%` (the same in
   `cmd` and in PowerShell), and then `echo %OS%` to find the SSH server's default
   shell: `cmd` prints `Windows_NT`, PowerShell prints `%OS%`. The shell goes to
   `rooms.shell`; the server and the gateway quote each command for it (package
   `shell`). Only the PowerShell default shell is tested (on `win-pc`); `cmd`
   is not (its quoting rules for `cmd /c` with several quoted words are a risk). A PowerShell command line ends with `exit $LASTEXITCODE`, because
   PowerShell itself exits with 0 or 1. The result selects
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
   && mv tmp hostel/bin/hostel'`. On Windows, Windows PowerShell does not give its
   stdin to a script, so the helper goes as a `tar` stream to `tar.exe` (part of
   Windows since 10 1803), and PowerShell then moves it into place. Windows cannot
   replace a running executable, but it can rename it: the old file moves aside
   and a later install deletes it. All other items go through the helper `install`
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
| `usage` | server | — | `{"cpu_percent", "memory_used_bytes", "memory_total_bytes", "disk_free_bytes"}`, best effort (M10b): the CPU use over 1 s (`/proc/stat`; `ps` on macOS; `GetSystemTimes` on Windows) and the memory (`/proc/meminfo`; `vm_stat`; `GlobalMemoryStatusEx`). The server runs it for the room details. |
| `hardware` | server | — | `{"cpu", "cores", "memory_bytes", "disk_free_bytes", "os_version"}`, best effort (M10). The server runs it after each provisioning and keeps it on the room for the status view. |
| `mise-install` | server | `{"timeout_seconds"}` | `{"exit_code", "output_tail", "timed_out"}`; installs the machine-level tools of `hostel/mise/config.toml` |
| `begin` | server | `{"lease_id", "platform", "devices"}` | `{}`. Writes `slots/<slot>/lease.json`. Saves the current `authorized_keys` as `authorized_keys.base` when no other slot of the room has a lease: the previous cleanup restored it, so it holds the owner's latest keys (for example a new server key). With a lease in another slot, it keeps the base, because `authorized_keys` can then have a key that that lease added. |
| `run-script <platform> <step>` | server, guest (`screenshot`) | `{"args": [...], "env": {...}, "timeout_seconds"}` | `{"exit_code", "output_tail", "missing", "timed_out"}`; `missing` when the platform has no script for this step and OS |
| `cleanup` | server | `{"lease_id", "platform", "devices", "ports", "health"}` (from the database, so a repeated cleanup does not need `lease.json`; the server sets `health` at the time of the cleanup: the platforms that the room is ready for) | `{"steps": [...]}` (section 17) |
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

Since M9, the platforms are in the server's database (`platform_revisions`).
The binary embeds the default platforms (`ios`, `android`, `macos`, `windows`,
from `platforms/` of the repository); each default applies until an admin
replaces it with their own files or removes it. Admins add their own platforms
from a client:

```
hostel config platforms                     # name, kind, machines, source (default, changed, own)
hostel config platforms new tvos ./tvos     # a folder with a template (--os linux|macos|windows)
hostel config platforms pull ios ./ios      # the current files into a new folder
hostel config platforms push ./tvos         # check and save the folder (the name is the folder name)
hostel config platforms reset ios           # a default back to the version of this release
hostel config platforms remove tvos         # refused while a machine lists it
```

A push sends only platform files (`platform.toml`, `README.md`, and scripts);
the server checks it with the whole configuration (the same rules as below) and
applies it at once. The server writes the effective platforms to
`data_dir/platforms/<hash>/` and uses them as before. A free room whose
platforms changed provisions again (an on-demand room: when a lease uses the
device platform). At the first start of M9, the platforms in `platforms_dir`
that differ from the defaults are imported as revision 1; after that,
`platforms_dir` is not read. Each device platform has one directory; OS
platforms can have one.

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
  lease's devices, comma-separated; `prepare`, `screenshot`, `reset`),
  `HOSTEL_EXCLUSIVE` (`1` when the lease holds the whole room, for example a
  desktop lease, else empty; `prepare`, `screenshot`, `reset`), the `MISE_*`
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
  The `android` scripts (`platforms/android`) write `ANDROID_SERIAL` (first
  device), `ANDROID_SERIALS` (all, comma-separated), and `ANDROID_HOME` (the SDK
  in `$HOSTEL_CACHE_DIR/android-sdk`). `provision` installs the SDK (Java from the
  machine-level tools), on Linux the Ubuntu packages that the emulator needs
  (with `sudo`), and one golden AVD `hostel-golden-<device>-<API>-<tag>` for each
  device with a `clean` snapshot. `prepare` starts each lease device from that
  snapshot with `-read-only` (the AVD does not change, so each lease gets a clean
  device) on the console port `HOSTEL_ANDROID_PORT` (slot or machine `env`,
  default 5554) + 2 × device index; a machine with several slots sets it for each
  slot. `reset` stops the emulators on the slot's ports. The machine `env` values
  `HOSTEL_ANDROID_API` (default 36) and `HOSTEL_ANDROID_TAG` (default
  `google_apis`) select the system image. Linux rooms need `/dev/kvm`.
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

`hostel server init` asks for the server name, the public addresses, and the
owner's name (or takes `--server-id`, `--url`, `--user`, `--listen`, `--tls`), and
writes a commented template with `tls = "self-signed"`, a gateway host key, the
server's own SSH key pair (`data_dir/server_ed25519`), the TLS certificate, and an
empty `platforms_dir`. When the server has no users, it makes the first admin
invite and prints `hostel connect <code>` (M8). `hostel server invite [--role
admin|member] [--name n]` makes an invite on the server machine without the API
(for example when nobody can sign in as admin). `hostel admin pubkey` prints the
server's public key.

**Two parts (M9).** `server.toml` has only the keys that the server needs on
its machine before it opens its database: `server_id`, `listen`,
`gateway_listen`, `gateway_public`, `tls`, `public_urls`, `data_dir`,
`helper_dir`, `platforms_dir`. Everything else (the providers with their
machines, and the settings `idle_timeout`, `poll_timeout`, `limit_retry`,
`port_process_offline`, `intervals`, `mise`) is the **configuration document**:
TOML with the same keys, kept in the database with a revision for each change
(`config_revisions`). The server validates the two parts together; a key in
the wrong part is an error with its key path. Errors in the document name its
revision ("configuration revision 4: providers.home.machines.box.os: ...").

- **Import.** At the first start of M9, a `server.toml` with document keys is
  split: the document keys become revision 1 (with their comments and order),
  the old file stays as `server.toml.pre-m9`, and `server.toml` keeps the local
  keys. Later, a document key in `server.toml` stops the server with a message.
- **Changes.** An admin changes the document from a client: `hostel config
  edit` (in `$EDITOR`), `hostel config apply <file>`, or the targeted commands:
  `hostel config settings set <key>=<value>`; `hostel config rooms` (the
  machines with their rooms), `rooms set <machine> <key>=<value>` (`platforms`
  and `labels` as comma lists), `rooms remove <machine>`, `rooms drain|undrain|
  repair|destroy <machine|room>` (the same as `hostel admin room`, by name);
  `hostel config providers` (list), `providers add <name> --kind <kind>`,
  `providers remove <name>`. The client sends the whole
  document with its base revision; the server refuses it if another revision
  came in the meantime (HTTP 409) or if it is invalid (HTTP 422 with each key
  path). The targeted commands change the text in place, so comments and the
  machine order (the scheduler's order) stay.
- **Apply.** A valid revision applies at once, without a restart: the server
  builds the providers again (only the changed ones: a Namespace provider
  checks its login, an external provider loads its capabilities), hashes the
  platforms, adds rooms for new static machines, and wakes the scheduler. A
  room records the hash of its machine settings, its platforms, and the `mise`
  settings when it is provisioned. When it is free and the hash changed, a
  static room provisions again and an on-demand room is destroyed. A machine
  with an active lease cannot be removed (drain its room, and remove it when
  the lease ends); an on-demand machine cannot be removed while it has rooms
  (destroy them first: `hostel config rooms destroy <room>`); a provider
  cannot be removed while it has on-demand rooms. The room of a removed static
  machine is retired.
  The local keys still need a restart.
- **Machines that join.** `hostel config rooms add <name> [--platform p]
  [--label l] [--provider p]` makes a machine invite (24 hours, once) and prints
  one command for macOS and Linux (`curl ... /v1/install.sh | sudo sh -s --
  <code>`) and one for Windows (`curl.exe ... /v1/download/hostel-windows-<arch>.exe`,
  then `hostel.exe machine join <code>`). With TLS `self-signed`, curl pins the
  server's key (`--pinnedpubkey`). `hostel machine join <code>`, as an
  administrator on the machine: (1) creates the user `hostel` without
  administrator rights if it is missing; (2) sends the OS, architecture, and
  host name with the code (`POST /v1/machines/join`, no token, the rate limit
  of `connect`); the server adds the machine to the document (`connect = true`,
  in the static provider `home` unless the invite names another, which it adds
  if it is missing) and returns a room token and its SSH public key; (3) puts
  the key into `authorized_keys` and `hostel/authorized_keys.base` of `hostel`;
  (4) writes the token, the result of the join (`join.json`: a second run
  after a failure continues with it), and a copy of the binary into a
  directory that only administrators can write (`/etc/hostel`, or
  `C:\\ProgramData\\hostel` with its owner and rules set by security IDs), and
  installs the service that runs `hostel machine connect` from there (Linux:
  systemd unit as `hostel-connect`, which owns the token; macOS: LaunchDaemon as
  root, which is not the room user; Windows: scheduled task as `LOCAL SERVICE`
  that runs `machine-connect.cmd`); (5) prints the steps that only a person can
  do. The server key goes into the room user's files as that user (a lease can
  leave links there). The server refuses a join for a machine that exists, and
  revokes the old room tokens of the name; removing a machine revokes its room
  tokens. `--no-service` skips the service. `hostel machine connect` is the
  former `hostel room connect` (still accepted).
- **Provider login.** `hostel config providers login namespace [--cli
  devbox|nsc]`: the server runs `<cli> login --browser=false` (deadline 10
  minutes, one at a time for each CLI) and gives the client the address that it
  prints; the client prints it and waits until the login ends. The credentials
  stay on the server. A logged-in CLI is reported, and no login runs.
- **Reading.** Members and admins read the document (`hostel config`,
  `hostel config settings`, `hostel config history`); for members, the values
  of `env`, `command_env`, and `github_token` are `***`.

The document, as in `server.toml` before M9:

```toml

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
# image: the room container (default ubuntu:24.04)
spec = { product = "instance", machine_type = "linux/amd64:8x16" }
platforms = ["android"]
labels = ["kvm"]
tools = { java = "21" }             # the android provision script needs Java
max_rooms = 2
```

Server-level keys and defaults:

| Key | Default |
|---|---|
| `listen` | `127.0.0.1:7410` (HTTP API) |
| `gateway_listen` / `gateway_public` | `127.0.0.1:7422` / same as `gateway_listen`, or `/v1/gateway` with `tls = "self-signed"` (the client adds the server address that it uses and its pinned key); `gateway_public` can also be a `wss://<host>/v1/gateway` URL (section 13) |
| `data_dir` | `~/.local/share/hostel` |
| `helper_dir` | `<data_dir>/helpers` (`hostel-<goos>-<goarch>` and `mise-<goos>-<goarch>`, section 8) |
| `platforms_dir` | `~/.config/hostel/platforms`; read only at the import (M9) |
| `idle_timeout`, `poll_timeout`, `limit_retry` | `10m`, `5m`, `2m` (`aging` was removed in M8; the key is an error) |
| `tls` | `off`; `hostel server init` writes `self-signed` (the server's own certificate in `data_dir/tls_cert.pem`, `tls_key.pem`; clients pin its key) |
| `public_urls` | `[]`: the addresses that clients use, in invite codes; port 0 becomes the listen port; empty: the listen address. Each is `https://` (also with `tls = "off"`: the proxy has TLS); `http://` only on loopback with `tls = "off"`, because clients send the invite secret and their token to it |
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

**Slot users** (static machines, M6). A slot can have its own OS `user`
(`slots = [{ name = "a", user = "hostel-a" }, { name = "b", user = "hostel-b" }]`),
so two leases on one room cannot read each other's files or stop each other's
processes. The user needs the server key in its `authorized_keys`, and a home
directory that other users cannot read. The server connects as the machine user
(`address`) for the room-level steps (probe, install, provision, health) and as
the slot user for the slot steps (begin, prepare, cleanup) and for the guest's
connections. At each room provisioning, it probes each slot user (its home goes to
`slots.home`), uploads its own `hostel/` directory, and installs the machine-level
tools for it. After the cleanup of such a slot, the server runs the health scripts
as the machine user. A platform whose `prepare` needs the result of `provision` in
the machine user's account (for example the iOS golden simulators, which belong to
the user that made them) does not work with slot users; such machines keep one
slot or use slots without users.

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
setup = ["tuist install && tuist generate --no-open"]
env = { APP_API_BASE_URL = "http://localhost:{tunnels.api.port}" }

[platforms.android]
env = { APP_API_BASE_URL = "http://10.0.2.2:{tunnels.api.port}" }

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
- **Control (M7).** The guest controls desktop apps with `agent-device
  --platform macos` (an XCTest runner; Automation Mode without a password
  prompt). It starts its own app with `open <path to .app>` and then `agent-device
  open <bundle id>`: `agent-device open` with a path failed (2026-10-29). The
  first command of a lease builds and starts the runner (about 30 s). A command
  can fail while the runner still drains an earlier one that hit its 30 s
  watchdog; a retry a few seconds later works.
- **Desktop cleanup (M7).** Apps that a guest starts with `open` are children of
  launchd, without `HOSTEL_LEASE` and outside the lease's sessions, so the process
  cleanup (section 17) does not find them. For a desktop lease
  (`HOSTEL_EXCLUSIVE=1`), `platforms/macos/prepare.macos.sh` records the
  foreground apps (`lsappinfo`; PID, start time, bundle ID) and the entries of
  `~/Applications`, and `reset.macos.sh` ends each foreground app that was not
  running then (SIGKILL: a quit can wait at a save dialog; not Finder or the
  Dock), deletes its saved window state, ends the `agent-device` runner, and
  deletes the new entries of `~/Applications`. A headless `macos` lease does not
  touch the desktop. The room needs an unlocked desktop session: a locked screen
  gives the lock screen to `screencapture` and blocks the runner (found on the
  static MacBook, 2026-10-29).

### The Windows desktop (M7)

- The guest requests it with `hostel acquire windows --require desktop`
  (exclusive, as on the Mac). The machine has the label `desktop`, a signed-in
  desktop user, and the "Hostel desktop" task (`static-room-setup.md`, Windows,
  step 6).
- **The desktop program.** SSH sessions (Windows session 0) cannot show
  windows, take screenshots, or use UI Automation. The task of the desktop user
  starts `hostel room desktop serve` as the room user in the desktop session
  (`Start-Process -Credential`; the Secondary Logon service gives it access to
  that desktop). It listens on `127.0.0.1` with a random port; `hostel/desktop.json`
  (only the room user can read it) has the address and a random token. It ends
  when the helper file changes, and the task starts the new helper.
- **`desktop [--detach] <command>`** (`hostel/bin/desktop`, `room desktop
  run`) sends a command with the lease environment and the current directory.
  The program accepts only the lease of the slot (the caller's `HOSTEL_LEASE`
  must match `lease.json`) and only a desktop (exclusive) lease. It sets
  `HOSTEL_LEASE` and `HOSTEL_SLOT` from `lease.json` (not from the request), runs the command in its own job object
  (kill on close, no breakaway; on macOS and Linux a process group), and returns
  the output and exit code. With `--detach`, it returns the PID at once. If the
  caller ends, the command ends. A command and its processes stay until the
  lease ends: within 2 s after cleanup starts (it writes the slot's
  `lease-ended` marker first) or the slot's lease changes, the program ends the
  job object. A command's output ends 2 s after it exits, also when a child
  keeps it open. The program removes `desktop.json` when it ends, and it waits
  for a new helper until no lease command runs. Cleanup (section 17) finds the processes by `HOSTEL_LEASE` before
  that. Without the program, `desktop` exits with 69.
- **Control.** Windows rooms with `agent_device` get the pinned `winapp` CLI
  (Microsoft, `config.WinAppVersion`, SHA-256 checked) in
  `hostel/tools/winapp-<version>`. `hostel/bin/winapp.cmd` runs it through
  `desktop`. `winapp ui` reads the UI Automation tree and drives apps with
  patterns (`invoke`, `set-value`) and with real input (`click`, `send-keys
  --via send-input`), which it sends only when the app is the foreground
  window.
- **Screenshot.** `platforms/windows/screenshot.windows.ps1` captures the whole
  desktop through `desktop`.
- **Limits.** The apps of a lease run as the room user, but they share the
  desktop with the desktop user's windows: real input can reach a window of
  the desktop user that is not elevated. An app that is started through the
  shell (for example a packaged app through its activation) is not a child of
  the program and has no `HOSTEL_LEASE`, so cleanup does not end it. A
  disconnected or locked desktop stops real input and screenshots.

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
| `hostel acquire <platform> --task ... [--device d]... [--priority low\|normal\|high] [--require label]... [--fresh] [--wait 100s] [--background]` | Creates or continues a lease. The lease ID is in the worktree (`$(git rev-parse --git-dir)/hostel/<platform>/lease.json`), so the same command continues the same lease and its place. When it returns with code 75 for a waiting lease, it says how long the server keeps the place without it (`poll_timeout`). With `--background`, it returns at once and starts a detached `hostel keep-place --dir <state dir>` that polls the lease until it is ready or ends (one keeper for each lease: `keeper.json` holds its ID and heartbeat; it also stops when the worktree has another lease, after 1 h without an answer, and after 24 h; it removes its heartbeat when it stops; server errors and network errors do not stop it). |
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
| `hostel status [room \| platform]` (aliases `ls`, `rooms`) | Plain text for agents and scripts (people use the app, below): the rooms with their leases (user, task, time, lease ID), the machines without rooms, and the queues with their next lease. `hostel status <room>` prints the details of a room (hardware, leases with jobs, queue, events, room ID); `hostel status <platform>` prints the queue (position, user, task, priority, wait, estimate, lease ID, reason). Control characters from users (tasks, commands) are removed. |
| `hostel release` | Ends the lease (or cancels it if it is still queued) and stops the port process. |
| `hostel agent-guide` | Prints short usage instructions for coding agents. |
| `hostel screenshot [--device d] [-o file.png]` | (see above) The script writes to a file in the slot directory; the guest downloads it, deletes it on the room, and prints the local path. |

Connection and users (M8, `multiplayer.md`): `hostel connect <invite code>`,
`hostel disconnect`, `hostel config users [invite|code|set|remove]` (`remove
<name> --computer <computer>` removes one computer only), and
`hostel release <lease>` (another user's lease: admins only).

Admin commands: `hostel admin token create --role room|revoke`, `admin pubkey`,
`admin lease release <lease-id>`, `admin room drain|undrain|repair|destroy <room>`,
`admin config check|schema`. The room commands take the room name or ID;
`hostel status` shows the lease IDs, `hostel status <room>` the room ID.

**The Hostel app (M10b).** People use `hostel` without arguments when stdin
and stdout are terminals: a full-screen app (Bubble Tea, `internal/app`); in a
pipe, `hostel` prints the help. Agents and scripts use only the `hostel <verb>`
commands; `hostel status` prints plain text with the next commands at the end.

- Not connected: *Check in* (an invite code, then the name; the same code as
  `hostel connect`, package `internal/connect`) or *Leave*.
- Main menu: *Lobby* (the platforms: free machines of all that serve it, the
  queue, the wait; a platform page with its machines, the typical lease, and
  the full queue), *Rooms* (the machines with their platforms and state in
  plain words; a machine page with the hardware, the live use, and each lease
  with its user, task, computer, repository, branch, and running jobs), *Front
  desk* (the server and you; *Invite a guest* for admins, *Add your other
  computer* for members, *Guest list*, *Add a room* for admins, *Check out*). Themed labels have a muted plain description (owner decision,
  2026-11-03).
- Actions ask "… ? [y/N]" (or a choice) and use the API: end a lease (admins:
  any; members: their own), move (`shift+↑/↓` or `K`/`J`) and set the priority
  of a queued lease, stop or allow new leases on the room of a static machine and repair it, invite
  someone, change a role, remove a user, a code for the caller's other
  computer, a machine code.
- Data: one long poll of `GET /v1/status`; `GET /v1/platforms` after each
  status answer; a machine page reads `GET /v1/rooms/{name}` when it opens and
  at most every 10 s. Each request has a deadline; after an error the app asks
  again after 5 s; a refused token (401/403) goes to Check in. Text from users
  and the configuration is cleaned of control characters before it is drawn.
- Invite codes and join commands can be longer than a screen line: the app
  wraps them, and prints them again after the full screen ends, to copy.
- Presence (owner decision, 2026-11-03): every `intervals.presence` (default
  30 s) the server makes an SSH connection with its key to each idle static
  room and to each offline one, and closes it (`presence.go`; through the
  gateway for a machine that connects to the server; 10 s deadline). It logs
  in on purpose: OpenSSH 9.8 and later (`PerSourcePenalties`) penalizes a
  source whose connections close without authentication, and then refuses the
  server's own connections. A running room without a lease
  that does not answer goes offline (broken, reason `unreachable`, the state
  that the scheduler already skips); an offline room that answers provisions
  again. `rooms.last_seen_at` (migration 009) is the last answer (a presence
  check or a provisioning); the status shows it as `last_seen` for an offline
  room. The reconcile loop no longer provisions unreachable rooms blindly.
  The app shows "Offline · last seen …" in Rooms, and the Lobby leaves offline
  machines out; `hostel status` prints "offline (last seen …)".
- The look (owner decision, 2026-11-03): each page has the header bar (the
  Hostel sign, the place, the user and role) and the footer (the description of
  the selected item, or a question or message; then the keys as chips; `?`
  lists all keys). Sections are cards with rounded borders; the card with the
  keyboard has an amber border, and `tab` moves between cards. Two cards stand
  side by side from 100 columns. Colors (ANSI 256; `NO_COLOR` turns them off):
  amber for Hostel and the selection, green vacant, blue occupied, yellow
  waiting, red broken, gray for secondary text. Machine states are themed
  words (Vacant, Occupied, Getting ready, Broken, Not connected); the live use
  shows as gauges. `1` `2` `3` jump to Lobby, Rooms, Front desk.

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

All requests except `POST /v1/connect` use `Authorization: Bearer <token>`. JSON
in and out. A user can read and change only their own leases (from any of their
computers); an admin can do everything. Any user can read `status`, `platforms`,
and `users`. Room tokens can only read `GET /v1/info`.

| Method and path | Effect |
|---|---|
| `GET /v1/info` | Server version, gateway address, gateway host key, server SSH public key (for `hostel admin pubkey`). |
| `GET /v1/platforms` | Platforms: name, kind, description, devices, `max_devices`, free slots, queue length, estimated wait, the keys of the machines that serve it, and `typical_lease_seconds` (the median of its last leases; M10b). |
| `POST /v1/leases` | Creates a lease. Returns its ID, state, and revision. HTTP 422 with `{"code": "unknown_platform" \| "unknown_device" \| "too_many_devices" \| "no_machine"}` for R4. |
| `GET /v1/leases/{id}?since=R&wait=50s` | Returns the lease when its revision is greater than `R`, or after `wait` (at most 60 s and half of `poll_timeout`, so the guest polls again before the timeout). Updates `last_poll_at` when the token is the lease owner (an admin can look without keeping a lease alive). The lease has `poll_timeout_seconds` and `idle_timeout_seconds` (the server settings), for the messages of the guest. |
| `POST /v1/leases/{id}/touch` | Records guest activity. HTTP 409 with `{"code": "lease_ended"}` and the end reason for a lease that ended; the guest then marks its local state as final. |
| `PUT /v1/leases/{id}/tunnels` | Registers the lease's tunnels: `[{"name", "room_port"}]` (`room_port` 0 = select from the slot pool). Returns the room port of each tunnel, or HTTP 409 for a port that is not available. |
| `POST /v1/leases/{id}/release` | Releases or cancels. For another user's lease (admin), the end reason is "released by <admin> (admin)". |
| `POST /v1/leases/{id}/move`, `/priority` (admin) | Queue control (section 5): `{"first": true}` or `{"before": "<lease id>"}`; `{"priority": "low|normal|high"}`. HTTP 409 for a lease that is not queued. |
| `GET /v1/status[?since=R&wait=50s]` | Leases (with their user, ready time, and the estimated wait of queued leases), rooms (with hardware), the machines of the configuration, the server ID, the caller (user, role), and `revision` (a hash of the content without the wait estimates). With `since`, the answer waits (at most 50 s) until the revision differs from `R`; the server checks again on each lease change and every 2 s (M10). |
| `GET /v1/rooms/{name}` | Room details (M10): the room, its machine, its leases, the jobs of its ready leases (the server asks the room helper, `job list`, all in one 10 s deadline; on an error `jobs_error` says "jobs unknown"), the queue of its platforms, its last 10 events, and the live use (`usage`, the helper command `usage` in the same deadline; M10b). A member gets the jobs and events of the member's own leases only (commands and messages can hold secrets). |
| `POST /v1/connect` (no token) | `{"secret", "name", "computer"}` from an invite code; returns `{"server_id", "user", "role", "token"}` once. HTTP 403 for an invalid code, 409 `name_taken`, 429 after 10 failures per minute. |
| `GET /v1/me`, `DELETE /v1/me` | The token's user, role, and computer; `DELETE` revokes the token (`hostel disconnect`). |
| `GET /v1/users`, `PATCH /v1/users/{name}`, `DELETE /v1/users/{name}`, `DELETE /v1/users/{name}/computers/{computer}` (admin to change) | Users with their computers; `{"role"}`; removal (tokens revoked, leases ended); removal of one computer (its tokens revoked; the user stays). The server keeps at least one admin (HTTP 409 `last_admin`). |
| `GET /v1/config`, `PUT /v1/config`, `GET /v1/config/history` (admin to change) | The configuration document `{revision, document}`; a change `{base_revision, document}` gives the new revision, HTTP 409 (`conflict`) if `base_revision` is not the latest, or HTTP 422 with the errors (M9) |
| `GET /v1/config/platforms`, `GET/PUT/DELETE /v1/config/platforms/{name}`, `POST /v1/config/platforms/{name}/reset` (admin to change) | The platforms with their source and machines; a platform's files `{name, files}`; a new version; removal (HTTP 409 while a machine lists it); a default back to the release's version (M9) |
| `POST /v1/machines/invites` (admin), `POST /v1/machines/join` (the code, no token), `GET /v1/install.sh`, `GET /v1/download/hostel-<os>-<arch>[.exe]` (no token) | A machine code with the commands to run; the join (adds the machine, returns a room token and the server's SSH key); the install script and the binaries of `helper_dir` (M9) |
| `POST /v1/logins/{cli}`, `GET /v1/logins/{cli}[?wait=1]` (admin; `cli` is `devbox` or `nsc`) | Starts a provider CLI login on the server and returns its address (or `already`); the state (`waiting`, `done`, `failed`) (M9) |
| `POST /v1/invites` | `{"role", "name"}` (admin) or `{"self": true}` (a code for the caller's other computer); returns the code, the key fingerprint, and the expiry (24 hours). |
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
  Check this for a real app build in M2.

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

- **Transport.** The gateway listens on TCP (`gateway_listen`). It also accepts
  the same SSH inside a WebSocket (binary messages) on the API listener at
  `GET /v1/gateway`, for a server that only HTTPS reaches (an exe.dev VM, M5).
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
  connection to the lease's room with `RoomAccess` (dial and server signers).
- **Sessions.** The gateway supports `exec`, `shell`, `pty-req`, `env`,
  `window-change`, `signal`, and `exit-status`. It wraps each session in the
  helper: `shell` becomes `hostel room shell --slot <slot>`, and `exec <string>`
  becomes `hostel room exec --slot <slot> --command-b64 <base64 of the string>`. The
  helper decodes the string and runs it with `sh -c` (macOS, Linux) or PowerShell
  (Windows: PowerShell 7 `pwsh` if it is in the room's `PATH`, else Windows
  PowerShell 5.1, with `-NoProfile -NonInteractive -Command`; not
  `-EncodedCommand`, with which Windows PowerShell writes stderr as CLIXML). On
  Windows, a wrapper gives the exit code of the last command and turns off
  progress records and colors. Base64 avoids all quoting problems.
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
- **Rooms behind NAT** (M5). `hostel room connect --server <url> --token-file
  <file>` (or `HOSTEL_ROOM_TOKEN`; never a token in the arguments, which other
  users can read) runs on the room as a service. It runs as an account other than
  the room user and the slot users, and only that account can read the token: a
  guest runs as the room user, and with the token it could replace the room's
  connection (found in M6; the server pins the room's host key, so it cannot read
  the server's sessions, but it can stop them). It reads the gateway address and
  host key from `GET /v1/info` (the only API call that accepts a `room` token),
  connects to the gateway (TCP or WebSocket), and authenticates as user `room`
  with the token as the password. A `room` token is bound to one machine when it
  is created (`hostel admin token create --role room --machine home/macbook`); the
  gateway accepts it only for a static machine with `connect = true`. The gateway
  keeps one connection for each machine (a new one replaces the old one) and opens
  a channel `hostel-room-ssh` on it for each server connection; the room connects
  the channel to its own SSH server (`--ssh`, default `127.0.0.1:22`). The room
  cannot open channels. The static provider's `Dial` uses this for machines with
  `connect = true`; the user comes from `address` (its host is not used). The room
  sends a keepalive every 30 s and connects again after a loss (backoff 1 s to
  30 s). Without the connection, the room is unreachable: a probe fails, and guest
  commands of a ready lease fail until the connection comes back. When the room
  connects, the server probes an unreachable room at once (not at the next
  reconcile). Revoking a room token closes the connections that use it; a
  connection checks its token again after it is registered, so a revoke during
  its handshake also ends it. A dial
  through the connection has the same 15 s limit as a TCP dial.

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
  embeds (for example `APP_API_BASE_URL`) stays valid.
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
  of its fixed room ports; it waits. (Such a lease can wait for that room although
  another room is free; a later version can treat fixed room ports as a resource of
  the room.)
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
- The gateway allows `direct-tcpip` only to `127.0.0.1` on the lease's room. On a
  room with one slot, any port is allowed (for example a bundler on 8081). On a room
  with several slots, only the ports of the lease's slot, unless the lease holds
  all slots (an exclusive lease) (M6). A tunnel cannot use a port of another
  slot either. This keeps the gateway paths apart; it does not isolate the slots
  on the room: all slot users share the room's loopback interface, so a guest
  command on the room can connect to a server of the other slot's lease. Slot
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
  (`setsid`) on macOS and Linux. On Windows, the SSH server puts each session in a
  job object that ends its processes when the session ends; the supervisor starts
  with `CREATE_BREAKAWAY_FROM_JOB`, and it starts the command suspended in a job
  object of its own (`Local\hostel-<user SID>-<slot>-<job id>`, with
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

   On Windows (M6), the helper reads `HOSTEL_LEASE` from the environment block of
   each process of the room user (through its PEB), and finds descendants through
   the parent PIDs of a Toolhelp snapshot; a child must be younger than its parent,
   because Windows gives the PID of an ended parent to new processes. Windows has
   no Unix sessions. A guest command (`exec`, `shell`) runs in a job object
   without breakaway, inside the SSH session's job, so its processes end with
   the session and cannot leave it; only the helper's own `job start` runs
   outside it (its supervisor must break away, section 15).
   The helper merges environment sources with upper-case names on Windows, which
   ignores the case of names (`Path` is `PATH`).
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

- **Users and tokens** (M8). A user has a unique name and a role, `admin` or
  `member` (`multiplayer.md`, section 6). A user token belongs to a user and a
  computer; it comes only from an invite code (`hostel connect`). Room tokens
  (only for `hostel room connect`) have no user. Tokens and invite secrets are
  stored as SHA-256 only. The client's connection file
  (`~/.config/hostel/config.toml`, mode 0600) or `HOSTEL_TOKEN` holds the token.
  `POST /v1/connect` allows 10 failed attempts per source address and minute.
- **Lease keys.** The guest generates a key pair for each lease. The private key
  never leaves the guest.
- **Provider credentials.** Only the server has them.
- **Transport.** With `tls = "self-signed"`, the API and the WebSocket gateway are
  HTTPS with the server's own certificate; clients check its key against the
  key that they pinned from the invite code (`internal/tlspin`), not the name or a
  chain. With `tls = "off"`, the server is behind a proxy with TLS (exe.dev,
  Caddy), and clients use the system roots. Both SSH
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

Planned hardening (milestone M11):

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
- `hostel status` shows the rooms with their leases (user, task, time, lease
  ID), the machines without rooms, and the queues; `hostel status <room>` adds
  the hardware, the jobs, and the last events (M10).

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
  `NAMESPACE_DEVBOX_TASKS_DIR` for SSH sessions, as a real devbox does. As at
  Namespace, a devbox is in the list from the start of its create and boots on
  its own (`FAKE_DELAY`), also when the create call is killed; `ssh-proxy` fails
  until the boot ends. `FAKE_CREATE_ERROR` makes a create fail with a given
  text.
- Under the name `nsc`, it is a fake `nsc` CLI: an instance is a state file
  (destroyed when its `--duration` ends), `list` prints `null` when empty, a
  create blocks at `FAKE_LIMIT`, and `instance port-forward` prints `Listening on
  127.0.0.1:<port>`. Its `nsc ssh` runs the command with a shell and a `docker`
  shim (the same binary under the name `docker`), whose `run` starts the room
  container from the test room image. So the E2E tests run the provider's real
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
