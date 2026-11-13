# M6 implementation plan

Status: in progress (2026-10-29). Milestone M6 in `plan.md`: Windows rooms and
several slots for each room. The Windows room is a Windows VM
(owner decision, 2026-10-29). This file gives the facts, the decisions,
the build order, and the risks. `design.md` stays the contract.

## 1. Facts from the experiments (2026-10-29)

The Windows VM: Windows 11 Enterprise 10.0.26300, amd64, 2 CPUs, 8 GB, 57 GB free. Not
in a domain. Reached at a private network address only. Agents of other projects use it
(with a `windows` desktop lock of those projects).

- **SSH.** OpenSSH Server runs (automatic start). The default shell of the SSH
  server is Windows PowerShell 5.1 (`HKLM\SOFTWARE\OpenSSH\DefaultShell`); this is
  one setting for all users. PowerShell 7 (`pwsh`), git, .NET, `tar`, and `curl`
  are installed.
- **User.** A local user `hostel` (group `Users`, not `Administrators`) with a
  random password that nobody knows logs in with a key in
  `C:\Users\hostel\.ssh\authorized_keys` (ACL: SYSTEM and `hostel` only). The
  profile must exist before the first SSH login; a start of `cmd /c exit` with the
  user's credentials creates it.
- **Probe.** `uname` does not exist. `cmd /c echo windows %PROCESSOR_ARCHITECTURE%`
  prints `windows AMD64` from the PowerShell default shell, as from `cmd`.
- **stdin.** A native program that the default shell starts gets the SSH stdin
  unchanged: 3,000,000 random bytes arrived with the same SHA-256.
- **Session job.** The SSH server puts each session in a job object with
  `KILL_ON_JOB_CLOSE` and `BREAKAWAY_OK` (limit flags `0x2800`). When the session
  ends, all its processes end, except a process that was started with
  `CREATE_BREAKAWAY_FROM_JOB`: it continues (checked).
- **Named jobs.** A user without administrator rights can create a named job
  object (`Local\...`) in an SSH session. The name disappears when the last handle
  closes, also while processes are still in the job. All SSH sessions are in
  Windows session 0.
- **Scp.** A relative target path of `scp` does not go to the home directory; use
  absolute paths (`/C:/Users/...`). Hostel does not use scp.

## 2. Decisions from the facts

- **Processes on Windows.**
  - A job supervisor starts with `CREATE_BREAKAWAY_FROM_JOB`, so it survives the
    SSH session (as `setsid` does on macOS and Linux).
  - The supervisor starts the command suspended, puts it in a job object named
    `Local\hostel-<user SID>-<slot>-<job id>` with `KILL_ON_JOB_CLOSE` and without
    `BREAKAWAY_OK`, and then resumes it: the command and all its descendants stay
    in the job (a breakaway fails with "Access is denied"; checked). The
    supervisor holds the only handle, so the name exists while the supervisor
    runs. `job stop` and cleanup open the job by name and terminate it. When the
    command ends, the supervisor ends, and the rest of the job ends with it: on
    Windows, a background process of a job does not outlive the job's command.
  - Processes of `exec` and `shell` (guest commands without a job) are in the SSH
    session's job, which ends them when the session ends. A process that breaks
    away (the session job allows it) keeps `HOSTEL_LEASE` in its environment.
  - Cleanup finds the other processes of the lease by `HOSTEL_LEASE` in their
    environment (read from the process memory: PEB, as `ps -E` and
    `/proc/<pid>/environ` do), and their descendants (parent PIDs from a Toolhelp
    snapshot; a child must be younger than its parent, because Windows reuses
    the PID of an ended parent). Start times (`GetProcessTimes`) guard against
    reused PIDs. Windows has no sessions in the Unix sense; that part of cleanup
    does not apply. Checked on the Windows VM: after a release, no process of the
    `hostel` user remained, also not a process that had broken away.
  - Windows has no SIGTERM for programs without a console. Stop and cleanup
    terminate at once (no 10 s grace time).
