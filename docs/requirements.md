# Hostel requirements

Status: ready for implementation. No code exists yet.

## 1. Purpose

Coding agents edit code and run cheap checks on their local machine. Builds, UI QA,
and tests on simulators, emulators, and desktops run on remote machines. Hostel
gives an agent short, temporary, exclusive access to a remote machine that has the
tools it asks for, and removes all traces of that access after the agent finishes.

`context.md` explains why the project exists and what was tried before.

## 2. Vocabulary

Only three words come from the hostel theme: **Hostel**, **Guest**, and **Room**.
All other terms are plain technical words.

| Term | Meaning |
|---|---|
| **Hostel** | The project, the `hostel` binary, and the central server. |
| **Guest** | A client: an agent (or a person) that uses the `hostel` CLI. |
| **Room** | One concrete remote machine at runtime (a static host, or one created cloud machine). |
| **Slot** | A place on a room for one lease, with its own directory, screens, and ports. |
| **Lease** | Exclusive use of one slot, from the request to the end of cleanup. |
| **Queue** | The leases that wait for a slot (one waiting line for all leases). |
| **Provider** | A source of machines, with its own settings (kind, login, account limit). Examples: a set of static hosts, a Namespace account, an external command. |
| **Machine** | An entry in a provider. In a static provider, a real host (one room). In an on-demand provider, a template that gives rooms when needed. |
| **Tool** | Something that a machine gives to a lease: software (`android-sdk`, `xcode`, `jdk`) or a screen (`android-emulator`, `ios-simulator`, `macos-desktop`, `windows-desktop`). A guest asks for the tools that it needs and can use all software of the machine; a lease starts only the screen tools that it asked for (M10e, replaces platform, device, and labels). The OS is a property of the machine, not a tool. |
| **Option** | A version of a software tool, or the phone or tablet of an emulator or simulator, after a colon: `node:22`, `android-emulator:pixel-9`. A filter: without it, any option matches. |
| **Screen tool** | A tool that the agent looks at and acts on. Hostel starts it for each lease, takes screenshots of it, and cleans it up. |
| **Tool scripts** | The scripts of a tool (one file for each step and OS) that install, check, start, screenshot, and stop it. |
| **Inventory** | The tools of a room with their real options, as the server installed and checked them. |
| **Cleanup** | The steps after a lease that delete everything the lease left. |
| **Helper** | The `hostel room` mode of the binary. It runs on rooms. |
| **Gateway** | The SSH server inside the Hostel server. All guest SSH traffic goes to it, and it connects to rooms. |

## 3. Owner decisions

