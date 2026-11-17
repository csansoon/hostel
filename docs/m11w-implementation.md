# M11 on Windows: lease users and the cleanup audit

## 1. Why

M11 gave macOS and Linux machines lease users and a cleanup audit
(`m11-implementation.md`). Windows machines still run each lease as the machine
user, and the audit does nothing there. So a scheduled task, a `Run` key, a
file in the Startup folder, or a changed PowerShell profile can survive cleanup.

## 2. Owner decisions (2026-11-16)

1. Windows gets lease users and the cleanup audit, as on macOS and Linux
   (`requirements.md`, section 3).
2. **The root step is a Hostel service that runs as SYSTEM**, with a named pipe that
   only the machine user can open, for `lease-user` only. `hostel machine join
   --lease-users` installs it.
3. `windows-desktop` leases keep the desktop user (`machine_user = true`) and get
   the audit.

## 3. The root step

- **Service.** A Windows service `HostelLeaseUsers` (LocalSystem, automatic start)
  runs `C:\Program Files\Hostel\hostel.exe lease-user serve`. Only SYSTEM and
  Administrators can write that directory. It is not `C:\ProgramData\hostel`,
  because the account of the connection (LOCAL SERVICE) can change files there and
  updates its binary there. So, as on macOS and Linux, `machine join` installs the
  root binary, and an update of the machine does not change it.
- **Pipe.** `\\.\pipe\hostel-lease-user`. Its security descriptor gives SYSTEM full
  access and the machine user read and write; nobody else can open it. The service
  creates the first instance (no other process can own the name while the service
  runs) and rejects remote clients.
- **Caller check.** The service also checks the client's token: the caller must be
  the machine user (`hostel`). The client checks that the server end of
  the pipe is a SYSTEM process, so a process that takes the name while the service
  is stopped cannot answer for it.
- **Protocol.** One JSON request (`{"op": "create", "name": "hostel-…"}`) and one
  JSON answer for each connection. The answers are the same as the `lease-user`
  output on macOS and Linux (`protocol.LeaseUserCheck`, `LeaseUserList`,
  `LeaseUserCreated`).
- **Client.** On Windows, `hostel.exe lease-user check|list|create|delete` (the
  machine user's helper binary) is the pipe client. The server runs it as the machine
  user over SSH, where it runs `sudo -n /etc/hostel/hostel lease-user` on macOS and
  Linux.

## 4. Operations (as SYSTEM)

The rules from macOS and Linux stay: only names `hostel-<12 characters>`, only users
with the marker "Hostel lease" (the account's full name), never a path from the
caller.

- **create.** A local user (NetUserAdd) with a random password that nobody keeps,
  the marker, membership of Users and of the local group `hostel-leases`. Its
  profile is made with `CreateProfile` (before its first logon, so Windows does not
  pick another directory). The server key from `C:\Program Files\Hostel\server_key.pub`
  goes into `.ssh\authorized_keys`, with access for SYSTEM and the user only. Windows
  does not use a security identifier (SID) again, so a file of an earlier lease
  user never belongs to a later one.
- **delete.** End all processes of the user's SID and log off its sessions; remove
  the scheduled tasks that run as the user; remove the files that the user owns in
  the places where all users can write (`C:\ProgramData`, `C:\Users\Public`,
  `C:\Windows\Temp`, and the top of the system drive), without following
  junctions or links; then the profile (`DeleteProfile`, also its registry hive)
  and the user (NetUserDel). A missing user is not an error.
- **list** and **check**: as on macOS and Linux.

## 5. Lease flow

- `lease_users = true` becomes valid for `os = "windows"`.
- The server runs the client over the machine user's shell (PowerShell) instead of
  `sudo`.
- `share-cache` on Windows: the machine user gives the group `hostel-leases` read
  access to `hostel\bin`, `tools`, `mise`, and `cache`, and nothing on `rooms` and
  the state files.
- The lease user fills its own home with a PowerShell script: copies of `bin`,
  `tools`, and `mise`, and junctions to the machine's cache entries.

## 6. The audit on Windows (leases on the machine user)

Snapshot at `begin` and repair at cleanup, as in design section 17:

| Location | Repair |
|---|---|
| The values (not the subkeys) of the HKCU keys `Run`, `RunOnce`, `Policies\Explorer\Run`, `Windows NT\CurrentVersion\Windows`, `Winlogon`, `Command Processor`, `Environment` | delete new values, restore changed and deleted ones |
| The user's Startup folder | delete new files, restore changed ones |
| PowerShell profiles (`Documents\WindowsPowerShell`, `Documents\PowerShell`), `.gitconfig`, `.ssh\rc` | restore the snapshot copy, or delete a new file |
| Scheduled tasks that run as the machine user (XML from `schtasks.exe /query /xml`) | delete new tasks, create changed and deleted ones again from their XML |

The task scheduler's CIM interface (`Get-ScheduledTask`) refuses the machine user
over SSH ("Access denied"), so the helper uses `schtasks.exe`. The machine lock
(`hostel\machine.lock`) uses `LockFileEx`.

## 7. Phases

| Phase | Content |
|---|---|
| 1 | This plan, decisions, design section 18 |
| 2 | The service, the pipe, the client, `machine join --lease-users` on Windows |
| 3 | The lease flow (server, `share-cache`, the lease user's home) |
| 4 | The audit on Windows |
| 5 | Real Windows machine: smoke tests, evidence, docs |

## 8. Tests

The E2E machines are Linux containers, so Windows is tested with smoke tests on a
real machine (as `TestSmokeWindows`):

- `TestSmokeWindowsLeaseUsers`: a lease runs as its own user, cannot read the
  machine user's rooms or write its cache; it leaves a process without
  `HOSTEL_LEASE`, a scheduled task, a `Run` value, and a file in `C:\ProgramData`.
  After the release, the user, its profile, its task, and its file are gone.
- The audit: a lease on the machine user adds a `Run` value, a file in the Startup
  folder, a scheduled task, and a PowerShell profile; after the release they are
  gone, and the event log names them.
- The whole E2E suite (Linux) in one local run, for the shared code.

## 9. Risks

- A tool that needs the desktop or the machine user's account fails in a lease user.
  Mitigation: `machine_user = true` (already on `windows-desktop`).
- A copy of `mise` and the tools for each lease user is slow. Mitigation: measure it
  on the real machine; use junctions for large read-only directories if necessary.
- Antivirus scans of new profiles can slow `create`. Mitigation: measure it.
