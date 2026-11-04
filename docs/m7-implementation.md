# M7 implementation plan

Status: in progress (2026-10-30): macOS and Windows parts built and tested. Milestone M7 in `plan.md`: desktop testing, macOS
first, then Windows (owner decisions, 2026-10-29). This file gives the facts, and
later the decisions, the build order, and the risks. `design.md` stays the
contract.

## 1. Facts from the experiments (2026-10-29)

### Home MacBook (static room)

- `hostel` is the console user, and Automation Mode is on without a password
  prompt (`automationmodetool`).
- The screen was locked (`CGSSessionScreenIsLocked`): "Require password after
  screen saver begins or display is turned off" was 5 minutes, and the screen
  saver had its default timer. With a locked screen, `agent-device` opened
  TextEdit, but its accessibility snapshot had 2 nodes and then hung, and
  `screencapture` gave the lock screen. The screen saver is now off for
  `hostel`; the password setting needs the owner (`static-room-setup.md`,
  section 6, step 2).

### Namespace macOS devbox (`tahoe-slim`, size M)

- Created in 16 s. Console user `runner`, screen not locked, Automation Mode off
  but needs no password (XCTest turns it on). Node and Xcode are in the image.
- `agent-device` 0.21.25 (`npm install -g`): `capabilities --platform macos`
  lists 31 commands (open, snapshot, click, fill, type, press, get, screenshot,
  close, and more). The first snapshot built and started its XCTest runner
  (24 s); later commands took 0 to 3 s.
- TextEdit: `open`, an interactive snapshot (45 visible nodes, the text view
  `@e17`), `click @e17`, `type` (20 and 11 characters), and a snapshot that read
  the text back. `get text` once hit the runner's 30 s watchdog, and the next
  command then failed while the runner drained; a retry after a few seconds
  worked.
- An own app: a 30-line AppKit app, built with `swiftc` into a `.app` bundle and
  signed ad hoc (`codesign -s -`) in 4 s. `agent-device open <path to .app>`
  failed; `open ./HostelSpike.app` (Launch Services) and then `agent-device open
  so.hostel.spike` worked. `click "id=add"` twice, then `get text "id=count"`
  read `Count: 2`. The window screenshot showed the same.
- Screenshots: `agent-device screenshot` (the app window) and `screencapture -x`
  (the whole desktop, 2560 × 1600) both work over SSH without a prompt.
- `devbox ssh -T -- cat file` writes a banner line before the file on stdout;
  binary copies must not use it.
- The devbox was deleted at the end (`devbox list`: none).

### Windows

- `agent-device` has no Windows support.
- The QA tools of another project control the `win-pc` desktop with a bridge: a
  scheduled task in the desktop user's session runs PowerShell jobs, UI
  Automation (FlaUI), and screenshots. A Cloud PC stops drawing when no client is connected.

### Windows: the room user on another user's desktop (`win-pc`, 2026-10-30)

The owner approved a test on `win-pc` while they keep its desktop connected
(option A: the apps of a lease run as the room user `hostel`, not as the desktop
user).

- An SSH session (session 0) cannot show windows, take screenshots, or use UI
  Automation. A scheduled task of the desktop user (logon type Interactive) runs
  in the desktop session.
- That task starts a process as `hostel` with `Start-Process -Credential`
  (`CreateProcessWithLogonW`; the Secondary Logon service gives the new logon
  access to the desktop). It needs the password of `hostel`: a random password in
  `C:\ProgramData\hostel\desktop\cred`, readable only by SYSTEM, Administrators
  and the desktop user. `hostel` cannot read it (checked); its key login still
  works.
- `C:\ProgramData\hostel` is closed to normal users (the room token, M6), so the
  desktop user and `hostel` need read access to the `desktop` folder.
- The `hostel` process ran in the desktop session (session 2) and could:
  - take a screenshot of the whole desktop (2560 × 1440);
  - start an app that shows its window on the desktop;
  - read the app's UI Automation tree (control types, names, AutomationIds);
  - use UI Automation patterns (Invoke twice: `Count: 2`; ValuePattern);
  - send real input with `SendInput`: Alt, `SetForegroundWindow`, an absolute
    mouse click into the text field (it got the keyboard focus), keys (the text
    arrived), and a click on the button (`Count: 3`).
- Pitfalls:
  - The first window of a process uses the show mode of its parent's
    `Start-Process`: `-WindowStyle Hidden` hid the app, and `Minimized` gave a
    minimized window at −32000. UI Automation patterns still worked on it, but
    mouse and keys did not. Start apps with the normal mode.
  - The managed UI Automation client (`System.Windows.Automation`) has no Win32
    proxies: classic Win32 and WinForms controls show as `Pane` without IDs, and
    `RegisterClientSideProviderAssembly` fails in PowerShell. WPF (and other
    apps with their own UI Automation support) show correctly.
  - Input goes to the foreground window. Before keys, check that the foreground
    window belongs to the app (an app's QA guide: otherwise keys go to the owner's
    desktop).
- After the test, the task was removed and no test process remained.

## 2. Decisions (macOS)

- **Rooms.** A macOS desktop lease is `hostel acquire macos --require desktop`
  (exclusive, design section 5). The reference room is a Namespace macOS devbox
  (`tahoe-slim`); a static Mac with an unlocked desktop session also works.
- **Control.** `agent-device` (already installed on rooms, `mise.agent_device`)
  with `--platform macos`. No new Hostel command: the guest runs `hostel run --
  agent-device ...`, and `hostel screenshot` takes the whole desktop.
