# Design: users, remote administration, and the status view

Status: approved by the owner (2026-10-30). Milestones M8 to M10 in `plan.md`
build it; each milestone moves its part into `design.md`. Owner decisions:
`requirements.md`, section 3, 2026-10-30.

## 1. Goals

1. Several people use one Hostel server and its rooms. The server knows who
   uses each lease, each queue entry, and each room.
2. A person connects a client to an existing server with one command, also when
   the server has no domain (an IP address, a LAN address, or a Tailscale name).
3. After the first setup on the server machine, an admin configures everything
   from a client: platforms, rooms, providers, users, and settings.
4. People see the whole server in the terminal: rooms, who uses them and for
   how long, and the queues. Admins act on leases and queues from there.

Not goals: one Hostel for everybody (each owner runs their own server),
passwords or an outside identity service, a web interface, protection against a
malicious user (milestone M11), a limit of leases per user (future).

## 2. Words

| Word | Meaning |
|---|---|
| **user** | A person on the server: a unique name and a role. |
| **role** | `admin` or `member` (section 6). |
| **client** | The `hostel` binary on a person's computer. Agents on that computer use it too. |
| **connection** | The client's link to one server: addresses, pinned server key, and token. A client has at most one. |
| **invite code** | A one-time code that connects a client to a server as a user (section 4). |
| **machine code** | A one-time code that adds a machine as a room (section 7). |

## 3. The first setup, on the server machine

This is the only step that needs a shell on the server machine.

```
$ curl -fsSL https://github.com/csansoon/hostel/releases/latest/download/install.sh | sh
$ hostel server init
Hostel server setup

  Name of this server [my-vps]: acme
  Addresses that clients use, one per line, empty line to end
  (for example https://hostel.example.com or 100.80.1.2:7410):
  > 100.80.1.2:7410
  >
  Your name [Alex Example]:

Wrote /home/exedev/.config/hostel/server.toml
Wrote the server keys and the TLS certificate to /home/exedev/.local/share/hostel
Installed and started the service hostel.service

Connect your computer as the first admin. Run this on your computer:

  hostel connect hostel1-6Y3kqP0...Zp9

The code works once, for 24 hours.
```

- `server.toml` keeps only what the server needs to start: name, listen
  addresses, public addresses, data directory, TLS mode. From M9, all other
  settings are in the database (section 7).
- TLS mode:
  - `self-signed` (the default): the server makes its own certificate and
    serves the API and the gateway (SSH in WebSocket, `/v1/gateway`) over HTTPS
    on one address. Clients pin its key from the invite code. No domain and no
    certificate authority are necessary.
  - `off`: for a server behind a proxy with TLS (exe.dev, Caddy). Clients check
    the proxy's certificate in the normal way.

## 4. Connecting a client

### The invite code

```
hostel1-6Y3kqP0...Zp9
```

One string (about 120 characters). The client finds the server with it:

- the server's public addresses, in order;
- the SHA-256 of the server's TLS public key (absent with TLS mode `off`);
- a one-time secret (128 bits);
- the code version (`hostel1`).

The server stores a hash of the secret, the role, an optional name, the user
(for a code that adds a computer to an existing user), and the expiry time
(default 24 hours). A code works once.

### `hostel connect`

Before a client has a connection, the other commands (except `help` and
`--version`) say:

```
$ hostel status
hostel: this computer is not connected to a Hostel server.
Ask an admin of the server for an invite code, then run:
  hostel connect <invite code>
```

```
$ hostel connect hostel1-6Y3kqP0...Zp9
Server:  acme (100.80.1.2:7410), Hostel 1.1.0
Key:     SHA256:q3J1...e8Q (pinned)
Your name [Ana Pérez]:
Connected as Ana Pérez (member) from ana-mbp.
Run "hostel status" to see the rooms. Agents on this computer use this connection.
```

- The client tries the addresses in order and uses the first one that answers
  with the pinned key.
- The name comes from `git config user.name`, or `--name`. The server refuses a
  name that another user has.
- The client writes `~/.config/hostel/config.toml` (mode 0600): server name,
  addresses, pinned key, and its token. The token belongs to this user and this
  computer (`ana-mbp`), so an admin can disconnect one computer.
- If the client has a connection, `connect` shows the old and the new server
  and asks before it replaces it (`--yes` skips the question).
- `hostel disconnect` deletes the token on the server and the connection.
- `HOSTEL_SERVER`, `HOSTEL_TOKEN`, and `HOSTEL_SERVER_KEY` still override the
  file (tests, CI).

A user's other computer: `hostel config users code` (any user, for themself)
prints an invite code that adds a token to the same user.

## 5. The command tree

Agents use the same commands as today. Server configuration is only under
`config`, so people who only use rooms see few top-level commands.

