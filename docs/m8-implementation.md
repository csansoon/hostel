# M8 implementation plan

Status: planned (2026-10-30). Milestone M8 in `plan.md`: users and connection.
Design: `multiplayer.md`, sections 3 to 6 and 9. Owner decisions:
`requirements.md`, section 3, 2026-10-30. When a step is built, its contract
moves into `design.md`.

## 1. Facts from the code (2026-10-30)

- Tokens (`tokens`: name, role `guest|admin|room`, machine, hash) are the only
  identity. `leases.token_id` links a lease to a token. `authWith` in
  `internal/server/server.go` checks the bearer token.
- The client reads `server` and `token` from `~/.config/hostel/config.toml`, or
  `HOSTEL_SERVER` and `HOSTEL_TOKEN` (`internal/config/guest.go`). The E2E harness
  uses the variables.
- The server listens with plain HTTP (`listen`). TLS comes from a proxy
  (exe.dev). The gateway is an SSH port (`gateway_listen`) and SSH in WebSocket
  on `/v1/gateway`; `gateway_public` tells the guest which one to use.
  `internal/gateway/dial.go` dials both; `hostel ssh-config` uses
  `hostel gateway-proxy wss://...` as the ProxyCommand.
- Lease priority is `low|normal|high` with aging (`aging`, default 10 minutes;
  `scheduler.EffectivePriority`). The scheduler is a pure function with unit
  tests.
- Migrations: `internal/store/migrations/001_initial.sql`, `002_m6.sql`.

## 2. Decisions

- **Users.** `users(id, name UNIQUE COLLATE NOCASE, role, created_at,
  removed_at)`. A user token has `user_id` and `computer`. Room tokens have no
  user. Role names: `admin`, `member` (the token role `guest` becomes `member`).
- **Migration of existing tokens.** Migration 003 makes one user for each
  distinct name of a `guest` or `admin` token that is not revoked; tokens with
  the same name join the same user; the role is `admin` if one of them is admin.
  Existing clients keep their tokens and keep working.
- **TLS.** `server.toml`: `tls = "self-signed" | "off"` (default `self-signed`
  for a new server; `off` for a server without the key, so existing servers do
  not change) and `public_urls = [...]`. `hostel server init` makes an ECDSA
  P-256 key and a self-signed certificate (valid 20 years; the client pins the
  key, not the certificate). With `self-signed`, `listen` serves HTTPS, and the
  gateway is reachable on `/v1/gateway` of the same address (`gateway_public`
  becomes that `wss://` URL unless it is set).
