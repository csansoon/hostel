# Hostel

Short, temporary, exclusive access to remote machines for coding agents.

![Guests (agents on your machines) check in at the Hostel front desk, wait in one queue for each platform, and get a room (iOS, Android, macOS, Windows)](docs/images/hostel.gif)

Coding agents edit code on your computer. When they need another platform for a
build, a UI check, or a device test (an iOS simulator, an Android emulator, a
macOS or Windows desktop app, Linux), they ask Hostel for a **lease**. The Hostel
server keeps one queue for each platform, gets a **room** (one of your machines,
or an on-demand cloud machine), prepares it, and gives the agent SSH access
through its gateway. The agent's code stays local: Hostel sends the changed files
to the room. When the lease ends, Hostel deletes everything that the lease left
on the room.

Hostel is one Go binary with three modes: the **server**, the **guest** (the CLI
that agents and people use), and the **room** helper.

- Many agents, many people, few machines: leases are exclusive and queued, with
  priorities, estimates, and admin controls.
- Your own machines (a Mac, a Windows PC, a Linux box; also behind NAT) and
  on-demand machines (Namespace, or your own provider script).
- iOS, Android, macOS, Windows, and Linux, with clean devices for each lease,
  screenshots, desktop app control, and tunnels from the room to your local
  servers.
- For people: `hostel` opens the Hostel app in your terminal: the platforms and
  their queues, the machines, the people, invites.

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/csansoon/hostel/main/install.ps1 | iex
```

Both download the latest [release](https://github.com/csansoon/hostel/releases)
and check it against its `SHA256SUMS`.

## Start

1. **A server** on a machine that your computers and rooms can reach (a small
   VPS, or a machine at home):

   ```sh
   curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sudo sh -s -- --dir /usr/local/bin --server /opt/hostel
   hostel server init --listen 0.0.0.0:7410 --url https://hostel.example.com:7410 --helper-dir /opt/hostel/helpers
   hostel server run
   ```

   `hostel server init` writes the configuration and prints the invite code for
   your first computer; `hostel server run` starts the server. Run it as a
   service, and open TCP 7410. Behind a proxy with TLS, or for details, see the
   `hostel-server-setup` skill.

2. **Connect your computer** with that code:

   ```sh
   hostel connect hostel1-…
   ```

3. **Add a room.** On your computer (as the admin), make a join code for the
   new machine, for example a Mac for iOS:

   ```sh
   hostel config rooms add mac-mini --platform ios --label xcode-26.5
   ```

   (Or in the app: Front desk › Add a room.) Then run the command that it prints
   on that Mac. The `ios` platform needs Xcode on the Mac and the label of its
   version. See the `hostel-rooms` and `hostel-platforms` skills; they also cover
   machines over SSH and on-demand machines.

4. **Use it.** Open the app with `hostel`, or let an agent work:

   ```sh
   hostel platforms
   hostel acquire ios --task "check the sign-in screen"
   hostel run -- 'xcodebuild test -scheme MyApp -destination "id=$SIMULATOR_UDID"'
   hostel release
   ```

## For agents

Hostel comes with skills for coding agents in [`skills/`](skills/), in the
`SKILL.md` format that Claude Code, Codex, and other agents read. Ask your agent
to read them, for example:

> Read the skills at https://github.com/csansoon/hostel/tree/main/skills and
> set up Hostel for me: a server on my VPS, my Mac as an iOS room, and invites for
> my team.

| Skill | For |
|---|---|
| [`hostel`](skills/hostel/SKILL.md) | What Hostel is; which skill to use |
| [`hostel-install`](skills/hostel-install/SKILL.md) | Install the CLI; connect a computer |
| [`hostel-server-setup`](skills/hostel-server-setup/SKILL.md) | Set up and run a server |
| [`hostel-rooms`](skills/hostel-rooms/SKILL.md) | Add machines: over SSH, joined from behind NAT, on demand |
| [`hostel-platforms`](skills/hostel-platforms/SKILL.md) | iOS, Android, macOS, Windows, Linux; devices; own platforms |
| [`hostel-users`](skills/hostel-users/SKILL.md) | Invites, roles, computers |
| [`hostel-use`](skills/hostel-use/SKILL.md) | Leases, the queue, `hostel.toml`, tunnels |
| [`hostel-troubleshoot`](skills/hostel-troubleshoot/SKILL.md) | Offline and broken rooms, logs, errors |

On a connected computer, `hostel agent-guide` prints the instructions for agents
that come with the installed version. To make your agents use Hostel, add this
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
