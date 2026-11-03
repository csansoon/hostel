# Hostel

Short, temporary, exclusive access to remote machines for coding agents.

![Guests (agents on your machines) check in at the Hostel front desk, wait in one queue for each platform, and get a room (iOS, Android, macOS, Windows)](docs/images/hostel.gif)

Agents edit code locally. When they need another platform (iOS, macOS, Android,
Windows, Linux) for builds, UI QA, or device tests, they request a lease from the
Hostel server. The server keeps a queue, gets a room from a provider (a static
machine or an on-demand cloud such as Namespace), prepares it, and gives the agent
SSH access through its gateway. When the lease ends, cleanup deletes everything the
lease left on the room.

Status: milestones M0 to M9 are done (Docker, static, and Namespace rooms; iOS, Android, macOS, Windows, and Linux; desktop testing; users and connection; remote configuration). M10 (the status view: `hostel status`) is in progress; see docs/plan.md.

| Document | Content |
|---|---|
| [AGENTS.md](AGENTS.md) | Rules for coding agents in this repository |
| [docs/context.md](docs/context.md) | Why Hostel exists, first user, history |
| [docs/requirements.md](docs/requirements.md) | Vocabulary, decisions, requirements, risks |
| [docs/design.md](docs/design.md) | Architecture and contracts |
| [docs/plan.md](docs/plan.md) | Milestones |
| [docs/namespace.md](docs/namespace.md) | Namespace facts, limits, and traps |
| [docs/m1-implementation.md](docs/m1-implementation.md) | M1 build order and technical decisions |
