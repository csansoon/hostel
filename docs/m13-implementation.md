# M13. Remote machine update (plan)

Status: implemented (section 8). Owner decisions of 2026-11-24 (section 2).

## 1. Why

A machine that joined (`hostel machine join`) runs the connect service from a
directory that only administrators can write. Until M13, only an administrator
on the machine could update it (`hostel machine update`, M10d). An admin with
several machines had to sign in to each one after each release.

## 2. Owner decisions (2026-11-24)

- An admin updates a connected machine from the client: Hostel ends the active
  leases of the machine, updates the software on the machine, and starts it
  again, with no step on the machine.
- After a new release, an admin updates everything from Front desk in the
  Hostel app: the server, then all connected machines.
- The machine installs only signed releases: the same signature check as
  `hostel update`, with the release key of the binary that is installed on the
  machine. A server that someone took over cannot install its own code as root.
  A server that runs a build that is not a release cannot update machines
  remotely; an administrator on the machine runs `hostel machine update`.
- The release v0.5.0 waits for M13, so that each machine needs only one more
  manual update (the one that installs the root step of section 3).

## 3. The root step on the machine

The update replaces a binary that only administrators can write, so it runs as
root (SYSTEM on Windows). The server reaches the machine as the machine user
(`hostel`), as for every lease step. The machine user may start one fixed
command, without arguments that it chooses:

- **macOS and Linux.** `/etc/sudoers.d/hostel-update`:
  `hostel ALL=(root) NOPASSWD: /etc/hostel/hostel machine update --signed`
  (checked with `visudo -cf`). The server runs
  `sudo -n /etc/hostel/hostel machine update --signed`.
- **Windows.** No sudo. The SYSTEM service of M11w (`HostelLeaseUsers`, named
  pipe `\\.\pipe\hostel-lease-user`, only SYSTEM and the machine user can open
  it, the client is checked) gets the operation `{"op": "update"}`. The join
  installs the service on each Windows machine (also without `--lease-users`;
  the lease-user operations still need `server_key.pub`, which only
  `--lease-users` writes). The server runs
  `C:\Program Files\Hostel\hostel.exe machine update --signed`, which is only
  the pipe client (the machine user cannot read `C:\ProgramData\hostel`).
- `hostel machine join` installs the root step. `hostel machine update` (as an
  administrator, M10d) also installs it, so one manual update after this release
  makes the machine remote-updatable.

`machine update --signed` (as root, or in the SYSTEM service):

1. Reads `join.json` (root-owned): the server address and its TLS key.
2. Downloads from the server `RELEASE` (the tag), `SHA256SUMS`, and
   `SHA256SUMS.sig` (`/v1/download/...`), and checks the signature with the
   release key of the installed binary (or the root-owned file
   `release-key` in the service directory, for a mirror with its own key, and
   the E2E tests). A tag that is not newer than the installed version is
   refused (no downgrade to an older signed release).
3. Downloads the binary for the machine, checks its SHA-256 against
   `SHA256SUMS`, and checks that it prints the tag (`--version`).
4. Replaces the binary of the connect service (on Windows also the binary of
   the SYSTEM service in `C:\Program Files\Hostel`), installs the root step
   again (a new release can change it), prints the result, and restarts the
   connect service (Windows: also the SYSTEM service, through its recovery
   action).

## 4. The server

- `update.Server` (server update) also writes `RELEASE`, `SHA256SUMS`, and
  `SHA256SUMS.sig` into `helper_dir`; `/v1/download/` serves them. A server
  without them (a build that is not a release) refuses a remote machine update
  with a message.
- `POST /v1/machines/{name}/update` (admin): only a machine with `connect =
  true`, and only when the server has a release. The machine row gets
  `update_to` (the tag) and keeps its drain state; it is drained.
