---
name: hostel
description: The entry point for Hostel. Use it when the user mentions Hostel, rooms, leases, or "a remote Mac/Windows/Linux machine for my agent", and you must find the skill for the task (install, set up a server, add a machine, add a platform, invite a coworker, use a room, fix a problem).
---

# Hostel

Hostel gives coding agents short, temporary, exclusive access to remote machines
for builds, UI QA, and device tests. The agent keeps the code on its own
computer. When it needs another platform (iOS, Android, macOS, Windows, Linux),
it asks the Hostel server for a lease, works on a room over SSH, and releases it.
At the end of each lease, cleanup deletes everything that the lease left.

One Go binary, `hostel`, has all three parts: the server, the guest (the CLI
that agents and people use), and the helper on rooms.

## Vocabulary

Use these plain words. Only Hostel, Guest, and Room come from the hostel theme.

| Word | Meaning |
|---|---|
| server | The central process (`hostel server run`). It keeps the queue, the users, the configuration, and the SSH gateway. |
| guest | The `hostel` CLI on a person's computer. Agents on that computer use it too. |
| room | One concrete machine at runtime: a static host, or a machine that an on-demand provider created. |
| machine | An entry in the configuration. A static machine is one real host (one room). An on-demand machine is a template (up to `max_rooms` rooms). |
| provider | A source of machines: `static` (your own hosts), `namespace` (on-demand cloud), `external` (your own command). |
| platform | What a guest asks for. OS platforms: `linux`, `macos`, `windows` (automatic for each machine with that OS). Device platforms: `ios`, `android`, or your own. |
| device | A device of a device platform, for example `iphone-17-pro` or `pixel-9`. |
| label | A free word on a machine, for example `desktop` or `xcode-26.5`. A lease can require labels. |
| slot | A place for one lease on a room. A machine has one slot by default. |
| lease | One guest's exclusive use of a slot, for one task. It ends at release, or after 10 minutes without a `hostel` command. |
| queue | The leases that wait for a slot, one queue for each platform, in three lanes (`high`, `normal`, `low`). |
| user | A person on the server, with the role `admin` or `member`. |

## Which skill

| The user wants to... | Skill |
|---|---|
| "install hostel", "update hostel", "connect this computer", "open the Hostel app" | `hostel-install` |
| "set up a Hostel server", "run Hostel on a VPS", "on my home machine", "back up the server" | `hostel-server-setup` |
| "add a Mac / Windows PC / Linux box", "add a machine behind NAT", "use Namespace", "remove or drain a machine" | `hostel-rooms` |
| "add iOS / Android", "what does the machine need", "make my own platform", "change a platform script" | `hostel-platforms` |
| "invite a coworker", "make someone admin", "connect my other laptop", "remove a user" | `hostel-users` |
| "build on iOS", "run the tests on Windows", "take a screenshot", "tunnel my local API", "tell my agent to use Hostel" | `hostel-use` |
| "the room is broken", "offline", "acquire fails with 69/78", "TLS key changed", "where are the logs" | `hostel-troubleshoot` |

## Fast facts

- `hostel` alone in a terminal opens the Hostel app (Lobby, Rooms, Front desk).
  Agents use `hostel <verb>` commands only.
- `hostel agent-guide` prints the instructions for agents. It is the source of
  truth for lease commands.
- `hostel help` lists all commands. `hostel --version` prints the version.
- Server configuration lives in the server's database. Admins change it from
  their own computer with `hostel config ...`. Only a few local keys stay in
  `server.toml` on the server machine.
- The design and all contracts are in `docs/design.md` of the repository.