```
hostel connect <invite code>         connect this computer to a server
hostel disconnect
hostel status   (aliases: ls, rooms) interactive in a terminal; plain text otherwise
hostel room <name>                   details of one room (plain text)
hostel platforms                     platforms with descriptions, devices, free slots, waits

hostel acquire / run / sync / release / ...   unchanged (agents)
hostel release <lease>               an admin can release any lease

hostel config                        the server configuration (TOML), read-only for members
hostel config edit                   edit it in $EDITOR (admin)
hostel config apply <file>           (admin)
hostel config history
hostel config platforms [new|pull|push|reset|remove] ...   (admin to change)
hostel config rooms [add|set|remove|drain|undrain|repair] ...
hostel config providers [add|set|remove|login] ...
hostel config users [invite|code|set|remove] ...
hostel config settings set <key>=<value>

hostel server init|run               on the server machine
hostel machine join|connect          on a room machine (was "hostel room ...")
```

A command that needs an admin says so before it does anything.

## 6. Roles

| Action | member | admin |
|---|---|---|
| See status, rooms, queues, platforms, users, configuration | yes | yes |
| Acquire and use leases | yes | yes |
| Release own lease or queue entry | yes | yes |
| Release, move, or change the priority of another user's lease | no | yes |
| Change platforms, rooms, providers, settings | no | yes |
| Invite, change, remove users | no | yes |

- The server refuses to remove or demote the last admin.
- Removing a user deletes their tokens, removes their queue entries, and ends
  their active leases (with cleanup).
- Room tokens stay a separate kind: they are not users.
- When an admin ends or moves a lease, the event and the lease's end reason say
  so ("ended by Alex E. (admin)"); the user's client prints the reason.

## 7. Remote configuration (M9)

### Where it lives

- The server configuration (providers, machines, server settings) is one TOML
  document with today's keys. The server stores it in the database with a
  revision, the user, and the time.
- A change goes through today's validation (`hostel admin config check`). A
  valid change becomes the new revision and applies at once (section 7.5).
  An invalid change is refused with its key path.
- `config edit` downloads the document, opens `$EDITOR`, and uploads it with
  its base revision; if someone changed it in the meantime, the upload is
  refused.
- The targeted commands (`config rooms set`, ...) change the same document.
- At its first start, the new version imports the provider and machine sections
  of the existing `server.toml` as revision 1, keeps a copy of the file, and
  removes those sections from it. Existing servers keep working.

### Platforms

The server defines its platforms. A platform is settings (`platform.toml`:
description, devices, `max_devices`, `requires`, `exclusive`, timeouts) and
scripts for each step and OS, as today.

- **Defaults.** `ios`, `android`, `macos`, and `windows` come with each Hostel
  release (embedded in the binary). On a server, each default is active until an
  admin replaces it with their own version or removes it.
- **Own platforms.** An admin creates new ones (for example `tvos`).
- Machines list the device platforms that they serve; OS platforms are
  automatic for their OS (as today). One macOS machine can serve `macos` and
  `ios`.
- A lease for a platform that the server does not have fails at once with the
  list of platforms (R4, as today). `hostel platforms` lists them with
  descriptions (as today).

```
$ hostel config platforms
NAME     KIND    MACHINES                SOURCE
android  device  namespace/linux         default
ios      device  home/macbook, ns/mac    default
macos    OS      2 machines              default (changed: rev 2, Alex E.)
tvos     device  home/macbook            own (rev 3, Alex E., 2 days ago)

$ hostel config platforms new tvos ./tvos     # a folder with a template for each script
$ hostel config platforms pull ios ./ios      # the current version as a folder
$ hostel config platforms push ./ios          # validate, then save as a new revision
$ hostel config platforms reset macos         # back to the default
$ hostel config platforms remove tvos         # refused while a machine lists it
```

`push` makes today's checks (file names, steps, OS names, `platform.toml`). A
room provisions the changed platform again after its current lease.

### Adding a machine

```
$ hostel config rooms add
Name of the machine: studio-mac
Device platforms (optional): ios
Labels (optional): xcode-26.5, desktop

Run this on studio-mac as an administrator, in the next 24 hours:

  curl -fsSL https://.../install.sh | sudo sh -s -- machine join hostel1m-7Pq...

It creates the user "hostel", installs Hostel, and starts the room service.
"hostel status" shows the room when it connects.
```

`hostel machine join <machine code>` on the machine:

1. creates the local user `hostel` without administrator rights (macOS, Linux,
   Windows);
2. installs the server's public key for that user;
3. installs `hostel machine connect` as a service of another account, with its
   room token (`deploy-vps.md`, section 6);
4. reports the OS, architecture, and hardware.

The server adds the machine (OS from the report), and the room appears. Steps
that only a person can do (Xcode, "Require password" on a Mac desktop, the
Windows desktop task) are printed at the end. Machines that the server reaches
with SSH stay possible through `config edit`.

### Providers

```
$ hostel config providers add namespace
The server needs a Namespace login. Open this address and sign in:
  https://cloud.namespace.so/...
Waiting... signed in.
Added provider "namespace". Add machine templates with "hostel config edit".
```

The server runs the provider's login and passes the address to the client
(for `nsc`: to check in M9). Credentials stay on the server.

### How a change applies