| Date | Decision |
|---|---|
| 2026-10-26 | The 10-run E2E stability gate runs in CI (10 parallel jobs, the workflow "E2E gate"), not on the owner's shared Mac, whose load from other projects makes the result depend on the machine. Before a milestone ends, the suite also passes once locally on macOS, so the macOS host is covered. |
| 2026-10-27 | Android emulators do not run on Namespace macOS devboxes, also if they can. Android rooms on Namespace are Linux machines. The test of the emulator on a macOS host uses the owner's physical Mac `mac-mini` (the static room of M2). |
| 2026-10-27 | The VPS for the server is an exe.dev VM. It uses the exe.dev hostname (`<vm>.exe.xyz`) and its TLS; no own domain. |
| 2026-10-27 | The M5 done-when uses a guest on the owner's Mac (a local agent), not a cloud agent: guest on the Mac → server on the VPS → home MacBook through `room connect`. A cloud agent is an optional extra check. |
| 2026-10-29 | Desktop testing for macOS and Windows comes before the hardening milestone: agents must build, install, start, and remove desktop apps, take screenshots, and click and type, as they do on iOS and Android. Running a repository's own UI test suites and signed builds are not in scope. macOS comes first (the home MacBook); the Windows machine is decided after that (on `win-pc`, the only desktop session is the owner's, and a Cloud PC stops drawing without a connected client). This replaces the decision of 2026-10-23 that macOS desktop apps are future work. |
| 2026-10-29 | The Windows room of M6 is the owner's cloud Windows PC (`win-pc`) as a static machine, not a Namespace instance (no plan change). Hostel uses its own local user `hostel` without administrator rights, one slot, command-line work only (the desktop session stays for the owner's other agents), and `room connect`. |
| 2026-10-30 | Windows desktop testing (M7) is built and tested on `win-pc` while the owner keeps its desktop connected; Hostel and the agents of other projects share that desktop for this test (the `windows` desktop lock of those projects). The apps of a lease run as the room user `hostel` on the desktop user's desktop, started by a scheduled task of the desktop user with the password of `hostel` (option A), not as the desktop user. Hostel uses Microsoft's `winapp ui` (winapp CLI) to control Windows apps, not its own UI Automation commands. |
| 2026-10-30 | Several people use one Hostel server (milestones M8 to M10; design in `multiplayer.md`). Each owner runs their own server; there is no central Hostel. Users have a unique name and a role, `admin` or `member`. Members see everything and use their own leases; only admins change the configuration, invite or remove users, and release, move, or reprioritize another user's lease. A user whose lease an admin ended or moved sees the reason. No limit of leases per user for now. |
| 2026-10-30 | A client connects with `hostel connect <invite code>`. The code contains the server's addresses, the fingerprint of its key, and a one-time secret, so it works without a domain (IP, LAN, Tailscale). A client has one connection at a time. The server serves HTTPS with its own certificate (pinned by the client), or no TLS behind a proxy with TLS. |
| 2026-10-30 | After the first setup on the server machine (`hostel server init`), admins configure everything from a client: platforms, rooms, providers, users, settings. Server configuration commands are under `hostel config`; everyday commands stay few (`connect`, `status`, `room`, `platforms`, and the agent commands). Commands on room machines move to `hostel machine` (`join`, `connect`). A new machine joins with one command on it (`hostel machine join <code>`) and connects out to the server. |
| 2026-10-30 | The server defines its platforms (settings and scripts). `ios`, `android`, `macos`, and `windows` are defaults that come with Hostel; an admin can change or remove them and create own platforms from a client. A lease for an unknown platform fails at once; `hostel platforms` lists the platforms with descriptions. |
| 2026-10-30 | Queues: one queue for each platform with the lanes `high`, `normal`, `low` (set only when the user asks for it), first in, first out inside a lane. Aging is removed. Admins can move a queued lease. The status view (`hostel status`) is interactive and inline in a terminal, not full screen. Hardening against backdoors moves to M11. |
| 2026-11-02 | On macOS, the service that a machine join installs (`hostel machine connect`) runs as root: it starts at boot without a login, and the room user cannot read its token. The M9 done-when uses the owner's real machines: the Windows VM, the home MacBook, and Namespace (Linux and macOS). Members read the whole configuration; the values of `env`, `command_env`, and `github_token` are shown as `***` (only admins see them). |
| 2026-11-03 | Two kinds of commands. People use the Hostel app: `hostel` without arguments in a terminal opens a full-screen interactive app (Bubble Tea) for everything: check in (connect), the platforms and their queues, the machines, the server, users, invites, check out (disconnect). Agents and scripts use only the `hostel <verb>` commands: no interaction, full plain text with the IDs and the next commands. The inline status view of M10 is replaced by the app (milestone M10b). This replaces the decision of 2026-10-30 that the status view is inline, not full screen. |
| 2026-11-03 | The app has a menu, not one dense screen: Lobby (the platforms, each with its queue, free machines, and wait), Rooms (the machines, each with its platforms and who uses it now), Front desk (the server: version, users, invites, add a machine, check out). Enter on an item opens its details (the full queue; the machine with its hardware, live CPU and memory use, and the current lease with its user, computer, repository, branch, and jobs) with the admin actions. |
| 2026-11-03 | Themed words are allowed as labels in the app only (Lobby, Rooms, Front desk, Check in, Check out, Guest, Guest list), each with a short muted plain description. Commands, CLI text, code, and docs keep the plain terms. |
| 2026-11-03 | The look of the app (M10b phase 7): the same frame on each page (a header bar with the Hostel sign, the place, and the user; a footer with the description of the selected item and the keys as chips; `?` lists all keys), cards with rounded borders, colors with one meaning each (amber: Hostel and the selection; green: vacant; blue: occupied; yellow: waiting; red: broken; a readable gray), status pills, gauges, and chips. Few elements: a selectable item has no description of its own (the footer shows it), and fields have no extra explanations. More themed labels: Vacant, Occupied, Guest, Checked in, Out by, Waiting line, Typical stay, Next room. |
| 2026-11-03 | Machine presence (in M10b): a configured machine can be off (for example the owner's Mac). The server checks the idle static machines and the offline ones regularly; an offline machine stays in Rooms ("Offline", last seen), the Lobby leaves it out, and the scheduler gives it no lease (leases wait); it comes back by itself when it answers. |
| 2026-11-03 | Public release (milestone M10c): the repository becomes public under the MIT license, with a README, a one-command install from GitHub Releases, and skills (`skills/*/SKILL.md`) that teach any agent to install, set up, extend, and use Hostel. The personal and internal details (the owner's server address, machine names, details of the owner's app) are removed from the current files and from the git history (a rewrite and force-push, confirmed again before it runs). The first release is `v0.1.0`; Claude makes the repository public and pushes the tag after the owner's OK. |
| 2026-11-03 | Hostel is its own tool, with its own goal: parallel QA and builds for coding agents on remote machines, for any app and for several apps at the same time. The docs do not name the app that it was first built for; examples use generic apps. |
| 2026-11-04 | Updates (M10d, `m10d-implementation.md`): `hostel update` replaces the CLI with the latest release; releases are signed (Ed25519, the key a repository secret with a backup by the owner) and the binary checks the signature. The CLI prints a notice when the server is newer. The server refuses guests older than its minimum version (HTTP 426). `hostel server update` on the server machine, and a remote update by an admin (`--remote`, and Update the server in the app), which restarts the server with the new binary. Rooms keep getting the helper from the server. A machine that joined shows "Out of date" when its connect service is older than the server; an administrator runs `hostel machine update` on it (the service does not replace itself). The app checks for a new release in the background and shows an Update entry. |
| 2026-11-05 | Tools (M10e, `m10e-implementation.md`) replace platforms, devices, and labels. A machine lists the tools that it gives (software and screens; the OS is a property of the machine, not a tool); the server installs and checks them and knows their real versions. A guest asks for tools, with an optional option as a filter (`node` any, `node:22`, `android-emulator:pixel-9`), and gets the first free machine that has all of them; all software of the machine is usable, and the lease starts only the screen tools that it asked for (an emulator only when asked for). A request that no machine can serve fails at once. When the lease is ready, the guest gets the facts of its machine (OS, hardware, all tools). `hostel.toml` has the top-level settings and `[tool.<name>]`; there is no compatibility with the old configuration. Desktops and emulators are tools too ("screen tools"). The server tells agents what each tool gives (`hostel tools`, a guide for each tool). One waiting line for all leases. Plain words: tool, option, machine, lease, slot; "platform" and "device" leave the vocabulary; Lobby and Rooms stay as app labels. |
| 2026-11-05 | One lease per worktree (M10e): lease commands always use the lease of the worktree, with no selector; a second machine at the same time uses another worktree. This is local to the CLI; the server knows leases by ID. |
| 2026-11-04 | Waiting in the queue while an agent works: `acquire` says how long the server keeps a waiting lease without it (`poll_timeout`), and `acquire --background` returns at once while a small background process on the agent's computer keeps the place until the lease is ready or ends. |
| 2026-11-05 | New look of the Hostel app (the levels stay: main menu, lists, detail pages): list items with a title line and a dim second line, sections with headings and empty space instead of boxes, glyph, color, and word for each state. Motion only inside a page (turning glyphs, a short glow on a change); pages do not slide. The main menu shows a small street as an easter egg (no legend, no explanation): the hostel between two buildings, a street lamp, a tree, and, rarely, a car that passes (a random time of up to 6 minutes before each car). The hostel has a fixed number of windows with a yellow light; the share of lit windows follows the share of occupied machines, so it works for one machine and for fifty. `hostel --demo` runs the app with made-up data and no server. Later the same day: the main menu is Front desk, Lobby, Rooms, with emoji icons on selectable items; Front desk has Guests (invite first), Rooms (admins: add a room, admin actions; management only there), This server, and Check out; every choice and question before a change is a popup (arrows and Enter, Esc cancels). |
| 2026-10-23 | Hostel focuses on iOS (simulators) for now. macOS desktop apps (UI control with Automation Mode, Mac Catalyst builds with team signing) are future work. The `macos` platform keeps its desktop screenshot. |
| 2026-10-23 | Each room gets `agent-device` (Callstack) by default, so that agents can tap, type, and read the screen of simulators, emulators, and desktops on the room. Hostel pins its version (the same as T3 Code). The server option `mise.agent_device = false` turns it off. |
| 2026-10-22 | The spare MacBook has FileVault off and logs in the `hostel` user automatically, so that it has a desktop session (label `desktop`). It holds no personal data. |
| 2026-10-22 | In M2, the server runs on the owner's Mac and reaches the MacBook over the local network. |
| 2026-10-21 | The server sends the mise binary to rooms compressed with gzip; the helper decompresses it and checks the hash. |
| 2026-10-21 | CI (GitHub Actions) runs lint, the unit tests, the cross-builds, and one E2E run for each pull request. |
| 2026-10-21 | Three pieces: a central server, thin guests, and rooms. The server holds the orchestration logic. |
| 2026-10-21 | The server must run on a Mac and on a small Linux VPS. It starts on the owner's Mac; later it moves to a VPS. |
| 2026-10-21 | Code transfer: the guest sends its working-tree files that git does not ignore, plus opt-in extra files such as `.env`. Rooms need no git access to the repository. |
| 2026-10-21 | Version 1 uses one slot per room. The data model must support several slots per room. |
| 2026-10-21 | Static rooms are in scope: an always-on VPS, and a spare MacBook for the first tests. |
| 2026-10-21 | Language: Go. |
| 2026-10-21 | Room access is always temporary and only through Hostel. Hostel can end it at any time, also on a static VPS. A guest cannot reach a room again without a new lease. |
| 2026-10-21 | Hostel uses mise on rooms to install package managers, compilers, and runtimes, at machine level (server configuration) and at repository level (the repository's mise files). |
| 2026-10-21 | Vocabulary: only Hostel, Guest, and Room come from the theme. CLI verbs are plain (`acquire`, `run`, `release`). |
| 2026-10-21 | Configuration model: providers contain machines (static hosts or on-demand templates); platforms are OS platforms (automatic) or device platforms (owner-defined, with scripts for each OS). The guest sees only platforms. Tools such as Node are not platforms. |
| 2026-10-21 | Configuration format: TOML, with defaults, strict validation, and one directory for each device platform. |
| 2026-10-21 | A guest can select devices of a device platform. The Mac desktop (Mac Catalyst) uses the `macos` platform with a `desktop` label, and takes the whole machine. |
| 2026-10-20 | Priority has a fixed default. The agent selects another priority only when the user asks for it. |
| 2026-10-20 | The API under test always runs on the guest's machine and is reached through a tunnel. |
| 2026-10-20 | Durable changes are made only locally. Changes on a room are temporary and are always deleted. |
| 2026-10-20 | On-demand machines (Namespace) are preferred to machines that are always on. |

## 4. Functional requirements

### Discovery and requests

- **R1.** A guest can list the tools that the machines give, with their options,
  what each tool sets in the environment, how to use it, the number of free slots,
  the waiting leases, and an estimated wait.
- **R2.** A request contains: the tools (each with an optional option), task
  description, and priority. A request without tools takes any machine.
- **R3.** The guest does not select a provider, machine, room, slot, or OS. It names
  the tools that it needs; only a desktop tool belongs to one OS.
- **R4.** If no configured machine can satisfy a request (an unknown tool or option,
  or no machine with all requested tools), the request fails at once with a clear
  message that names the reason.
- **R4a.** One tool can be available on machines with different OSes (for example
  the Android SDK on Linux and on macOS). A defined tool has scripts for each OS.
- **R4b.** When a lease is ready, the guest gets the facts of its machine: the OS
  with version and architecture, the hardware, and all tools of the machine with
  their options and environment (also the ones that it did not ask for).

### Queue

- **R5.** Order: higher effective priority first, then first in, first out.
- **R6.** Effective priority increases with wait time (aging), so that a
  low-priority lease does not wait forever.
- **R7.** A waiting guest sees its position and the reason it waits (no free slot,
  provider limit, room in creation).
- **R8.** Each guest command returns before a typical agent tool timeout (2
  minutes). A special exit code means "not finished", and the last output line names
  the command that continues the work (for `acquire`, the same command).
- **R9.** A queued lease that nobody polls for a configured time is cancelled.

### Rooms

- **R10.** The server gets rooms from providers. A new provider kind is a new
  adapter on the server only. Guests and rooms do not change.
- **R11.** First provider kinds: `static` (SSH hosts), `namespace` (devbox and
  instance machines), and `external` (any executable that implements a JSON
  contract). One provider can offer several machines.
- **R12.** Before a room accepts a lease, the tool scripts check that each tool of
  the machine works, and record its real option (the inventory).
- **R13.** A room can stay warm after a lease (first running, then stopped), as its
  machine declares.
- **R13a.** Limits exist for each on-demand machine and for each provider (account
  limit).

### Code and environment

- **R14.** The guest sends the files that `git ls-files --cached --others
  --exclude-standard` lists, plus extra files that the repository configuration
  lists. The guest prints the names of the extra files that it sends.
- **R15.** The guest can send the files again during a lease. The second send
  transfers only changed files and deletes files that the guest deleted.
- **R16.** The room makes a local baseline commit after each send, so that an agent
  can see its temporary changes with a diff.
- **R17.** The repository declares its usual tools, its project tools (mise), its
  setup commands, and settings for each tool that it uses. The server does not
  hardcode project versions.
- **R17a.** Hostel installs and uses mise on each room. Rooms need no tool
  manager before Hostel uses them. Tool versions come from the machine configuration and from
  the repository's mise files; installed tools stay in a cache between leases.
- **R18.** The guest's local services (for example the API on `PORT`) are available
  inside the room through a reverse tunnel. Commands on the room get environment
  variables with the tunnel addresses. The guest can add, change, and remove tunnels
  during a lease. A change of the guest's local port needs no restart. The room port
  is stable unless the guest changes it, and a tunnel can request a fixed room port.
- **R18a.** The guest can reach a TCP server on the room (for example a bundler or a
  debugger) through a local forward.

### Use

- **R19.** A guest can run commands in its slot, with streamed output. A command
  continues when the connection drops, and the guest can attach to it again.
- **R20.** A guest can get screenshots of the lease's screen tools, copy files to and from
  the slot, and open an interactive SSH session.
- **R21.** A person can see all leases, rooms, and the queue (`hostel status`).

### End of lease

- **R22.** A lease ends when the guest releases it, after 10 minutes without guest
  activity (configurable), or at the maximum lease duration.
- **R23.** Cleanup stops all processes of the lease, deletes the slot's files
  (including extra files such as `.env`), stops the lease's screen tools, and removes the
  guest's access.
- **R24.** If cleanup fails, the room is not used again until it is repaired or
  destroyed.

### Access control

- **R25.** A guest reaches a room only through Hostel. The guest never receives a
  credential that a room accepts.
- **R26.** Hostel can end a guest's access at any time (release, expiry, admin
  decision). Open sessions and tunnels close at once. Access returns only with a new
  lease.

## 5. Non-functional requirements

- **N1. No configuration for agents.** One command starts a lease. The server
  address and token come from one user config file or from environment variables
  that the owner sets once for each machine.
- **N2. Cost safety.** No on-demand room uses compute without a provider-side
  bound: a deadline, or an idle auto-stop for machines that can stop.
  The server finds and removes rooms that it created but no longer tracks.
- **N3. Resilience.** A server restart does not lose leases or the queue. A
  dropped connection does not stop a running command.
- **N4. Secrets.** Hostel never logs tokens, keys, file contents, environment
  values, or the output of guest jobs. The tail of tool script output (scripts that
  the owner controls) can go to the event log. Extra files such as `.env` exist on a room only during a lease.
- **N5. Portability.** The server runs on macOS and Linux. The guest CLI runs on
  macOS, Linux, and Windows. Rooms can run macOS, Linux, or Windows.
- **N6. Testability.** Automated E2E tests run the real server, guest, and helper
  binaries against rooms in Docker. Real-provider tests are opt-in smoke tests.
- **N7. Bounded I/O.** Each provider call, SSH operation, and external command has a
  deadline. No retry loop is unbounded.

## 6. Assumptions

- Guests are the owner's own agents. Hostel protects against accidents (state that
  goes from one lease to the next, forgotten cloud machines, secrets in logs). It does
  not protect a room against a hostile guest.