- **Pinning.** The client stores `key = "sha256:<base64>"` (SHA-256 of the
  certificate's SubjectPublicKeyInfo) and checks it in `VerifyPeerCertificate`
  for the API and the gateway WebSocket (`gateway-proxy` too). Without `key`
  (TLS `off`), the client uses the system roots.
- **Invite code.** `hostel1-` + base32 (no padding, lower case) of: version byte,
  flags, 32-byte key hash (absent with TLS `off`), 16-byte secret, then the
  addresses (length-prefixed UTF-8). A unit test is allowed for the encoding
  (small, deterministic; E2E tests cannot cover all malformed codes).
- **Invites.** `invites(id, kind user|computer, secret_sha256, role, name,
  user_id, created_by, created_at, expires_at, used_at)`. `POST /v1/connect`
  `{secret, name, computer}` uses the invite in one transaction (compare-and-set
  on `used_at IS NULL AND expires_at > now`) and returns the user, the token,
  and the server name. Failed attempts: at most 10 per minute per source
  address (HTTP 429).
- **Client configuration.**

  ```toml
  server_name = "acme"
  addresses = ["https://100.80.1.2:7410"]
  key = "sha256:q3J1...e8Q"
  token = "..."
  ```

  The old form (`server`, `token`) is still read. `HOSTEL_SERVER`,
  `HOSTEL_TOKEN`, and the new `HOSTEL_SERVER_KEY` override the file. Without a
  connection, every command except `connect`, `help`, `--version`, `server`,
  and `machine`/`room` (room machines) prints the "not connected" message (exit
  code 78).
- **Queues.** The scheduler order becomes: manual position first (admin moves),
  then lane (`high`, `normal`, `low`), then request time. `aging` is removed
  (an `aging` key in `server.toml` gives a clear error: "removed in M8"). The
  blocking rule stays.
- **Admin lease actions** (API in M8; the view in M10):
  - `POST /v1/leases/{id}/release` by an admin for another user's lease: end
    reason "released by <admin> (admin)".
  - `POST /v1/leases/{id}/move` `{before: <lease id>|"", last: bool}` (queued
    leases of the same platform).
  - `POST /v1/leases/{id}/priority` `{priority}`.
  - CLI in M8: `hostel release <lease>` (admin: any lease). Move and priority
    come with the view (M10); the E2E tests use the API.
- **The guest prints the reason.** A lease that an admin ended or moved shows the
  reason in `acquire`, `run`, and `status` (from the lease's `reason` and
  events).
- **`hostel server init`.** Asks for the server name, the public addresses, and
  the owner's name (flags for each, for scripts and tests); writes `server.toml`
  with `tls = "self-signed"`; makes the first admin user and invite; prints
  `hostel connect <code>`. It no longer writes the owner's guest config: the
  owner connects with the code. `--service` installs the service (systemd or
  launchd) as today in `deploy-vps.md`.

## 3. Build order

1. Store: migration 003 (users, invites, token and lease and event columns),
   the token migration; store functions.
2. Server auth: the token's user and role in the request context; role checks
   (`member` and `admin`); room tokens unchanged.
3. TLS: `tls`, `public_urls`, the certificate at init, HTTPS on `listen`, the
   gateway on the same address.
4. Invite codes (encoding, unit test), `POST /v1/invites`, `POST /v1/connect`,
   `GET /v1/me`, the rate limit.
5. Client: the connection file, the pinned HTTP client and gateway dial,
   `hostel connect`, `hostel disconnect`, the "not connected" message.
6. `hostel server init` (questions and flags) and the first invite.
7. `hostel config users` (list, `invite`, `code`, `set`, `remove`), the API, the
   rule of the last admin, the removal of a user's leases.
8. Users on leases, events, and `hostel status` (USER column); the reason for
   admin actions in the guest.
9. Scheduler: lanes without aging, manual position; admin move, priority,
   release; `hostel release <lease>` for admins.
10. Docs (`design.md` sections 3, 4, 5, 9, 11, 13, 18; `deploy-vps.md`;
    `README.md`), agent guide; review; gates; done-when.

## 4. Tests

E2E (Docker rooms; each server in the harness uses TLS `self-signed` unless
the test needs `off`):

- `TestConnect`: an admin invites; a client with a new home connects with the
  code; `hostel status` works with the pinned key. A second use of the code, an
  expired code, and a wrong secret are refused. A server with another
  certificate on the same address is refused (pinning). `disconnect` removes the
  token on the server. A client without a connection prints the message.
- `TestConnectProxy`: TLS `off` behind a small TLS proxy in the test (system
  roots from a test CA through `SSL_CERT_FILE`): connect and a lease work.
- `TestUsersRoles`: a member cannot invite, change the configuration, or release
  another user's lease; an admin can; the last admin cannot be removed or
  demoted; removing a user ends their lease (cleanup runs) and removes their
  queued lease.
- `TestQueueLanes`: with one room busy, leases in the lanes come out high, then
  normal, then low, first in, first out in each lane; an old `low` lease stays
  behind a new `normal` lease (no aging); an admin move puts a lease first; the
  moved user's client prints the reason.
- `TestTokenMigration`: the server starts on a database at migration 002 with
  two `guest` tokens of the same name and one `admin` token; after the start,
  there are two users, both clients work, and their leases show their users.
- Existing tests: the harness connects its clients with invite codes (one
  admin, and members where a test needs them).

Unit: the invite code encoding; the scheduler order (lanes, manual position, no
aging).

Smoke (owner's done-when): the exe.dev server (TLS `off`, behind its proxy) and
a test server on the owner's Mac reached by its LAN or Tailscale IP (TLS
`self-signed`).

## 5. Risks

- **The exe.dev upgrade.** Its `server.toml` has no `tls` key, so it stays `off`.
  Its existing tokens become users by the migration. Check with `hostel status`
  from the owner's client and the room tokens of `home/macbook` and `home/win`
  after the deploy.
- **Clients of other projects.** T3 Code and the agents of other projects use
  `HOSTEL_SERVER` and `HOSTEL_TOKEN`; these keep working.
- **WebSocket gateway on HTTPS.** Long SSH sessions over the WebSocket on the
  API listener need the same timeouts as on exe.dev (checked in M5); with TLS
  `self-signed`, the E2E tests use this path.