- **Desktop leases are marked.** The server tells the helper that a lease is
  exclusive (`exclusive` in the begin and cleanup requests); the scripts get
  `HOSTEL_EXCLUSIVE=1`. Only then do the `macos` scripts touch the desktop, so a
  headless `macos` lease next to another lease changes nothing there.
- **Desktop cleanup** (`platforms/macos`): `prepare` records the GUI apps that
  run and the contents of `~/Applications`. `reset` ends every GUI app that was
  not running at `prepare` (SIGKILL: a "quit" can wait at a save dialog),
  deletes their saved window state, the `agent-device` runner, and the new
  entries of `~/Applications`. Apps that the guest starts with `open` are
  children of launchd, not of the lease, so the process cleanup of section 17
  does not find them.
- **Starting an own app.** `open <path to .app>` first (Launch Services), then
  `agent-device open <bundle id> --platform macos`; the agent guide says this.

## 2b. Decisions (Windows, 2026-10-30)

- **Rooms.** `hostel acquire windows --require desktop` on a static Windows
  machine with a signed-in desktop user and the "Hostel desktop" task
  (`static-room-setup.md`, Windows, step 6). The test room is `win-pc`
  while the owner keeps its desktop connected (owner, 2026-10-30).
- **Account.** The apps of a lease run as the room user `hostel` on the desktop
  user's desktop (option A, owner, 2026-10-30), not as the desktop user.
- **The desktop program** (`hostel room desktop serve`) runs in the desktop
  session; `desktop` and `winapp` on the room send lease commands to it
  (`design.md`, "The Windows desktop").
- **Control.** Microsoft's `winapp ui` (winapp CLI v0.7.1, MIT; owner,
  2026-10-30), not own UI Automation commands. `agent-device` has no Windows
  support. WinAppDriver is not maintained.
- **Cleanup.** The program's job objects end the lease's desktop processes when
  the lease ends; no prepare or reset script.

## 3. Build order

1. `exclusive` in the begin and cleanup requests, `HOSTEL_EXCLUSIVE` for the
   scripts; an E2E check with the fake platform.
2. `prepare.macos.sh` and `reset.macos.sh` for the desktop, checked on a devbox.
3. The agent guide: a macOS desktop section.
4. The Namespace smoke test: a desktop lease builds and starts a small app,
   clicks it with `agent-device`, and takes a screenshot; a second lease on the
   same warm devbox finds no app of the first one.
5. Docs (`design.md`), review, gates.

## 4. Done when (macOS)

An agent on the owner's Mac acquires a macOS desktop lease on a Namespace devbox,
builds a small macOS app there, starts it, reads its accessibility snapshot,
clicks and types in it, and takes a screenshot; after the release, the app runs
no more and the desktop is clean (checked by a second lease on the same room).

## 5. Results (macOS, 2026-10-29)

- `TestSmokeNamespaceDesktop` passed (136 s; the devbox was deleted at the end).
  First lease (ready after 44 s): the app built with `swiftc` on the room,
  `open`, `agent-device open so.hostel.smoke`, a snapshot with `Count: 0`, two
  clicks on `id=add`, `get text` read `Count: 2`, `fill` and `get text` on the
  text field, and a desktop screenshot. The lease also left a copy in
  `~/Applications` and TextEdit with unsaved text. Second lease on the same warm
  devbox (ready after 19 s): no `HostelSmoke`, no TextEdit, no copy.
- Found on the way:
  - `osascript` to TextEdit waits (an Automation permission prompt); the test
    uses `agent-device`.
  - `agent-device open com.apple.TextEdit` found no app; `open TextEdit` (the
    name) works. For an own app, the bundle ID works after `open <path>`.
  - `agent-device type` checks the text afterwards; TextEdit's corrections make
    that check fail although the text is there. Typing is not safe to repeat.
- `TestExclusiveScripts` (E2E) passed: `HOSTEL_EXCLUSIVE=1` for the scripts of a
  desktop lease, empty for a normal one.

## 6. Results (Windows, 2026-10-30)

- `TestSmokeWindowsDesktop` passed on `win-pc` (72 s; the owner kept the
  desktop connected; the `windows` desktop lock of the other projects was held, and the room `home/win` of
  the exe.dev server was drained during the test). First lease (ready after
  20 s): `dotnet build` of a small WPF app (4 s), `desktop --detach
  out\HostelSmoke.exe`, `winapp ui status` and `inspect`, two `invoke`s
  (`Count: 2`), `set-value` and `get-value` on the text field, a real `click`
  into the field, `send-keys --via send-input` (the keys arrived), a real
  `click` on the button (`Count: 3`), and a desktop screenshot (2560 × 1440)
  through the `windows` platform script. Second lease: no `HostelSmoke` process.
- `TestSmokeWindows` (M6) passed again with the same helper.
- `TestDesktop` (E2E, Docker): output, exit codes, a missing command,
  `--detach`, a detached process that cleared `HOSTEL_LEASE` ends with the
  lease, and the program serves the next lease.
- Found on the way:
  - The platform hash had the file modes. Windows has none (Go reports 0666),
    so the first Windows platform directory failed with "hash mismatch". The
    hash of a Windows room now has no modes (the other rooms keep theirs, so
    they do not provision again).
  - `hostel screenshot` read the file with `cat`, which does not work on a
    Windows room. It now uses a binary copy in PowerShell.
  - `winapp ui send-keys " and keys"` lost the first space (not checked
    further; the test types `+keys`).
  - The first `winapp ui status` of a new install can take more than a minute.
  - After the test, the exe.dev server installs its older helper again, which
    has no `room desktop serve`; the task tries again every 10 seconds until
    that server runs a build with M7.
