# Hostel

Short, temporary, exclusive access to remote machines for coding agents.

![Guests (agents on your machines) check in at the Hostel front desk, wait in one queue for each platform, and get a room (iOS, Android, macOS, Windows)](docs/images/hostel.gif)

Coding agents edit code on your computer. When they need another platform for a
build, a UI check, or a device test (an iOS simulator, an Android emulator, a
macOS or Windows desktop app, Linux), they ask Hostel for a **lease**. The Hostel
server keeps one queue for each platform, gives the agent a **room** (one of your
machines, or an on-demand cloud machine), and deletes everything that the lease
left when it ends. The agent's code stays local: Hostel sends the changed files.

- Many agents, many people, few machines: leases are exclusive and queued, with
  priorities and wait estimates.
- Your own machines (a Mac, a Windows PC, a Linux box; also behind NAT) and
  on-demand machines (Namespace, or your own provider script).
- iOS, Android, macOS, Windows, and Linux: clean devices for each lease,
  screenshots, desktop app control, and tunnels from the room to your local
  servers.

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/csansoon/hostel/main/install.ps1 | iex
```

Both install the latest [release](https://github.com/csansoon/hostel/releases)
and check it against its `SHA256SUMS`. Run the same command again to update.

## For humans

Run `hostel` in a terminal. It opens the Hostel app:

```sh
hostel
```

- **Check in**: the first time, paste the invite code that an admin of your
  server gave you. The app connects your computer, and your agents use the same
  connection.
- **Lobby**: each platform (iOS, Android, macOS, …) with its free machines and
  its waiting line. Open a platform to see the whole queue; admins move entries
  and change priorities.
- **Rooms**: each machine, its state (Vacant, Occupied, Offline, …), and who uses
  it. Open a machine to see its hardware, its live CPU and memory use, and the
  current lease with its branch and running job. You can end your own lease;
  admins can end any lease, stop new leases, and repair a broken room.
- **Front desk**: the server, the guest list, and Check out. Members can make a
  code for their other computer. Admins invite guests (Invite a guest), change
  roles, and add machines (Add a room).

The footer shows what the selected item is and which keys work. `?` lists all
keys; `1` `2` `3` jump to Lobby, Rooms, and Front desk; `q` quits.

### Your own server

No server yet? Ask your coding agent to set one up with the skills below, for
example:

> Read the skills at https://github.com/csansoon/hostel/tree/main/skills and set
> up Hostel for me: a server on my VPS, my Mac as an iOS room, and an invite for
> me.

Or do it yourself on a Linux machine that your computers can reach:

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sudo sh -s -- --dir /usr/local/bin --server /opt/hostel
hostel server init --listen 0.0.0.0:7410 --url https://hostel.example.com:7410 --helper-dir /opt/hostel/helpers
hostel server run
```

Run `hostel server init` and `hostel server run` as the user of the service (not
root), and open TCP 7410.
`hostel server init` prints the invite code for your computer; use it within 24
hours. Then open the app and add your machines in Front desk › Add a room. The
[`hostel-server-setup`](skills/hostel-server-setup/SKILL.md) skill has the
details (a service, a proxy with TLS, backups).

## For agents

Agents use the `hostel <verb>` commands, never the app. Hostel comes with skills
in [`skills/`](skills/), in the `SKILL.md` format that Claude Code, Codex, and
other agents read. Start with [`hostel`](skills/hostel/SKILL.md): it says which
skill fits a task.

- [**`hostel-install`**](skills/hostel-install/SKILL.md): install or update the
  CLI on macOS, Linux, or Windows; connect a computer with an invite code
  (`hostel connect hostel1-…`); check it with `hostel --version` and
  `hostel status`.
- [**`hostel-server-setup`**](skills/hostel-server-setup/SKILL.md): run a server
  on a VPS or a machine at home: `hostel server init` (its own TLS, or behind a
  proxy), a systemd service, the configuration in the database
  (`hostel config edit`), backups, updates.
- [**`hostel-rooms`**](skills/hostel-rooms/SKILL.md): add machines: over SSH, from
  behind NAT (`hostel config rooms add`, then `hostel machine join` on the
  machine), or on demand (Namespace, or an `external` provider); limits, warm
  rooms, drain, repair, remove; offline machines.
- [**`hostel-platforms`**](skills/hostel-platforms/SKILL.md): what each platform
  needs on a machine (Xcode and its label for iOS; Java, and KVM on Linux, for
  Android, whose SDK Hostel downloads; the `desktop` label for desktop apps),
  devices, labels, and your own platforms with their scripts.
- [**`hostel-users`**](skills/hostel-users/SKILL.md): invite people
  (`hostel config users invite`), roles (admin, member), a code for another
  computer, remove a user or one computer.
- [**`hostel-use`**](skills/hostel-use/SKILL.md): the work loop:
  `hostel platforms`, `hostel acquire <platform> --task "…"` (exit code 75: run it
  again), `hostel run`, `hostel sync`, `hostel screenshot`, `hostel release`;
  `hostel.toml` in a repository for setup commands and tunnels to a local API.
- [**`hostel-troubleshoot`**](skills/hostel-troubleshoot/SKILL.md): room and lease
  states, offline and broken machines, repair and changed host keys, logs,
  common errors (exit codes 69 and 78, a changed TLS key).

A typical lease:

```sh
hostel platforms
hostel acquire ios --task "check the sign-in screen"
hostel run -- 'xcodebuild test -scheme MyApp -destination "id=$SIMULATOR_UDID"'
hostel screenshot -o screen.png
hostel release
```

`hostel agent-guide` prints the instructions for agents that come with the
installed version. To make your agents use Hostel, add this
to their instructions file (for example `AGENTS.md` or `CLAUDE.md`):

```markdown
## Remote machines (Hostel)

This computer can borrow remote machines for builds, UI QA, and device tests on
other platforms. Before the first use in a session, run `hostel agent-guide`
and follow it. Always run `hostel release` when you are done.
```

## Documentation

| Document | Content |
|---|---|
| [docs/design.md](docs/design.md) | Architecture and contracts |
| [docs/requirements.md](docs/requirements.md) | Vocabulary, decisions, requirements |
| [docs/deploy-vps.md](docs/deploy-vps.md) | A server on a VPS |
| [docs/static-room-setup.md](docs/static-room-setup.md) | Your own machines as rooms |
| [docs/namespace.md](docs/namespace.md) | On-demand rooms on Namespace |
| [docs/plan.md](docs/plan.md) | Milestones |
| [AGENTS.md](AGENTS.md) | Rules for coding agents that work on Hostel itself |

Status: version 0.1. Milestones M0 to M10c are done; M11 (hardening against
backdoors) is planned. See [docs/plan.md](docs/plan.md).

## License

[MIT](LICENSE)