- One owner. All leases use the owner's provider accounts.

## 7. Non-goals (version 1)

- Durable changes on rooms, or a push from a room to GitHub.
- A replacement for CI.
- Several owners or teams with separate quotas.
- A web UI.
- More than one slot per room in production use. The model supports it; the
  scripts and tests come in milestone M6.
- Protection against a guest that installs a backdoor during a lease (for example a
  process that connects out, or a cron job). Planned for M11; see design section 18,
  "Backdoors".

## 8. Known constraints

`namespace.md` has the details and the sources.

| Fact | Effect on Hostel |
|---|---|
| Namespace Developer plan: 1 devbox per user; stopped devboxes count. | One devbox room at a time. A stopped warm room uses the limit. |
| macOS compute limit: 12 vCPU (two M machines), for devboxes and instances together. | The `namespace` provider's `max_rooms` covers its devbox and instance machines together. |
| No Windows devboxes. Windows instances need the Team plan or higher. | Windows needs a plan change and a Namespace instance machine, or a static Windows host. |
| Devbox SSH goes through the `devbox` CLI as a proxy. | Guests cannot reach a Namespace room without a Namespace login. The server relays. |
| macOS devbox: create 22 s, tuist 86 s, cold build 53 s, simulator boot 9 to 27 s. | A warm room saves about 2 minutes for each lease. |
| Linux instance: create 6 s, Android emulator 99 s, cold Gradle build 3 min 25 s. | Android needs warm rooms or prepared images. |
| `nsc ssh` drops long commands that print nothing. | Commands run detached on the room, with log streaming. |

