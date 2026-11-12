# M10h implementation plan: desktop app follow-ups

Status: draft (2026-11-12). The owner adds more feedback before the work starts.
Milestone M10h in `plan.md`. Owner decisions: `requirements.md`, section 3,
2026-11-12.

## 1. Why

Two inputs:

- **A report from a Mac app user (2026-11-12).** An agent made signed Mac Catalyst
  builds and did Mac app QA on `home/macbook` for a real app. It worked, with tunnels
  that have fixed machine ports, `hostel cp`, agent-device, and `hostel screenshot`.
  It also found six gaps:
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
- **Windows was never tested with a real app.** M6 and M7 tested a small WPF app
  only. On 2026-11-12 at 15:17, a `windows-desktop` lease on `home/win` ended with a
  failed cleanup ("processes of the lease still run: [31100]"; the room directory
  was in use), and the machine became `broken`.

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
  computers, Linux computers, and on-demand providers. More MacBooks will join.
- **Windows:** a test flight of a real Windows app through Hostel, before the fixes,
  so that its findings go into this milestone.

## 3. Order

| Phase | Work | Why in this place |
|---|---|---|
| 1 | Windows test flight (find problems, no code changes) | Its findings change phases 3 to 5 |
| 2 | Tunnel traffic as activity | Independent; it has an E2E test |
| 3 | Desktop cleanup on macOS and Windows | Fixes report point 2 and the 15:17 failure |
| 4 | Agent guides for both desktops | Uses the results of phases 1 and 3 |
| 5 | The machine setup guide | Uses phases 1 to 4 |
| 6 | Windows fixes from the test flight, then a second test flight | Must show a full run with no manual step |
| 7 | Docs, evidence, pull request | When the owner asks for the pull request |

Each phase ends with a local review, the tests, and a commit.

## 4. Phase 1: Windows test flight

Goal: an agent does the Windows QA flow of a real app only through Hostel. The
reference is the app's own runbook (`apps/windows/QA.md` in that repository). It
uses the shared `idem-win` host directly today. Each failure or manual step is a
finding.

### Before the flight

1. Find the cause of the 15:17 failure. Read the server log and the events for lease
   `5440afb6…`: who held it, which jobs and `desktop` commands it ran, and which
   program PID 31100 was. That process no longer runs.
2. Repair `home/win` (`hostel admin machine repair home/win`). Check that the
   machine is `running` and free.
3. Check that the desktop program runs the current helper. The "Hostel desktop"
   task starts `C:\Users\hostel\hostel\bin\hostel.exe`, which the server updates.
   Its start date must be after the last provisioning.
4. Hold the devlock `windows` lock for the whole flight. The owner's desktop
   session must show `Active` (`query session`).
5. Make a new worktree of the app repository. Run `devlock infra up` and start its
   API. Write a `hostel.toml` there, and do not commit it:
   - tunnels `api` (local port from `PORT`) and `model` (the scripted model, local
     port `PORT` + 1);
   - `[tool.windows-desktop]` env with the API address
     `http://localhost:{tunnels.api.port}`.

### Steps

Record the time, the result, and each manual action for each step.

| # | Step | Through Hostel |
|---|---|---|
| 1 | Acquire | `hostel acquire windows-desktop --task "Windows test flight"` |
| 2 | Sync | `hostel sync` of the worktree (size and time) |
| 3 | .NET SDK | `dotnet --version` in the lease: the SDK of `global.json` (`10.0.402`, roll forward). Does it come from the machine or from mise? |
| 4 | Build | `dotnet build` of the solution |
| 5 | Debug package | the MSIX build of the runbook, then `signtool sign` with the `CN=idem-dev` certificate |
| 6 | Install | `desktop Add-AppxPackage <msix>` as the `hostel` user |
| 7 | Start | `desktop --detach` with `shell:AppsFolder\<family>!App`; then `hostel screenshot` |
| 8 | Control | `winapp ui inspect` and one `invoke` on the app |
| 9 | API | the app reaches the local API through the tunnel (sign in as a new `@dev.local` user) |
| 10 | Scenarios | the FlaUI scenarios executable through `desktop`, with `IDEM_SCENARIO_APP` and `IDEM_SCENARIO_API_URL` |
| 11 | Logs | read the app log in `%LOCALAPPDATA%\Packages\<family>\LocalState` |
| 12 | Release AOT | one release AOT build: time, CPU, disk (the machine has 2 cores and 19 GB free) |
| 13 | Release | `hostel release`; cleanup must pass |
| 14 | After | no app process, the state of the installed package, the room deleted, the machine free |