- **Environment.** Windows ignores the case of variable names (`Path` is `PATH`).
  The helper uses upper-case names on Windows when it merges the sources of the
  lease environment.
- **Shell of guest commands.** Guest commands, `shell = true` jobs, and setup
  commands run with PowerShell 7 (`pwsh`) if it is in the room's `PATH`, else
  Windows PowerShell 5.1, with `-NoProfile -NonInteractive -Command`. Not
  `-EncodedCommand`: with it, Windows PowerShell writes stderr as CLIXML. A
  wrapper makes the exit code that of the last command (PowerShell itself exits
  with 0 or 1), and turns off progress records and colors. The platform scripts
  (`.windows.ps1`) run with the same PowerShell, `-ExecutionPolicy Bypass -File`.
  The default setup job on Windows is `mise install`, then the `PATH` from `mise
  bin-paths`, then the setup commands, each stopped at the first failure
  (Windows PowerShell has no `&&`).
- **Server commands.** The default shell of the SSH server can be PowerShell or
  `cmd`. The probe finds it: `echo %OS%` prints `Windows_NT` in `cmd` and `%OS%`
  in PowerShell. The rooms table records it (`rooms.shell`). The server and the
  gateway quote for that shell (package `shell`); a PowerShell command line
  ends with `exit $LASTEXITCODE`, because PowerShell itself exits with 0 or 1.
- **First helper upload.** Windows PowerShell does not give its stdin to a
  script (`[Console]::OpenStandardInput()` waited without end). The server sends
  the helper as a `tar` stream to `tar.exe` (in Windows since 10 1803), which a
  native program receives unchanged, and then moves it into place with
  PowerShell.
- **Replacing executables.** Windows cannot overwrite a running `.exe`, but it can
  rename it. An install renames the old file to `.<name>.old-<random>`, renames
  the new one into place, and deletes old files that are not in use.
- **File modes.** Windows has no executable bit. On a Windows room, sync compares
  file content only and does not apply modes. Symbolic links need a privilege;
  a sync that contains a link fails on a Windows room.
- **`room connect` on Windows.** A scheduled task that runs as `LOCAL SERVICE` at
  boot, with the binary and the token in `C:\ProgramData\hostel` (only that
  account and administrators can read it). No Windows service code. A task as the
  `hostel` user was not possible: `Register-ScheduledTask` failed with "Access is
  denied" in an SSH session, and `schtasks /RU hostel` asks for the password.
- **The room token is not for the room user.** A lease runs as the room user; with
  the token, a guest could replace the room's connection. `room connect` runs as
  another account (design, section 13). The M5 setup of the home MacBook does not
  do this yet: its token is in the `hostel` user's home (owner action, section 6).
- **Slot users.** A slot of a static machine can have its own OS `user`. The
  server connects to that user for the slot's operations (begin, prepare, cleanup,
  guest commands); the machine user (`address`) does the room-level operations
  (probe, install, provision, health). Each slot user has its own `hostel/`
  directory, helper, machine-level tools, and `authorized_keys`. Health runs as
  the machine user, also after the cleanup of a slot with its own user (the
  provision result is in the machine user's account; found in the E2E test).
  Platforms whose `prepare` needs that result (the iOS golden simulators) do not
  work with slot users (design, section 9).
- **Forwards.** On a room with several slots, the gateway allows `direct-tcpip`
  only to the ports of the lease's slot (an exclusive lease: any port), and a
  tunnel cannot use another slot's port. On a room with one slot, any port stays
  allowed (for example a bundler on 8081). Slots still share the room's loopback
  interface: a guest command can reach the other slot's servers (design,
  section 14; review finding).
- **Guest commands on Windows.** `exec` and `shell` put the helper in a job
  object without breakaway before they start the command (review finding: a
  process that broke away from the SSH session and cleared its environment
  escaped cleanup). The helper's own `job start` is the exception: the `exec`
  helper runs it itself, outside that job, because the job supervisor must break
  away.