## 9. Risks

| Risk | Control |
|---|---|
| Orphan on-demand rooms cost money. | Provider deadline at creation; reconcile at start and every 5 minutes; name prefix or label on all rooms. |
| Provider limit errors at creation. | Treat as "machine full" and keep the lease in the queue. Do not fail the lease. |
| The server is a single point of failure. | Leases and queue in SQLite; detached commands on rooms; guests reconnect. No new leases while the server is down. |
| `.env` and other secrets on cloud disks. | Delete at cleanup; destroy the room if cleanup fails; never log contents. |
| Namespace credentials on a VPS. | Only the server has them. Guests use Hostel tokens. TLS on the VPS. |
| Screen state goes from one lease to the next. | Screen tools give each lease a clean screen (simulator clone, clean emulator snapshot) and stop it at cleanup. |
| An agent thinks that a long cold start is a hang. | Progress lines, the "run again" exit code, and an estimated wait. |
| A static room behind NAT (the home MacBook) is not reachable from a VPS. | The room opens an outbound connection to the gateway. |
| Many rooms download tools from GitHub and hit its rate limit. | Optional `mise.github_token`; installed tools stay in the room cache. |
| One tool behaves differently on different OSes (for example an ARM Android image on a Mac, x86 on Linux). | The lease facts show the machine, its OS, and the real option of each tool. The owner lists a tool only on the machines where it should run. |
| A build cache from an earlier lease breaks a build. | `--fresh` requests a new room with empty caches. |
| Windows: SSH access and scripts are not verified. | Windows comes last (M6), on a static Windows host (`win-pc`); scripts in PowerShell. |
| The data path goes through the server, which sees the plaintext. | The server is the owner's own machine. Transfers are small (source files only, about 87 MB for a the test app monorepo on the first sync). |
| A public static room accepts SSH from anywhere. | Guests have no room key; the owner also limits SSH to the server's address (M5). |
| The owner uses Namespace for other work. | Hostel acts only on machines that it created (name prefix or label). |

## 10. Open questions

The owner cannot answer these. The milestones in `plan.md` answer them by
experiment.

1. Can `nsc` and `devbox` log in on a headless Linux VPS? (M5)
2. Does a stopped devbox cost storage, and how much? (M3)
3. Does a Namespace Windows instance have an OpenSSH server? (M6)
4. How do cloud agents install a binary from a private repository? (M5)
5. How long do `simctl clone` and a clean Android snapshot boot take? These times
   select the device reset method. (M2, M4)
6. How does the server get a byte stream to the SSH server of a devbox and of an
   `nsc` instance? (M3, M4)
7. Does the Android emulator run inside a Namespace macOS devbox (virtualization
   inside a virtual machine)? (M4)
8. Future macOS desktop support: can a provision script set Automation Mode and
   Screen Recording on a Namespace devbox (passwordless `sudo`, image
   permissions)? How do on-demand rooms get team signing for Mac Catalyst builds
   (a shared `.p12` and an App Store Connect API key, given to the room as
   secrets)? (not planned yet)
