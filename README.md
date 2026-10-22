# Hostel

Short, temporary, exclusive access to remote machines for coding agents.

Agents edit code locally. When they need another platform (iOS, macOS, Android,
Windows, Linux) for builds, UI QA, or device tests, they request a lease from the
Hostel server. The server keeps a queue, gets a room from a provider (a static
machine or an on-demand cloud such as Namespace), prepares it, and gives the agent
SSH access through its gateway. When the lease ends, cleanup deletes everything the
lease left on the room.

Status: milestone M1 (core with Docker rooms) is in review in PR #1. Next: M2 in docs/plan.md.

| Document | Content |
|---|---|
| [AGENTS.md](AGENTS.md) | Rules for coding agents in this repository |
| [docs/context.md](docs/context.md) | Why Hostel exists, first user, history |
| [docs/requirements.md](docs/requirements.md) | Vocabulary, decisions, requirements, risks |
| [docs/design.md](docs/design.md) | Architecture and contracts |
| [docs/plan.md](docs/plan.md) | Milestones |
| [docs/namespace.md](docs/namespace.md) | Namespace facts, limits, and traps |
| [docs/m1-implementation.md](docs/m1-implementation.md) | M1 build order and technical decisions |