## 3. Build order

Each phase: plan, build, review, fix, check, commit, and push to the draft pull
request.

1. **Windows helper.** The process layer (`proc_windows.go`): start times, parent
   PIDs, environment, job objects. Jobs, `exec`, `shell`, scripts, `lookPath`
   with `PATHEXT`, installs that rename, sync without modes. `go vet` for
   `GOOS=windows`.
2. **Windows on the server, gateway, and guest.** Probe, command quoting for
   Windows rooms, the first helper upload, the helper path. A test platform
   directory for `windows`. An opt-in smoke test (`HOSTEL_SMOKE_WINDOWS=<address>`)
   against a static Windows room: acquire, sync, run, a detached job, job stop, a
   tunnel, `cp`, cleanup of a process that the lease left, and of a process that
   tried to break away.
3. **Windows in CI.** An E2E job on a GitHub Windows runner with its own OpenSSH
   server, if the runner can start one in a few minutes. Else the smoke test stays
   the Windows check, and the owner accepts that.
4. **The Windows VM through the VPS.** `room connect` as a scheduled task; the machine
   in the production server's configuration; `deploy-vps.md` for Windows.
5. **Several slots.** Slot `user`; forwards limited to the slot; E2E scenarios: two
   leases at the same time on one Docker room with two slot users, each with its
   own files, processes, and ports, and cleanup of one that does not touch the
   other; an exclusive lease that waits for both slots and then takes them.
6. **Docs, review, gates.** `design.md`, `static-room-setup.md` (Windows),
   results in this file, the 10-run gate, and the local run.

## 4. Done when

- A guest on a Mac acquires `windows` through `https://hostel.example.com`
  on the Windows VM, syncs a repository, runs a .NET build, a detached job, and a
  tunnel. After the release, no process of the lease runs on the Windows VM.
- The E2E suite has the slot scenarios of phase 5, and it passes in the 10-run
  gate and once on a Mac.
- The Windows smoke test passes on the Windows VM (and the Windows CI job, if phase 3
  makes one).

## 5. Risks

| Risk | Control |
|---|---|
| Hostel work slows down the agents of other projects on a 2-CPU machine. | One slot. The owner can drain the machine (`hostel admin`) while those agents need it. |
| A guest process escapes the jobs (for example through WMI or a scheduled task). | Cleanup also finds it by `HOSTEL_LEASE` in its environment. A process that clears its environment and is started outside the lease's process tree escapes, as on macOS (design, section 18). |
| Microsoft Defender blocks or slows down the unsigned helper. | Not seen in the experiments. If it happens, the owner adds an exclusion for `C:\Users\hostel\hostel`. |
| The Windows VM restarts or sleeps. | `room connect` starts at boot; the room is unreachable until then, and the lease returns to the queue (design, section 6). |

## 6. Results (2026-10-29)

- **Windows smoke test** (`make test-smoke-windows` against the Windows VM): passed
  in 203 s. Lease environment, exit codes (native, PowerShell, through `ssh`),
  tunnel, forward, `cp` both ways, sync of a change, `diff`, `job stop`, a
  breakaway that a job refuses ("Access is denied"), and one from an SSH session
  that cleanup then stopped; a second lease found no file and no process of the
  first.
- **Slot users** (`TestSlotUsers`, E2E with two slot users in one Docker room):
  passed. The first run found that health ran as the slot user; fixed.
- **Done-when, Windows part:** from a Mac through
  `https://hostel.example.com`, a `windows` lease on the Windows VM was ready in 15 s; a
  .NET 10 build and run, a tunnel ("hello from the guest mac"), and a detached
  job worked. After the release, the `hostel` user had no process; only `room
  connect` (as `LOCAL SERVICE`) ran.
- **Owner action:** move the home MacBook's room token and its `room connect`
  service out of the `hostel` user (`deploy-vps.md`, section 6).
