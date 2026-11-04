# M10d implementation plan

Status: planned (2026-11-04). Milestone M10d in `plan.md`: updates.
Owner decisions: `requirements.md`, section 3, 2026-11-04.

## 1. Facts (2026-11-04, after v0.1.0)

- A release (`v*` tag) has the binaries, the pinned mise binaries,
  `hostel-platforms.tar.gz`, and `SHA256SUMS`. `install.sh` and `install.ps1`
  check each file against `SHA256SUMS`. Nothing signs a release.
- The CLI updates only when a person runs the install command again. The
  server updates the same way (`install.sh --server`), then a restart.
- The helper on each room follows the server: at each provisioning and
  `preparing`, the server compares hashes and sends the helper of `helper_dir`
  (design, section 8). On-demand rooms too.
- A machine that joined (`hostel machine join`) runs `hostel machine connect`
  from its own copy in `/etc/hostel` or `C:\ProgramData\hostel`. Nothing updates
  it.
- The guest and the server do not tell each other their versions (only
  `GET /v1/info` has the server version).

## 2. Decisions

- **Signed releases.** The release workflow signs
  `"hostel-release <tag>\n" + SHA256SUMS` with Ed25519 (`SHA256SUMS.sig`,
  base64). The private key is the repository secret `HOSTEL_RELEASE_KEY`; the
  owner keeps a backup. The public key is in the binary
  (`internal/update/key.go`). `hostel update` and the server update refuse a
  release without a valid signature, and a tag that is not newer. The tag in
  the signed text stops an old release that is served under a new tag. The
  install scripts keep `SHA256SUMS` only (the first install trusts HTTPS).
- **Release source.** `https://github.com/csansoon/hostel/releases`: the
  latest tag is the redirect of `<base>/latest` (`…/tag/vX.Y.Z`, no API rate
  limit); the files are `<base>/download/<tag>/<file>`. `HOSTEL_UPDATE_URL`
  and `HOSTEL_UPDATE_KEY` replace the base and the public key (a mirror; the
  E2E tests). Each request has a deadline (30 s for the tag, 10 min for a
  file).
- **Versions.** `vX.Y.Z`; a `git describe` version (`vX.Y.Z-N-gHASH[-dirty]`)
  is after `vX.Y.Z`. Other versions (`dev`) are unknown: they get no notice,
  and `hostel update` replaces them.
- **`hostel update [--check] [--version vX.Y.Z]`.** Reads the latest tag,
  downloads and verifies `SHA256SUMS`, its signature, and the binary for this
  OS and architecture; writes it next to the running binary, runs it with
  `--version`, and renames it over the running binary (Windows: the running
  file moves to `hostel.exe.old` first; a later start deletes it). A binary
  that it cannot write gives the command to run (`sudo hostel update`, or the
  install command). `--check` only prints the state. Exit codes: 0, 1 on an
  error, 69 when the release source cannot be reached.
- **Notice.** Each server answer has the header `X-Hostel-Version`. When the
  server is newer than the CLI, a CLI command prints one line on stderr, at most
  once in 24 hours: `hostel: Hostel vB is available (this computer has vA); run
  "hostel update"`. The time of the last notice is in the guest configuration
  directory (`update-notice`).
- **Version check.** Each guest request has `X-Hostel-Version`. The server
  refuses a known version older than its minimum guest version (`v0.1.0` now)
  with HTTP 426 and `update this computer: run "hostel update"`. A request
  without the header (v0.1.0, curl) passes. All 0.x versions work together
  until a change breaks the API; such a change raises the minimum.
