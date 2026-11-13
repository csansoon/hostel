# M10b implementation plan

Status: built (2026-11-03); the contracts are in `design.md`. Milestone M10b in `plan.md`: the Hostel app.
Owner decisions: `requirements.md`, section 3, 2026-11-03. When a step is
built, its contract moves into `design.md`.

## 1. Facts from the code (2026-11-03, after M10)

- `hostel status` has the inline interactive view of M10 (`cmd/hostel/view.go`,
  `viewactions.go`, `vt_*.go`) and the plain output (`status.go`).
- The server has what the app needs to read: `GET /v1/status` (long poll,
  machines, rooms with hardware, leases, caller), `GET /v1/rooms/{name}` (leases
  with jobs, queue, events), `GET /v1/platforms` (description, devices, free
  slots, queue, estimated wait), `GET /v1/me`, `GET /v1/users`.
- And what it needs to change: `POST /v1/connect` (an invite code),
  `DELETE /v1/me` (disconnect), `POST /v1/invites`, `PATCH/DELETE
  /v1/users/{name}`, `POST /v1/machines/invites`, lease release, move,
  priority, room drain, undrain, repair.
- The lease has the repository name and the guest computer, not the branch.
  The room has the hardware from provisioning, not the live use.

## 2. Decisions

- **Entry.** `hostel` without arguments, when stdin and stdout are terminals,
  opens the app. Without a terminal (a pipe, an agent), it prints the help as
  now (exit code 2). `hostel status` prints only plain text: the M10 view and
  its files go away; `--plain` stays accepted (it is the only output).
- **Library.** Bubble Tea v1 (`github.com/charmbracelet/bubbletea` v1.3.10),
  Lip Gloss v1.1.0, Bubbles v0.21.1 (text input, lists): pure Go, Windows
  console support. Full screen (alternate screen); the terminal is as before
  after quit. The code is in `internal/app`; `cmd/hostel` only starts it.
- **Data.** One long poll of `GET /v1/status` (as in M10) feeds all pages;
  `GET /v1/platforms` is read with each status change; a details page reads
  `GET /v1/rooms/{name}` when it opens and again at most every 10 s. Each
  request has a deadline; errors show in a status line and the app asks again
  after 5 s; a refused token (401/403) goes to Check in.
- **Pages.** Themed label + muted plain description (owner decision):
  - *Check in* (not connected): an invite code field, then the name (default
    `git config user.name`), then the connection (the same code as `hostel
    connect`). *Leave* quits.
  - *Main menu*: Lobby (platforms and their queues · N waiting), Rooms
    (machines where agents run · N in use), Front desk (this server, people,
    invites, check out).
  - *Lobby*: one row for each platform: name, description, free machines
    ("1 of 2 free"), waiting count, the wait of the next lease. Enter: the
    platform with its devices, the machines that serve it (and their state),
    the typical lease, and the full queue (position, user, task, priority,
    waiting, estimate, reason). Admins: `shift+↑/↓` (or `K`/`J`) move, `p`
    priority; `e` ends a lease (members: their own).
  - *Rooms*: one row for each machine (a static machine, or an on-demand
    machine with its running rooms): name, platforms, state in plain words
    ("Free", "In use · Ana · "iOS snapshots" · 12m", "On demand · 1 of 2
    running", "Not connected", "Broken: <reason>", "No new leases"). Enter: the
    machine with its hardware and live use, each lease (user, task, computer,
    repository, branch, times, jobs). Admins: `e` end the lease, `d` no new
    leases / allow again, `r` repair.
  - *Front desk*: the server URL, ID, and version; you (user, role, computer).
    Guest list (users with role and computers; admins: `i` invite (shows the
    code once), `r` change the role, `x` remove). Add a room (admins: makes a
    machine code and shows the join commands). Check out (disconnect this
    computer, with a confirmation).
  Each change asks "… ? [y/N]" and uses the existing API.
- **Branch.** `hostel acquire` sends the current git branch (`git branch
  --show-current`, 5 s deadline, empty when it fails) with the lease; the server
  keeps it (`leases.branch`, migration 008) and shows it in the status and
  details.
- **Live use.** A new helper command `hostel room usage` prints the CPU use
  (over 1 s), the used and total memory, and the free disk (pure Go: `/proc` on
  Linux; `sysctl`/`vm_stat` on macOS; the kernel on Windows). `GET
  /v1/rooms/{name}` gets it in the same 10 s deadline as the jobs (best effort;
  empty on an error).
- **Agent output.** `hostel status` keeps its layout and adds two lines at the
  end with the next commands (`hostel status <room|platform>` for details,
  `hostel acquire <platform> --task "…"` to get a room). The agent guide does
  not change otherwise.

## 3. Build order (phases; each ends with review, tests, a commit)

1. **Decisions and cleanup.** The decisions and this plan; `hostel status`
   plain only (the M10 view and its test go away); the agent hints.
2. **App frame.** `internal/app` with Bubble Tea: entry in `hostel`, the data
   loop, Check in, the main menu, Front desk (server, you, check out).
3. **Lobby.** The platform list, the platform page with the queue and the
   actions.
4. **Rooms.** The machine list and page; the branch (migration 008) and the
   live use (helper command).
5. **Front desk for admins.** Guest list, invite, role, remove; add a machine.
6. **Look** (owner decision, 2026-11-03): the frame (header bar, footer with the
   description of the selected item and key chips, `?` for all keys), cards,
   colors with one meaning each, pills, gauges, chips; `tab` between the cards
   of a page; `1` `2` `3` jump to Lobby, Rooms, Front desk. Colors are ANSI 256
   (readable on dark and light terminals); `NO_COLOR` turns them off.
7. **Presence** (owner decision, 2026-11-03): offline static machines (see
   `design.md`), `TestRoomOffline`.
8. Docs (`design.md`, `README.md`), review, gates, deploy, done-when.

## 4. Tests

E2E (Docker rooms; the app in a pseudo-terminal with `github.com/creack/pty`,
the screen read with a pure-Go terminal emulator, test only):

- `TestAppCheckIn`: without a connection, `hostel` shows Check in; an invite
  code connects; Front desk › Check out disconnects.
- `TestAppLobby`: the platform list (free machines, waiting); the platform
  page with the queue; an admin moves a lease and changes its priority; a
  member cannot.
- `TestAppRooms`: the machine list with a lease (user, task); the machine page
  with the branch, the running job, and the live use; an admin ends the lease.
- `TestAppFrontDesk`: an admin invites a user (the code connects), changes the
  role, and removes the user; add a machine shows a code.
- `TestStatusPlain`: the plain output with the agent hints; `hostel` in a pipe
  prints the help.

Smoke (owner's done-when): on `hostel.example.com`, with a coworker's lease.

## 5. Risks

- **Terminal emulation in tests.** Bubble Tea draws only changed lines; the
  test must read the screen through an emulator, not the raw bytes.
- **Windows console.** Bubble Tea supports it; the owner's done-when is on macOS.
  A Windows check runs on the Windows VM (SSH, no GUI) at the end.
- **Live use on macOS.** `vm_stat` and `top` are slow or need parsing; a deadline
  and best effort keep the page fast.