- Worker (each step can repeat after a restart of the server):
  1. Ends each active lease of the machine (reason "machine update by
     <admin>"); queued leases wait.
  2. Waits until the cleanup of each room ends.
  3. Machine state `updating`. Runs the root step over SSH (deadline 10
     minutes); a connect version that equals `update_to` already skips it.
  4. Waits until the machine connects with the new version (10 minutes).
  5. `provisioning` (the probe, the new helper), then `running`. The drain goes
     back to its earlier state, and `update_to` is cleared.
  A failure makes the machine `broken` ("update failed: ..."), and the drain
  goes back to its earlier state at once; a repair makes the machine usable.
- `POST /v1/server/update` takes `"machines": true`: after the restart with the
  new release, the server updates each connected machine whose connect version
  is older (a flag in the database, so it survives the restart). All machines
  update at the same time: each one has its own leases.

## 5. Client and app

- `hostel config machines update <machine>...` and `--all` (the connected
  machines that are out of date). The command waits and prints each machine's
  result (`--no-wait` returns at once).
- Front desk: "Update everything" when a newer release exists: the server, then
  all connected machines. A machine that is out of date has "Update" on its
  page instead of the command for an administrator.

## 6. E2E scenarios

- A joined Linux machine (container, `--no-service`; the test restarts the
  connect process as a service manager would): an admin runs `hostel config
  machines update`; an active lease ends with the reason; the server runs the
  root step; the machine connects with the new version and takes the next
  lease. The drain state stays.
- A server that is not a release refuses, with the message.
- A release with a bad signature, or an older tag: the root step refuses; the
  machine is `broken` with the message, and a repair makes it usable.
- The machine user cannot run the root step with other arguments (sudo
  refuses).
- Server update with `machines: true`: the server restarts with the release,
  then updates the machine.
- The app: Update on the machine page; Update everything in Front desk.
- Real machines (not in the suite): the Windows machine and the Mac, after one
  manual update.

## 7. Risks

- A guest that runs as the machine user can also start the root step. It can
  only install the newer signed release that the server offers, which is what
  the admin would do; it can restart the connect service (the guest can already
  disturb its own lease).
- The update restarts the connect service, which carries the SSH session of the
  update: the root step prints its result before the restart, and the server
  waits for the new connection, not for the end of the command.

## 8. Implementation notes

- The step on the machine waits for the connection: after a server restart
  (Update everything), the machine connects later. The step runs when the
  machine connects with an older version, again after 30 s if it still has
  it, at most 3 times; the whole update has 10 minutes.
- During an update, `drain`/`undrain` change the state that the machine gets
  after the update; the machine stays drained until then.
- The app's machine actions (drain, repair, update) are also for azure
  machines now (before: static only).
- The release v0.5.0 is the first one with M13; machines that joined before
  need one manual `hostel machine update` (as an administrator on them).
- Windows: the code compiles and follows the M11w service, but it was not run
  on a real Windows machine yet (the E2E suite has Linux machines only). The
  first check is on the Windows machine after v0.5.0: one manual update, then
  a remote update (with the release, the root step answers "up to date" and
  restarts the connection; the full path at the next release).
- Review fixes (subagent, 2026-11-24):
  - The root step checks the SHA-256 of the server's binary before it runs it
    (`--version`); before, a server that someone took over could run its own
    program as root with copied signed files. The E2E test serves a script
    and checks that it did not run as root.
  - The root step sets its own `PATH` (macOS sudo keeps the caller's).
  - On Windows the SYSTEM service downloads into `C:\Program Files\Hostel`,
    checks there, and copies only the checked bytes; it never runs or copies
    the binary in `C:\ProgramData\hostel`, which the connection's account can
    change. The installed version is the version of the running root step.
  - The end of an update (state and drain) is one transaction.
  - Each loop pass ends the leases of a machine that waits for its update (a
    lease that the scheduler assigned meanwhile, or after a restart).
  - A connection with a newer version than the server's release is up to
    date; the step waits for "not older", not for the same tag.
  - Tests: another release key, an older release, a drained machine, and the
    app's Update everything (machines).
- Second review (2026-11-24):
  - An administrator's `hostel machine update` (and the join) gives the
    SYSTEM service only a copy of a file that only administrators can change
    (a private temporary directory, or the running binary), never the file in
    `C:\ProgramData\hostel`.
  - On Windows a failed replacement of the service's binary starts the
    connection again, and a restart ends the old connect process first (a
    running task ignores `/Run`).
  - An update that waits while its machine leaves `running` (a failed
    cleanup, a deallocation) is cancelled, with an event, and the machine gets
    its drain state back; the admin starts it again.
  - Tests: sudo refuses with its own message; a `release-key` that the machine
    user owns is refused.
- Limits on Windows: a remote update replaces the binary of the SYSTEM
  service but does not change the service's settings (a release that changes
  them needs a manual update). While the service restarts after an update
  (about 5 s), a process of the machine user can take the pipe's name; then
  the service cannot start until that process ends (no access is gained: the
  client checks that SYSTEM owns the pipe).
- The E2E tests use the root-owned `release-key`; the release key in the
  binary is used only with real releases.
- After the release v0.5.0 (2026-11-24):
  - An update runs the code of the installed binary. A server that updated
    from v0.4.0 had no release files in `helper_dir` (they come with its next
    update, or by hand: server setup skill, section 9). A machine that updated
    by hand from v0.4.0 needs a second `hostel machine update`, which installs
    the root step.
  - The first check on a real Windows machine found that the machine user
    cannot read `C:\ProgramData\hostel` (only SYSTEM, the administrators, and
    LOCAL SERVICE). The server now starts the step with the SYSTEM service's
    copy in `C:\Program Files\Hostel`, which all users can run. The exact
    command of the server, as the machine user, answered "up to date (v0.5.0,
    the server's release)".