| Change | Effect |
|---|---|
| New machine, provider, or platform | Available to the scheduler at once. |
| Changed machine settings, platforms, or `mise` settings | New leases use the new values. When a room is free, a static room provisions again and an on-demand room is destroyed (a new one starts when a lease needs it). |
| Removed machine | Static: refused while its room has an active lease (drain the room first: a removed machine has no settings left for the lease's cleanup); a free room is retired. On demand: refused while the machine has rooms (the provider destroys a room through its machine's settings); destroy them first with `hostel config rooms destroy <room>`. |
| Removed provider | Refused while it has on-demand rooms: remove its machines first. |

## 8. The status view (M10)

### Without a terminal

`hostel status` in a pipe, an agent, or with `--plain`:

```
acme · 4 rooms (2 in use) · 1 waiting                     you: Ana Pérez (member)

ROOM             PLATFORMS     STATE   USER         TASK                   TIME
home/macbook     ios, macos    in use  Alex E.    fix login screen       12m
home/win         windows       free
namespace/mac-1  ios           in use  Ana Pérez    iOS snapshot tests     41m
namespace/linux  android       none    (on demand, up to 2)

QUEUE    WAITING  EST.   NEXT
ios      1        ~15m   Lucía G. · sign-in flow (normal, 3m)
```

### In a terminal

The same lines, drawn inline under the prompt (not full screen), updated when
something changes (long poll on the status revision):

```
acme · 4 rooms (2 in use) · 1 waiting                     you: Ana Pérez (member)

  ROOM             PLATFORMS     STATE   USER         TASK                   TIME
❯ home/macbook     ios, macos    in use  Alex E.    fix login screen       12m
  home/win         windows       free
  namespace/mac-1  ios           in use  Ana Pérez    iOS snapshot tests     41m
  namespace/linux  android       none    (on demand, up to 2)

  QUEUE    WAITING  EST.   NEXT
  ios      1        ~15m   Lucía G. · sign-in flow (normal, 3m)

↑↓ select · enter details · q quit
```

`q` or Ctrl-C ends it; the terminal keeps the last frame.

Details of a room (Enter, or `hostel room <name>` as plain text):

```
home/macbook · static, connects to the server · macOS 26.1, arm64
Apple M2 · 8 cores · 16 GB · disk 182 GB free
Platforms ios, macos · labels xcode-26.5, desktop · 1 slot

NOW      Alex E. · fix login screen · lease 7f3a2c
         ready 12m ago · ends at 15:48 at the latest, or after 10m without activity
         from alex-mbp · repository exampleapp (branch fix-login)
         job j-91 running 3m: xcodebuild test -scheme ExampleApp

QUEUE    1. Lucía G. · sign-in flow · normal · waiting 3m · ~15m

EVENTS   14:31 lease 7f3a2c ready (prepare 18s)
         14:30 cleanup of lease 2b81e0 ok (6s)

esc back · e end lease · d drain · r repair (admin)
```

Details of a queue:

```
ios · 1 room in use, 0 free · median lease 22m

  #  USER        TASK               PRIORITY  WAITING  EST.
❯ 1  Lucía G.    sign-in flow       normal    3m       ~15m
  2  Alex E.   settings redesign  low       1m       ~37m

esc back · e end · shift+↑/↓ move · p priority (admin)
```

- The lease's jobs come from the room when the details open (the server asks
  the helper); the server does not store them.
- The hardware line comes from the probe (new: CPU, memory, disk).
- Each admin action asks for a confirmation and writes an event with the admin.

## 9. Queues

- One queue for each platform.
- Three lanes: `high`, `normal` (default), `low`. An agent sets the lane only
  when the user asks for it (as today). No aging: a lease stays in its lane.
- Inside a lane: first in, first out.
- A room that serves several platforms takes the first lease among their
  queues: the higher lane first, then the older request.
- An admin can move a lease inside its queue or change its lane. A moved lease
  keeps its place until it starts.
- The blocking rule stays (`design.md`, section 5): a lease that cannot start
  holds its candidate machines for itself, so a later lease cannot pass it on
  them.

## 10. Server changes (summary)

| Table | Change |
|---|---|
| `users` | new (M8): id, name (unique), role, created_at, removed_at |
| `tokens` | M8: add `user_id` and `computer` (user tokens); room tokens unchanged |
| `invites` | new (M8): id, kind (`user`, `computer`, M9: `machine`), secret hash, role, name, user_id, created_by, expires_at, used_at |
| `leases` | M8: add `user_id` and `manual_position` |
| `events` | M8: add `user_id` (who did it) |
| `config_revisions` | new (M9): revision, document, user_id, created_at |
| `platform_revisions` | new (M9): platform, revision, files (or removed), user_id, created_at |

| API (new) | Milestone |
|---|---|
| `POST /v1/connect` (invite code; no bearer token) → user, token | M8 |
| `GET /v1/me` | M8 |
| `GET/POST/PATCH/DELETE /v1/users...`, `POST /v1/invites` | M8 |
| `POST /v1/leases/{id}/move`, `/priority` (admin) | M8 |
| `GET /v1/status?since=R&wait=50s` | M10 |
| `GET /v1/rooms/{id}` (details with jobs) | M10 |
| `GET/PUT /v1/config`, `/v1/platforms/{name}/files` | M9 |
| `POST /v1/machines/join`, `/v1/providers/{name}/login` | M9 |
