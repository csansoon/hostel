# M9 implementation plan

Status: built (2026-11-02); phases 1 to 5 done, done-when open. Milestone M9 in `plan.md`: remote configuration.
Design: `multiplayer.md`, section 7. When a step is built, its contract moves
into `design.md`.

## 1. Facts from the code (2026-11-02)

- `serverRun` loads `server.toml` once (`config.LoadServer`) and gives the
  `*config.Server` to `server.Run`. Nothing replaces it later: the server has no
  lock around it.
- At start, the server builds the providers (`newProviders`: a Namespace
  provider checks its login, an external provider loads its capabilities), the
  platform uploads and hashes (`loadPlatformUploads`), the static rooms
  (`ensureStaticRooms`: a room for each static machine; a room whose machine is
  gone is `retired`), and the loop ticker.
- The providers keep a pointer to their `config.Provider` and read it on each
  call. The rest of the server reads `s.cfg` on each use (about 40 places). The
  scheduler gets fresh values on each pass.
- Platforms come from `platforms_dir` (`config.LoadPlatforms`); the release
  ships `platforms/{android,ios,macos,windows}` as a tar file. A room records the
  hash of each platform that it provisioned (`room_platforms`); the scheduler
  uses a room only when the hashes match. A static room provisions again only at
  start, on repair, or when it connects again: changed scripts need a restart.
- A machine joins by hand (`static-room-setup.md`, `deploy-vps.md` section 6):
  a user `hostel`, the server key in `authorized_keys`, `hostel admin token
  create --role room`, and a service that runs `hostel room connect`.
- Namespace: the server only checks the login at start (`devbox auth
  check-login`, `nsc auth check-login`); the login is a manual `nsc login`.
- `install.sh` downloads from a GitHub release. The server already has the
  binaries for all OSes in `helper_dir`.

## 2. Decisions

- **Two parts of the configuration.**
  - `server.toml` keeps what the server needs before it can open its database
    or what is tied to the machine: `server_id`, `listen`, `gateway_listen`,
    `gateway_public`, `tls`, `public_urls`, `data_dir`, `helper_dir`.
  - The **document** in the database has everything else: `[providers...]`
    with their machines, and the settings (`idle_timeout`, `poll_timeout`,
    `limit_retry`, `port_process_offline`, `intervals`, `mise`). It is TOML with
    today's keys. `config_revisions(revision, document, user_id, created_at)`;
    the latest revision applies.
  - The server validates the two parts together with today's `LoadServer`
    rules. A key of one part in the other part is an error with the key path.
- **Import (first start of M9).** If the database has no revision, the server
  takes the providers and settings of `server.toml` as revision 1, writes the
  old file to `server.toml.pre-m9`, and writes `server.toml` again with the
  local keys only (with a comment that says where the rest went). Platforms in
  `platforms_dir` that differ from the defaults become own platforms
  (revision 1). After the import, `platforms_dir` is not read again; the key
  gives a warning.
- **Live apply.** The server holds the current configuration in an atomic
  pointer (`s.config()`); each worker step takes one snapshot. A new revision:
  1. validates (and, for a new or changed Namespace provider, checks its login;
     for an external provider, loads its capabilities) without a lock and
     without a transaction;
  2. saves the revision with a compare-and-set on the base revision;
  3. builds the providers again, recomputes the platform uploads, runs
     `ensureStaticRooms`, and wakes the scheduler.
  Intervals apply at the next tick. `listen` and the other local keys still
  need a restart.
