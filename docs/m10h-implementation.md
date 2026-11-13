# M10h implementation plan: desktop app follow-ups

Status: approved (2026-11-12), phase 6.
Milestone M10h in `plan.md`. Owner decisions: `requirements.md`, section 3,
2026-11-12.

## 1. Why

Two inputs:

- **A report from a Mac app user (2026-11-12).** An agent made signed Mac Catalyst
  builds and did Mac app QA on a static Mac machine for a real app. It worked, with
  tunnels that have fixed machine ports, `hostel cp`, agent-device, and
  `hostel screenshot`. It also found six gaps:
  1. Signing material went into each lease by hand (`.p12`, its password, the App
     Store Connect key).
  2. A launch agent that one lease registered still ran in the next lease, with the
     old binary.
  3. App state in the home directory (sessions, settings, a Chrome profile with
     logins) stays for the next lease.
  4. agent-device cannot answer macOS system dialogs.
  5. Some steps need an administrator: TCC permissions and installs in
     `/Applications`.
  6. A long test run that uses only a tunnel goes idle after 10 minutes. The agent
     ran `hostel info` every 4 minutes to keep the lease.
- **Windows was never tested on purpose with a real app.** M6 and M7 tested a small
  WPF app only. An app agent used seven `windows-desktop` leases on a static
  Windows machine. The last one ended with a failed cleanup ("processes of the
  lease still run: [31100]"; the room directory was in use), and the machine
  stayed `broken`.

## 2. Owner decisions (2026-11-12)

- **No secret store in M10h.** It is planned for a later milestone. For now, signing
  material is part of the machine setup: an administrator installs it on the
  machine one time. Every lease on that machine can use it. Use a certificate only
  for Hostel, so that it can be revoked alone.
- **Tunnel traffic counts as activity.** It is one more activity signal. The other
  signals stay.
- **System dialogs:** the `macos-desktop` guide gives the workaround (a screenshot,
  then a click at coordinates).
- **Admin steps are machine setup.** One machine setup guide covers Macs, Windows
  computers, Linux computers, and on-demand providers.
- **Windows:** a test flight of a real Windows app through Hostel, before the fixes,
  so that its findings go into this milestone.
- **The Windows desktop user is the machine user** (section 15).

## 3. Order

| Phase | Work | Why in this place |
|---|---|---|
| 1 | Windows test flight (find problems, no code changes) | Its findings change phases 3 to 5 |
| 2 | Tunnel traffic as activity | Independent; it has an E2E test |
| 3 | Desktop cleanup on macOS and Windows | Fixes report point 2 and the failed cleanup |
| 4 | Agent guides for both desktops | Uses the results of phases 1 and 3 |
| 5 | The machine setup guide | Uses phases 1 to 4 |
| 6 | Windows fixes from the test flight, then a second test flight | Must show a full run with no manual step |
| 7 | Docs, evidence, pull request | When the owner asks for the pull request |

Each phase ends with a local review, the tests, and a commit.

## 4. Phase 1: Windows test flight

Goal: an agent does the Windows QA flow of a real packaged (MSIX) app only through
Hostel. Each failure or manual step is a finding.

Before the flight: find the cause of the failed cleanup (server log, events, the
agent's transcript), repair the machine, and check that the desktop program runs
the current helper. The app's repository gets a `hostel.toml` (not committed) with
tunnels to its local API and an env for `windows-desktop` with the API address.

| # | Step | Through Hostel |
|---|---|---|
| 1 | Acquire | `hostel acquire windows-desktop --task "Windows test flight"` |
| 2 | Sync | `hostel sync` of the worktree (size and time) |
| 3 | .NET SDK | `dotnet --version` in the lease: from the machine or from mise? |
| 4 | Build | `dotnet build` of the solution |
| 5 | Debug package | the MSIX build, then `signtool sign` with the machine's certificate |
| 6 | Install | `Add-AppxPackage <msix>` as the machine user |
| 7 | Start | `desktop --detach` with `shell:AppsFolder\<family>!App`; then `hostel screenshot` |
| 8 | Control | `winapp ui inspect` and one `invoke` on the app |
| 9 | API | the app reaches the local API through the tunnel |
| 10 | UI tests | the app's UI test executable through `desktop` |
| 11 | Logs | read the app log in `%LOCALAPPDATA%\Packages\<family>\LocalState` |
| 12 | Release build | one release AOT build: time, CPU, disk |
| 13 | Release | `hostel release`; cleanup must pass |
| 14 | After | no app process, the state of the installed package, the room deleted, the machine free |

Expected risks: the machine user cannot use the signing key; a packaged app does
not start for the machine user in another user's desktop session; a packaged app
is not a child of `desktop` and has no `HOSTEL_LEASE`, so cleanup does not find
it; the installed package stays; disk and CPU are too small.

Rules: no administrator change on the machine without the owner's approval for
that change; close every window that the flight opens; release the lease at the
end, also when a step fails; no change to the app repository is committed.

## 5. Phase 2: tunnel traffic counts as activity

### Behavior

- A ready lease stays active while data goes through its tunnels (machine → guest)
  or its forwards (guest → machine), in either direction.
- An open connection with no data does not count. `cmd` connections and `touch` stay
  as they are. `max_lease` still ends each lease.
- Hostel's own checks make no data on these channels. A tunnel is confirmed when the
  remote listen succeeds (`listenTunnel` in `internal/guest/ports.go`), and the
  guest checks its local server directly, not through the gateway. So a lease with
  tunnels and no app traffic still goes idle.

### Implementation

- `internal/gateway/forward.go`: `relay` gets a callback that the copies call when
  they move bytes. `acceptForwarded` (tunnels) and `directTCPIP` (forwards) pass a
  callback for the lease.
- The gateway reports activity for a lease at most once per interval. The interval
  is the shorter of 30 s and `idle_timeout` / 4, so short timeouts in tests still
  work.
- The backend's `Activity` sets `last_activity_at` with the existing `store.Touch`.
  A lease that is not `ready` is ignored.
- The idle check in `internal/server/lifecycle.go` does not change.

### Docs

- `design.md`, section 16: tunnel and forward traffic sets `last_activity_at`;
  `ports` connections alone do not.
- `skills/hostel-use/SKILL.md` and `skills/hostel/SKILL.md`: a lease stays while
  commands run or data goes through its tunnels.

### E2E (`test/e2e/ports_test.go`)

With a short `idle_timeout` and wide margins: a lease with tunnel traffic stays
`ready` and expires as idle after the traffic stops; the same for a forward.

## 6. Phase 3: desktop cleanup

Principle: a desktop lease (`macos-desktop`, `windows-desktop`) holds the whole
machine (`whole_machine`). So each GUI process, agent, and app of the machine user
that started during the lease belongs to that lease.

### macOS (`tools/macos-desktop`)

- `start.macos.sh` records all apps that `lsappinfo` lists (every type, with PID and
  start time), the jobs of the user's launchd domain, and the entries of
  `~/Library/LaunchAgents` and `~/Applications`.
- `stop.macos.sh` boots out new jobs whose label does not start with `com.apple.`,
  deletes the new plists, ends new apps (not Apple background processes that macOS
  starts by itself), and keeps the earlier steps (saved window state, the
  agent-device runner, new entries of `~/Applications`).

Evidence (the E2E machines are Linux containers, so they cannot run these scripts):
a lease on a real Mac installs a test launch agent and starts a background-only
app. After release, neither runs and the plist is gone.

### Windows (`internal/helper`, `tools/windows-desktop`)

- `start.windows.ps1` records the packages of the machine user and the processes of
  the desktop session. `stop.windows.ps1` ends the new processes of the session
  (programs that the shell started for the lease have no `HOSTEL_LEASE`) and removes
  the new packages, with their app data.
- Cleanup waits up to 10 s for the processes to end, ends the rest again, names
  them in the error, and retries the room deletion for 10 s.

## 7. Phase 4: agent guides

`tools/macos-desktop/GUIDE.md`: system dialogs (screenshot, then a coordinate
click with agent-device), Chrome with a profile in the room, what stays after the
lease, logs with `OS_ACTIVITY_DT_MODE=YES`.

`tools/windows-desktop/GUIDE.md`: the flow for a packaged app (build, sign,
install, start, cleanup), exit codes of cmdlets, package identity conflicts.

## 8. Phase 5: the machine setup guide

`docs/machine-setup.md` replaces `docs/static-machine-setup.md`. It starts with
`hostel machine join` and covers all machines, Mac (desktop session, app QA,
signing keychain), Windows (sshd, join, build tools, the desktop user and task,
signing), Linux (KVM), on-demand providers, and a check after each part.

## 9. Phase 6: Windows fixes and the second test flight

- The machine user becomes the desktop user (section 15), on a Windows machine
  that only Hostel uses.
- Run the Windows smoke tests, then the test flight again. Done when the full flow
  (steps 1 to 14) runs with no manual step.

## 10. Tests and evidence

- E2E: the new tunnel activity cases (phase 2), the runtime tunnel case, the
  relative `desktop` path. The whole suite passes in one local run on macOS and in
  the 10-run CI gate.
- `make lint`.
- A full local run (2026-11-13) failed in `TestTrafficActivity`: the next lease in
  the same repository used the port process of the expired one, which then
  stopped and removed the socket. Fix: the ping answer has the lease ID, and a
  lease command stops the process of another lease first (design, section 14).
  `TestPortsNextLease` shows it (it failed on the old code).
- Real machines (not in the E2E suite): a Mac (launch agent and background app
  cleanup, the dialog click, the signing keychain with a test certificate) and a
  Windows machine (the smoke tests and the two test flights).

## 11. Risks

- Desktop cleanup ends a process that the machine needs. Mitigation: only processes
  of the machine user, only those that started after the lease, not Apple
  background processes on macOS, and not programs in the Windows folder.
- A Windows machine whose desktop is also used by a person cannot be a
  `windows-desktop` machine (section 15).

## 12. Not in this milestone

- The secret store (a later milestone, decision of 2026-11-12). Note for its design:
  rooms on one machine share one OS user, so one lease can read the files of a lease
  in another room.
- Full isolation of app state in the home directory (report point 3). M10h removes
  the Chrome profile (guide) and Windows packages (phase 3). The rest waits for M11.
  M11's one OS user for each lease does not work for desktop leases as planned,
  because the desktop needs the one signed-in user.
- System dialog support in agent-device (an upstream project).

## 13. Owner answers (2026-11-12)

1. The machine user may get read access to the private key of the machine's
   signing certificate. The agent may configure the machine as an administrator.
2. The administrator who sets up a machine also installs its signing material. The
   machine setup guide says so.
3. App agents move their Windows QA to Hostel after the test flights pass and their
   findings are fixed.

## 14. Test flight findings (phase 1, 2026-11-12)

### Windows flight steps

| # | Step | Result | Time | Manual action |
|---|---|---|---|---|
| 1 | Acquire | ready; the first call returned "not finished" during the sync | 2 min 46 s (2 calls) | none |
| 2 | Sync | 6,031 files | in step 1 | none |
| 3 | .NET SDK | from the machine (system install); `dotnet` was not a Hostel tool of the machine | – | none |
| 4 | Build (Debug MSIX) | passed | 5 min 10 s (3 min 35 s again) | none |
| 5 | Sign | failed: "No certificates were found"; passed after an admin gave the machine user read access to the key | – | admin `icacls` |
| 6 | Install | failed with `0x80073D19` (the desktop user had a package with the same identity and other content); passed with another package name | 5 s | package renamed in the worktree |
| 7 | Start | **failed**: `0x80070520` "A specified logon session does not exist" (activation API); `explorer.exe shell:AppsFolder\…` started nothing; the unpackaged build needs the Windows App Runtime package | – | – |
| 8–11 | Control, API, UI tests, logs | not possible without step 7 | – | – |
| 12 | Release build | skipped: it does not change the result | – | – |
| 13 | Release | cleanup passed; Notepad (started by a desktop command) ended; room deleted | < 1 min | none |
| 14 | After | the installed package stayed for the machine user | – | admin removed it |

### Findings and what M10h does

| ID | Finding | Action |
|---|---|---|
| W1 | **A packaged app cannot start for the machine user on a desktop of another user** (`0x80070520`). | The machine user is the desktop user (section 15) |
| W2 | `explorer.exe` from a lease makes `explorer.exe` processes of the desktop user (COM launch). The target program did not start. No escalation was found, but the desktop user's shell takes requests from leases. | Goes away with section 15; stop ends the programs that the shell started for the lease |
| W3 | Signing needs read access to the certificate key. | Machine guide 4.5 |
| W4 | A package with the same identity for another user blocks the install. | Guide |
| W5 | Packages stay after the lease (app data too). | `stop.windows.ps1` removes them |
| W6 | A cmdlet error does not fail a job (exit code 0). | Guide |
| W7 | The guide's `acquire dotnet windows-desktop` fails: `unknown tool "dotnet"`. | Guide fixed |
| W8 | A failed cleanup: a process of the lease still ran and held the room directory. Not reproduced. | Cleanup waits up to 10 s, kills again, names the process, and retries the deletion |
| W9 | A runtime tunnel (`hostel tunnel add`) came back in the next lease, also on another machine. | Fixed, with an E2E test |
| W10 | `desktop --detach <relative path>` failed (exit 127). | The path is now relative to the command's directory; checked by the Windows smoke test (`desktop --detach out\HostelSmoke.exe`) |
| W11 | Event times have no date (an event of two days earlier looked like today). | `hostel status` shows the date for an event of another day |
| W12 | Small free disk on the machine (8.5 GB). | Machine guide: disk space |
| W13 | A sync that lost its connection made a lease expire. | Open: not reproduced |
| W14 | After a helper update, the desktop program restarts within 10 s; a lease that started in that time failed in the `windows-desktop` start script, and the machine became `broken`. | The start script waits up to 30 s for the program |
| W15 | Windows PowerShell did not start in the desktop task at the first sign-in of the machine user (0.08 s CPU in 10 minutes). | The task runs a `cmd` loop |
| W16 | At the first sign-in, the Start menu was open and had the focus: real clicks failed ("Target window is not in the foreground"). The console screen was 1024 × 768. | Machine guide 4.4: a second restart, and the screen size |
| W17 | With the machine user as the desktop user, `Add-AppxPackage` in a job fails with `0x80070005` ("Failed to initialize PLM"); through `desktop` it works. | Guide |
| W18 | The stop script failed at the release: it tried to remove a framework package (the Windows App Runtime) that the lease's package had brought. Windows refuses that. The machine became `broken`. | Stop removes only packages that are not frameworks |
| M1 | macOS: a launch agent of an earlier lease stayed registered (through `SMAppService`). | Desktop cleanup ends new agents (tested) |
| M2 | macOS: apps that a lease without `macos-desktop` opens stay. | Guide: open apps only in a desktop lease |
| M3 | macOS: `osascript` that controls another app opens a permission dialog for `sshd-keygen-wrapper`. | Guide |
| M4 | macOS: a new launch agent shows the notification "App Background Activity". | None (it goes away) |

## 15. The Windows desktop user (owner decision, 2026-11-12)

Windows starts a packaged app only for the user of the desktop session. With a
person's account as the desktop user and leases as the machine user, packaged apps
cannot start. The machine user must be the desktop user.

- Machine guide 4.4: Windows signs in the machine user at start (automatic sign-in,
  password in an LSA secret), and its "Hostel desktop" task starts the desktop
  program directly in that session. The `cred` file and `Start-Process -Credential`
  go away.
- Windows 11 has one desktop session. A Remote Desktop connection of another user
  disconnects the machine user's session. So a `windows-desktop` machine is used
  only by Hostel; administrators work over SSH.
- `tools/windows-desktop`: `start` records the processes of the desktop session,
  and `stop` ends the new ones that are not in the Windows folder, before it
  removes the lease's packages.
- The smoke test `TestSmokeWindowsDesktop` also starts the app through
  `explorer.exe` and checks that cleanup ends it.