### Expected risks (to confirm or reject)

- **Signing (step 5).** The private key of `CN=idem-dev` is in the computer's store.
  The `hostel` user is not an administrator, so it probably cannot use it.
- **Starting a packaged app (step 7).** This is the largest risk. The desktop
  program runs as `hostel` inside the desktop user's session. An MSIX package is
  registered for each user. I do not know if Windows activates a package of `hostel`
  in the session of another user.
- **Cleanup (steps 13 and 14).** A packaged app is not a child of `desktop` and has
  no `HOSTEL_LEASE` (design, section 9, "The Windows desktop", "Limits"). The installed package stays after
  the lease.
- **Capacity (step 12).** Disk and CPU can be too small.

### Rules for the flight

- No administrator change on `idem-win` without the owner's approval for that
  change.
- Close every window that the flight opens. Release the lease and the lock at the
  end, also when a step fails.
- No change to the app repository is committed.

### Output

A "Test flight findings" section at the end of this document: a table with step,
result, time, manual action, and the Hostel gap. Then phases 3 to 6 get their final
scope. A finding that does not block the QA flow goes to section 12 ("Not in this
milestone").

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
- A new backend method (beside `AllowForward` and `AllowDirect`) sets
  `last_activity_at` with the existing `store.Touch`. A lease that is not `ready`
  is ignored.
- The idle check in `internal/server/lifecycle.go` does not change.

### Docs

- `design.md`, section 16: tunnel and forward traffic sets `last_activity_at`;
  `ports` connections alone do not.
- `skills/hostel-use/SKILL.md` and `skills/hostel/SKILL.md`: a lease stays while
  commands run or data goes through its tunnels. The keep-alive with `hostel info`
  is no longer necessary for a test that uses a tunnel.

### E2E (`test/e2e/ports_test.go`)

With a short `idle_timeout` (for example 3 s) and wide margins:

1. A lease with a tunnel. A loop on the machine sends a request through the tunnel
   every 500 ms for 3 × `idle_timeout`. The lease stays `ready`. After the loop
   stops, the lease expires with the end reason "idle".
2. A lease with a tunnel and no traffic expires after `idle_timeout`.
3. The same as case 1 for a forward (guest → machine).

## 6. Phase 3: desktop cleanup

Principle: a desktop lease (`macos-desktop`, `windows-desktop`) holds the whole
machine (`whole_machine`). So each GUI process, agent, and app of the machine user
that started during the lease belongs to that lease.

### macOS (`tools/macos-desktop`)

- `start.macos.sh` also records:
  - all apps that `lsappinfo` lists, of every type (foreground, UI element, and
    background only), with PID and start time. Today it records only foreground
    apps.
  - the jobs of the user's launchd domain (`launchctl print gui/$(id -u)`), by
    label;
  - the entries of `~/Library/LaunchAgents`.
- `stop.macos.sh`:
  - runs `launchctl bootout gui/<uid>/<label>` for each new job whose label does not
    start with `com.apple.`, and deletes the new plists in `~/Library/LaunchAgents`;
  - ends each new app of any type, except Apple background processes that macOS
    starts by itself (`com.apple.*` that is not in the foreground);
  - keeps the steps of today: saved window state, the agent-device runner, and the
    new entries of `~/Applications`.
- Login items that an app registers with `SMAppService`: test on `home/macbook`
  whether `bootout` removes them. Reading the background item database
  (`sfltool dumpbtm`) needs an administrator. If cleanup cannot remove them, the
  guide says so.

Evidence (the E2E machines are Linux containers, so they cannot run these scripts):
a lease on `home/macbook` installs a test launch agent and starts a background-only
app. After release, neither runs and the plist is gone. A second lease does not see
them.

### Windows (`internal/helper`, `tools/windows-desktop`)

- **The desktop program ends what the lease started.** At the end of a
  `windows-desktop` lease, the desktop program ends each process of the machine
  user in its desktop session that started after the lease started, except itself.
  This also ends packaged apps, which are not children of `desktop`. The helper
  already lists the processes of the machine user (`proc_windows.go`); this adds
  the session ID and the creation time. A process can then not escape by starting
  through the shell.
- **New packages go.** `start.windows.ps1` records the packages of the `hostel` user
  (`Get-AppxPackage`; no administrator needed). A new `stop.windows.ps1` removes each
  package that the lease added (`Remove-AppxPackage`). Removal also deletes the
  package's app data, so no app session stays (report point 3, for Windows).
- **Cleanup waits for processes to end.** On Windows, a process that was told to end
  can still hold files for a short time. Step 1 of cleanup waits on the process
  handles (at most 10 s) before the check. Step 5 retries the room deletion for up
  to 10 s after a sharing violation. When a process still runs, the error gives its
  image name, not only its PID.
- The cause of the 15:17 failure (phase 1) can add a change here.

Evidence: phase 6, the second test flight.

## 7. Phase 4: agent guides

`tools/macos-desktop/GUIDE.md`:

- System dialogs (local network, Input Monitoring, password prompts): take
  `hostel screenshot`, find the button, and click its coordinates with
  agent-device. The exact command comes from `agent-device help`; test it on
  `home/macbook` with a real dialog before it goes in the guide.
- Chrome: start it with `--user-data-dir` in the room directory, so that cleanup
  deletes the profile and its logins.
- What stays after the lease: app settings, keychain items, and "Always allow"
  answers in the home directory. Do not sign in to real accounts.
- Signing: if the machine has the signing keychain (phase 5), unlock it with the
  command in the machine facts, then build with `-allowProvisioningUpdates`.
- Logs: `log show` needs an administrator. Start the app executable in a job with
  `OS_ACTIVITY_DT_MODE=YES` to get its `os_log` lines on stderr. Test this before it
  goes in the guide.

`tools/windows-desktop/GUIDE.md`:

- The flow for a packaged app, from the test flight: build, sign, install, start,
  logs, and what cleanup removes.
- Each workaround that phase 1 found and that phase 6 does not remove.

## 8. Phase 5: the machine setup guide

`docs/machine-setup.md` replaces `docs/static-machine-setup.md`. The old guide is
from M2 and gives the steps by hand. The new guide starts with `hostel machine join`.
It is for a person who adds a new machine, step by step, with a check after each
part. Links to the old file change in `README.md`, `docs/design.md`,
`docs/deploy-vps.md`, `skills/hostel-tools`, `skills/hostel-machines`, the tool
scripts, `cmd/hostel/machine.go`, `internal/helper/desktop.go`, and
`test/e2e/smoke_windesktop_test.go`.

Contents:

1. **Overview.** What Hostel does by itself (the user, the server key, the
   connection service, the helper, mise, the tools) and what only a person can do.
   A table: for each tool, the setup steps that it needs.
2. **All machines.** Power and sleep, a fixed address or `machine connect`, disk
   space (with numbers for Xcode, Android, .NET), and an admin account that is not
   the machine user.
3. **Mac.**
   - Join: `install.sh`, then `sudo hostel machine join <code>`.
   - Xcode and the iOS runtime.
   - Desktop session: automatic login of `hostel`, no lock, no sleep.
   - Screen Recording for `sshd-keygen-wrapper`, and Automation Mode.
   - **Mac app QA (new):**
     - apps that leases need and cannot install: an administrator installs them in
       `/Applications` (for example Chrome);
     - Accessibility, Screen Recording, and Input Monitoring for the test apps. An
       administrator grants them once for each app. macOS keeps the grant while the
       app's signature stays the same (the same certificate and bundle ID), so later
       builds keep it. A PPPC profile needs MDM, and no profile can grant Screen
       Recording;
     - optional: a sudoers rule for `/usr/bin/log` only, so that leases can read the
       system log.
   - **Signing (new, optional):**
     - register the Mac's provisioning UDID in the Apple team;
     - make a certificate only for Hostel, and import it without export rights
       (`security import -x`) into a separate keychain of the `hostel` user,
       `hostel-signing.keychain-db`, with Apple's WWDR intermediate certificate;
     - its password goes in a file that only `hostel` can read. Over SSH a keychain
       is locked, so each lease unlocks it before a build;
     - the App Store Connect key (Developer role) goes in a file that only `hostel`
       can read; the machine's `env` gives its path and IDs;
     - risk: every lease on this machine can sign with this certificate and use the
       key. Revoke them when the machine leaves.
     - Evidence: a build signed with a self-signed test certificate on
       `home/macbook`. A real Apple certificate needs the admin of that Mac.
4. **Windows.**
   - Join, OpenSSH Server, and the `LOCAL SERVICE` connect task.
   - The desktop task (`windows-desktop`), with its security note.
   - The tools of a lease: the .NET SDK (system install or the mise tool
     `dotnet:<version>`, from the test flight), Windows SDK (`signtool`), sideloading.
   - **Signing (new, optional):** the package certificate in the computer's store,
     trusted in `TrustedPeople`, and read access to its private key for `hostel`
     (or a copy in the `hostel` user's own store). The same risk as on the Mac.
   - Cloud PC compared with a physical computer: a Cloud PC shares its one desktop
     session with a person. A physical computer can sign `hostel` in automatically,
     so that the desktop is its own. Not tested: the guide says so.
5. **Linux.** Android: KVM, and the emulator tools.
6. **On-demand providers.** Links to `namespace.md` and to the template
   configuration; the provider login.
7. **Check.** `hostel tools` lists the machine and its tools. One short lease for
   each tool (commands given), with a screenshot for each screen tool.

## 9. Phase 6: Windows fixes and the second test flight

- Fix the findings of phase 1 that block the QA flow, in Hostel code, tool scripts,
  or the setup guide. The fix for each finding is decided after phase 1. If a
  finding needs an owner decision (for example, packaged apps cannot start for
  `hostel` in another user's session), stop and ask.
- Then run the test flight again. Done when the full flow (steps 1 to 14) runs with
  no manual step, other than the owner's connected desktop.

## 10. Tests and evidence

- E2E: the new tunnel activity cases (phase 2). The whole suite passes in one local
  run on macOS and in the 10-run CI gate (if CI can run; the GitHub billing lock
  stopped Actions on 2026-11-09).
- `make lint`.
- Real machines (not in the E2E suite):
  - `home/macbook`: the launch agent and background app cleanup (phase 3), the
    dialog click (phase 4), and the signing keychain with a test certificate
    (phase 5).
  - `home/win`: the two test flights (phases 1 and 6). Screenshots of the app in a
    lease.
- The smoke tests `TestSmokeWindowsDesktop` and `TestSmokeDesktop` pass again with
  the changed scripts.

## 11. Risks

- A packaged app cannot start for `hostel` in another user's session. Then Windows
  desktop QA of packaged apps needs a machine whose desktop user is `hostel`.
  An owner decision is needed.
- Desktop cleanup ends a process that the machine needs. Mitigation: only processes
  of the machine user, only those that started after the lease, and not Apple
  background processes on macOS.
- `idem-win` has 2 cores and 19 GB of free disk. A release AOT build can be too slow
  or too large.
- The shared desktop on `idem-win` has two locks: Hostel leases and the devlock
  `windows` lock of the app's own agents. They do not know each other.

## 12. Not in this milestone

- The secret store (a later milestone, decision of 2026-11-12). Note for its design:
  rooms on one machine share one OS user, so one lease can read the files of a lease
  in another room.
- Full isolation of app state in the home directory (report point 3). M10h removes
  the Chrome profile (guide) and Windows packages (phase 3). The rest waits for M11.
  M11's one OS user for each lease does not work for desktop leases as planned,
  because the desktop needs the one signed-in user.
- System dialog support in agent-device (an upstream project).
- One lock for the shared Windows desktop for Hostel and devlock. After the test
  flight, the owner decides if the app's agents move their Windows QA to Hostel.

## 13. Questions for the owner

1. May the second test flight's setup give `hostel` read access to the private key
   of `CN=idem-dev` on `idem-win`? It is a development certificate. Every Hostel
   lease on that machine could then sign packages as `CN=idem-dev`.
2. The signing keychain on `home/macbook` with the real certificate needs the
   administrator of that Mac. Who does it, and when?
3. After the test flights, do the app's agents move their Windows QA from
   `idem-win` and devlock to Hostel leases?
