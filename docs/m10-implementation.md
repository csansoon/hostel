# M10 implementation plan

Status: built (2026-11-03); the contracts are in `design.md`. Milestone M10 in `plan.md`: the status view.
Design: `multiplayer.md`, section 8. When a step is built, its contract moves
into `design.md`.

## 1. Facts from the code (2026-11-02)

- `GET /v1/status` returns the rooms (state, slots, ready platforms) and the
  leases that are not final (user, task, platform, room, slot, position).
  `hostel status` prints two tables (leases, rooms). Tests and agents read it.
- The server has no status revision. A lease change calls the store's lease
  hook (`s.notify`); room changes have no hook.
- Jobs live on the room: `hostel jobs` asks the room helper (`job list`) over
  the guest's SSH. The server can reach the helper of a room (`connectAs`).
- The probe reads the OS, architecture, home, and shell. It reads no hardware.
- Admin actions exist as API routes: release any lease, move a queued lease,
  change its lane (M8); drain, undrain, repair, destroy a room (M9).

## 2. Decisions

- **Revision and long poll.** The status response has `revision`: a hash of
  its content (without the times that only count up). `GET
  /v1/status?since=<revision>&wait=<duration>` (at most 50 s) answers at once
  when the revision differs, else when it changes, else at the end of the wait.
  The server checks again when the lease hook fires and every 2 s (rooms,
  config changes); no new store hooks.
- **More in the status.** The server ID and the caller (user, role); the
  machines from the configuration (kind, OS, platforms, labels, `max_rooms`,
  connect), so the view shows on-demand machines without rooms; for each
  queue: the waiting leases in order, the estimated wait, the next lease. The
  room's hardware (below).
- **Hardware.** A new helper command `hostel room hardware` prints the CPU
  model, cores, memory, free disk of the home, and the OS version (pure Go,
  `sysctl`/`/proc`/PowerShell with deadlines). The server runs it after each
  provisioning and keeps it on the room (`rooms.hardware`, JSON).
- **Room details.** `GET /v1/rooms/{name}`: the room, its machine, the lease of
  each slot with its jobs (the server asks the helper of the slot's user,
  deadline 10 s; on an error the details say "jobs unknown"), the queue of the
  room's platforms, and the last 10 events of the room. The server does not
  store jobs.
- **Plain output.** `hostel status` (aliases `ls`, `rooms`) prints the layout
  of `multiplayer.md` section 8 when stdout is not a terminal, or with
  `--plain`. `hostel status <room>` prints the details of a room and `hostel
  status <platform>` the queue of a platform, as plain text, with the lease IDs
  for the admin commands. (Not `hostel room <name>`: `hostel room` is the room
  helper, and on-demand room names have no `/`.) The agent guide keeps `hostel
  status`; agents read the plain layout.
- **Interactive view.** In a terminal, `hostel status` draws the same lines
  inline below the prompt (no alternate screen), with a selection (`↑`/`↓`,
  Enter for details, Esc back, `q` or Ctrl-C to quit; the last frame stays).
  It redraws on each long-poll answer. Own code on `golang.org/x/term` (raw
  mode, ANSI cursor moves): no TUI library. On Windows the console must
  support VT sequences (Windows Terminal and Windows 10+ conhost do).
- **Admin actions in the view.** Room details: `e` end the lease, `d`
  drain/undrain, `r` repair. Queue details: `e` end the lease, `Shift+↑/↓`
  move, `p` change the lane. Each asks "… ? [y/N]" in the view and uses the
  existing API (which writes the event with the admin). Members see the keys
  only for their own leases (`e`).

## 3. Build order (phases; each ends with review, tests, a commit)

1. **Server.** Revision and long poll; machines, queues, caller, and server ID
   in the status; hardware (helper command, probe step, migration 007); room
   details with jobs and events.
2. **Plain output.** `hostel status --plain` (and in a pipe), aliases,
   `hostel status <room|platform>`.
3. **Interactive view.** The inline renderer, keys, details, long poll.
4. **Admin actions** in the view, with confirmations.
5. Docs (`design.md`, agent guide, `README.md`), review, gates, done-when.

## 4. Tests

E2E (Docker rooms):

- `TestStatusPlain`: a pipe gives the plain layout: the header (server, rooms,
  waiting, you), a room in use with the user and the task, a free room, an
  on-demand machine without rooms, and a queue with its next lease.
- `TestStatusLongPoll`: `since` with the current revision waits; a new lease
  answers it before the wait ends; an old revision answers at once.
- `TestRoomDetails`: `GET /v1/rooms/<name>` (and `hostel status <room>`) shows the hardware, the lease with its
  running job (`hostel run --detach`), the queue, and the events.
- `TestStatusView`: in a pseudo-terminal (`github.com/creack/pty`, pure Go, test
  only): the view draws, `↓` and Enter open the details, `e` and `y` end the
  lease (the lease ends), a queue move with `Shift+↓`, `q` quits and the last
  frame stays.
- Existing tests that read `hostel status` move to the new layout.

Smoke (owner's done-when): on `hostel.example.com`, with a coworker's lease (a
second user) and a running job.

## 5. Risks

- **Terminal differences.** Key sequences differ (Shift+arrows); the view
  falls back to `K`/`J` for moves. A terminal without VT support gets the plain
  output (`--plain`).
- **The job list needs the room.** A room that is slow or offline must not
  block the details: a deadline, and "jobs unknown".