- **Server update.** `update.Server` replaces, in this order, each file of
  `helper_dir` that differs from the release (`hostel-*`, `mise-*`; a file
  with the same hash stays), `platforms_dir` (from `hostel-platforms.tar.gz`,
  unpacked next to it and swapped with a rename), and the server binary. It
  checks first that the server's user can write each of them.
  - `hostel server update [--version v] [--config f]` on the server machine
    does it and prints the restart command.
  - `POST /v1/server/update {"version"}` (admin) starts it in the server (one
    at a time; 202). `GET /v1/server/update` returns
    `{"state": "idle|running|failed", "from", "to", "error"}`. After a
    successful update, the server stops as at a signal and starts the new
    binary in the same process (`exec`, macOS and Linux), with the same
    arguments and environment. Workers are idempotent (design, section 4) and
    guests retry, so leases continue. `hostel server update --remote` (an
    admin's computer) starts it and waits (15 min) until `GET /v1/info` has the
    new version, or the update fails.
  - The layout for remote updates: the service user owns the binary
    (`/opt/hostel/bin/hostel`), `helper_dir`, and `platforms_dir`
    (`install.sh --dir /opt/hostel/bin --server /opt/hostel`, then `chown`).
- **Machines that joined.** `hostel machine connect` (and `hostel room
  connect`) sends its version in the SSH client version
  (`SSH-2.0-hostel-<version>`); the gateway keeps it with the connection, and
  the status has it for each machine (`connect_version`). The app shows "Out of
  date" on a machine whose service is older than the server, with the command
  to run on it as an administrator: `sudo /etc/hostel/hostel machine update`
  (Windows: `C:\ProgramData\hostel\hostel.exe machine update`). `hostel machine
  update` downloads the server's binary for the machine (`/v1/download`, over the
  connection's pinned TLS), checks it with `--version`, replaces the binary of the
  service directory, and restarts the service. The service does not replace
  itself: it cannot write its directory, which protects the room token.
- **App.** In the background, at start and every 6 hours, the app reads the
  latest tag (15 s deadline; errors are silent; `HOSTEL_NO_UPDATE_CHECK=1`
  turns it off). When it is newer than the app, the main menu has a fourth
  entry, **Update** ("vB available"): Enter asks, then runs the same update as
  `hostel update`, and says to quit and open `hostel` again. For admins, when
  the latest tag is newer than the server, Front desk has **Update the
  server**: Enter asks, then starts the remote update and shows its state;
  the version in Reception changes when the server is back.

## 3. Build order (phases; each ends with review, tests, a commit)

1. **Decisions and plan.** This file, `requirements.md`, `plan.md`.
2. **Releases and `hostel update`.** `internal/update` (source, versions,
   signature, replace), the signing tool (`tools/signrelease`), the release
   workflow and key, `hostel update`, the notice and the version check.
3. **Server update.** `update.Server`, `hostel server update`, the API, the
   restart with `exec`.
4. **Machines that joined.** The version in the SSH client version, the
   status, `hostel machine update`.
5. **App.** The background check, Update, Update the server, Out of date.
6. Docs (`design.md`, skills, README), review, gates, done-when.

## 4. Tests

E2E, with a fake release source (an HTTP server in the test with the release
files of two test versions, built with `-X …version.Version`, and a test key):

- `TestUpdate`: `--check`; an update from v1.0.0 to v1.1.0; a wrong signature
  and a changed binary are refused and the binary stays; the latest version
  says "up to date".
- `TestUpdateNotice`: a newer server; the notice once, not on the next command.
- `TestGuestTooOld`: a request with an old version gets HTTP 426.
- `TestServerUpdate`: a server from a v1.0.0 copy; `hostel server update
  --remote` starts v1.1.0 in the same process; `helper_dir` and
  `platforms_dir` have the new files; a ready lease continues. The local
  command on a stopped server.
- `TestMachineUpdate` (in `TestMachineJoin`): the connect version in the
  status; `hostel machine update` in the container replaces the service binary.
- `TestAppUpdate`: the Update entry; Enter and y update the binary copy.

Done when: v0.2.0 is released and signed; on this Mac, a build with the version
v0.1.99 updates itself to v0.2.0 from GitHub (`hostel update`, and the app's
Update); an admin updates
the server of a test deployment from the app; a joined Linux machine shows
"Out of date" and `hostel machine update` fixes it.

## 5. Risks

- **The key.** A lost private key means a new key in a new release, and
  computers on the old key must install again. A leaked key lets an attacker
  sign releases: rotate it the same way.
- **exec on restart.** Open files and sockets close at `exec` (Go sets
  close-on-exec); the listen ports are bound again at once. A failed start of
  the new binary stops the server: systemd starts it again (`Restart=on-failure`).
- **Windows binaries in use.** A running `hostel.exe` cannot be replaced, only
  renamed (as for the helper, design, section 8).