- **Rooms after a change.** A running room whose platform hashes or tools
  changed goes to `provisioning` when it has no lease (the room worker checks
  it after each lease). A room whose machine is removed gets no new leases;
  after its leases, a static room is `retired` and an on-demand room is
  destroyed (today's `retired` path, now also at run time).
- **Platforms in the database.** `platform_revisions(platform, revision, files,
  removed, user_id, created_at)`; `files` is a JSON map of file name to
  content. The defaults are embedded in the binary (`platforms/` becomes a Go
  package with `go:embed`). The effective set: each default unless a revision
  replaces or removes it, plus the own platforms. The server writes the
  effective set to `data_dir/platforms` (a new directory for each change, then
  a rename), so `LoadPlatforms`, the hashes, and the uploads stay as they are.
- **API.** All `GET` routes for members and admins; changes for admins only.
  For members, the values of `env` and `command_env` are hidden (they can hold
  secrets).
  - `GET /v1/config` → `{revision, document}`; `PUT /v1/config`
    `{base_revision, document}` → the new revision or the errors (HTTP 422,
    each with its key path); `GET /v1/config/history`.
  - `GET /v1/config/platforms`; `GET/PUT/DELETE /v1/config/platforms/{name}`
    (files; `DELETE` of a default resets it, of an own platform removes it;
    refused while a machine lists it).
  - `POST /v1/machines/invites` (admin) → a machine code; `POST
    /v1/machines/join` (the code, no bearer token) → the room token and the
    server key.
  - `POST /v1/providers/{name}/login` → the login address, then the result.
- **CLI.** The targeted commands change the document on the client and upload
  it with its base revision (the same path as `config edit`), so the server has
  one way to change the configuration.
  - `hostel config` (the document with its revision), `config edit`,
    `config apply <file>`, `config history`.
  - `config settings [set <key>=<value>]`.
  - `config rooms [add|set|remove|drain|undrain|repair]` (`drain`, `undrain`,
    and `repair` move from `hostel admin room`).
  - `config providers [add|remove|login]`.
  - `config platforms [new|pull|push|reset|remove]`.
- **Machine join.** `config rooms add <name> [--platform p] [--label l]` adds
  nothing yet: it makes a machine invite (kind `machine`, 24 hours, with the
  name, platforms, and labels) and prints one command for each OS. The command
  downloads the binary from the server (`GET /v1/download/hostel-<os>-<arch>`,
  no token; the TLS key is pinned with `curl --pinnedpubkey` or the PowerShell
  equivalent) and runs `hostel machine join <code>` as an administrator. Join:
  1. creates the user `hostel` (no administrator rights) if it is missing;
  2. installs the server's public key for that user;
  3. sends the OS, architecture, and host name with the code; the server adds
     the machine (`connect = true`, provider `machines`, a static provider that
     the server adds if it is missing) in a new revision and returns a room
     token;
  4. writes the token where only the service account can read it, and installs
     the service that runs `hostel machine connect` (macOS: LaunchDaemon as
     root with `UserName` of a service account; Linux: systemd unit as user
     `hostel-connect`; Windows: scheduled task as LOCAL SERVICE), as in
     `deploy-vps.md` section 6;
  5. prints the steps that only a person can do (Xcode, the Mac desktop, the
     Windows desktop task).
  `hostel room connect` stays as an alias of `hostel machine connect`.
  `--no-service` (for tests and for people who run the service themselves)
  skips step 4 and prints the token file path.
- **Provider login.** `config providers login namespace`: the server runs
  `devbox login --browser=false` and `nsc login --browser=false` (deadline 10
  minutes), sends the address to the client, and reports when the login is
  done. Credentials stay on the server. The login belongs to the CLI on the
  server, not to a provider: the server refuses a Namespace provider before
  the login.
- **Changes found while building** (design.md has them): a machine with an
  active lease cannot be removed (drain it first); a free room whose machine
  settings changed provisions again (static) or is destroyed (on demand); the
  default provider of joined machines is `home`; the join service runs from
  the service directory; on macOS it runs as root.

## 3. Build order (phases; each phase ends with review, tests, a commit)

1. **Live configuration.** `config_revisions`; the split of `server.toml`;
   the import; `s.config()` and the apply steps; rooms after a change;
   `GET/PUT /v1/config`, history; `hostel config`, `edit`, `apply`,
   `history`, `settings`.
2. **Platforms.** The embedded defaults; `platform_revisions`; the effective
   directory; re-provisioning after a change; `config platforms ...`.
3. **Targeted room and provider commands.** `config rooms` (list, `set`,
   `remove`, `drain`, `undrain`, `repair`), `config providers` (list, `add`,
   `remove`).
4. **Machine join.** Machine invites, the download route, `install` commands,
   `hostel machine join` and `machine connect`, the services for each OS.
5. **Provider login** (Namespace).
6. Docs (`design.md`, `deploy-vps.md`, `static-room-setup.md`, `README.md`,
   agent guide), review, gates, done-when.

## 4. Tests

E2E (Docker rooms):

- `TestConfigLive`: change a machine's labels and a setting with
  `hostel config apply`; a new lease uses them without a restart; a change with
  an old base revision is refused; an invalid change gives the key path; a
  member cannot change the document and sees `env` values hidden; history lists
  the revisions with the users.
- `TestConfigImport`: a server with providers in `server.toml` (as before M9)
  starts: revision 1 has them, `server.toml.pre-m9` exists, `server.toml` has
  only local keys, and the room works.
- `TestPlatformsLive`: `config platforms new` and `push` add a device platform;
  a lease for it runs on a room that lists it, after the room provisions
  again; `pull` gives the same files; `reset` and `remove` work; `remove` is
  refused while a machine lists the platform.
- `TestConfigLive`, `TestConfigRooms`: the removal of a machine with an active
  lease is refused (drain first); a free removed machine's room is `retired`.
- `TestMachineJoin`: in a Linux room container, `hostel machine join <code>
  --no-service` creates the user and the key; then `hostel machine connect`
  runs; the room appears and a lease runs on it. The Linux service file is
  checked in the same test (systemd is not in the containers).
- Existing tests: the harness writes the document with `hostel config apply`
  (or the import, for tests of the old form).

Smoke (owner's done-when): from a Mac, without a shell on the server:
`hostel config rooms add` and the printed command on a new machine; a new
platform with `config platforms new` and `push`; a lease for it on that machine;
`config rooms set` for its labels; `config rooms remove`.

## 5. Risks

- **The production server upgrade.** The import changes `server.toml` on the VM (a copy
  stays). Check the rooms `home/macbook` and `home/win` after the deploy.
- **Service installation on three OSes.** Only the Linux path runs in the E2E
  tests; macOS and Windows get a smoke test on real machines.
- **Scope.** M9 is larger than M8. If it grows, provider login (phase 5) can
  move to a later milestone; the owner decides.
