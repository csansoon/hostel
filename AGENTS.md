# AGENTS.md

Hostel gives coding agents short, temporary, exclusive access to remote machines
for builds, UI QA, and device tests. It has three pieces: a central server,
guests (the CLI that agents use), and machines. One Go binary contains all three
modes.

## Read first

Read these files in full before you change code:

1. `docs/context.md`: why Hostel exists, typical use, and history.
2. `docs/requirements.md`: vocabulary, owner decisions, requirements, risks.
3. `docs/design.md`: architecture and contracts.
4. `docs/plan.md`: milestones. Work only on the current milestone.
5. `docs/namespace.md`: before you work on a Namespace provider.

When a decision changes the design, update the docs in the same change. Record a new
owner decision in `requirements.md`, section 3, with the date.

## Owner communication

- Report to the owner only in ASD-STE100 Simplified Technical English.
- Use short, direct sentences. State evidence, uncertainty, and blockers clearly.
- At the end of each milestone, stop, show the evidence (test output, screenshots),
  and wait for approval.
- For each pull request that you create, follow `.claude/skills/pr-ready/SKILL.md`
  (owner instruction, 2026-10-26): a local review by a subagent, fixes pushed
  without approval, every review comment resolved or answered, and a report to
  the owner when the pull request is ready to merge. Do not merge.
- Make local git commits at logical steps, and at least one at the end of each
  milestone. Do not create the GitHub repository, push, or open a pull request unless
  the owner asks for it in the conversation.

## Vocabulary

Only Hostel, Guest, and Room come from the hostel theme. Use the plain terms from
`requirements.md` (provider, template, machine, room, tool, lease, queue, cleanup) in code, CLI text, and docs. Do not add new themed words. CLI verbs stay plain
(`acquire`, `run`, `release`). Exception (owner decision, 2026-11-03): the labels of the
Hostel app (`hostel` without arguments) may be themed (Lobby, Rooms, Front desk, Check in,
Check out, Guest, Guest list, Vacant, Occupied, Checked in, Out by, Waiting line, Typical
stay, Next machine), each with a plain description (in the footer for a selectable item).

## Toolchain

- Go version: `mise.toml`. Run `mise install` once; then use `mise exec -- go ...`,
  or activate mise in your shell.
- Docker is necessary for the E2E tests (on a Mac, for example OrbStack).
- Pure Go dependencies only (no cgo), so that `GOOS`/`GOARCH` cross-builds work.

## Engineering rules

- **Smallest complete change.** No speculative abstractions, placeholders, dead
  code, or unrelated cleanup. Do not hide errors or weaken types to make a change
  pass.
- **Comments are rare.** Prefer clear names and types. Add a short comment only for
  a critical invariant, a security constraint, an external contract, or a
  non-obvious workaround. Explain why, not what.
- **Errors.** Wrap errors with context (`fmt.Errorf("provision machine: %w", err)`).
  Messages for guests must say what failed and what to do next.
- **No I/O in transactions.** Never hold a SQLite transaction during a provider
  call, an SSH operation, or a script. Read the state, do the I/O, then write with a
  compare-and-set.
- **Deadlines.** Every provider call, SSH operation, HTTP request, and external
  command has a `context` deadline. No unbounded retries.
- **Idempotent steps.** Each server worker step must be safe to repeat after a
  restart.
- **Secrets.** Never log tokens, private keys, file contents, environment values,
  or the output of guest jobs. Platform script output tails may go to the event log.
- **Cost safety.** Code that creates cloud machines must set a provider-side bound
  (a deadline, or an idle auto-stop for machines that can stop) and must act only on
  machines that Hostel created (name prefix or label).

## Testing rules

- **E2E tests are the quality gate.** They run the real server, guest, and helper
  binaries against Docker machines (`docs/design.md`, section 20). Add or extend an E2E
  scenario for each changed behavior.
- **Mocks.** Mock only third-party services, at their adapter boundary. The
  `external` fake provider is the boundary for cloud providers. Do not mock internal
  packages, the store, or the gateway.
- **Unit tests.** Do not add unit tests by default. A unit test is allowed only for
  small, complex, deterministic code that E2E tests cannot cover well (for example
  the scheduler order or the sync plan). Write the reason in the test file.
- **No fixed sleeps as assertions.** Wait for a condition with a deadline. Use short
  configured durations in tests, with wide margins.
- **Stability.** From M1, a milestone is done only when the whole E2E suite passes
  in 10 separate runs in CI (the label `e2e-10` on the pull request starts the
  workflow "E2E gate"), and in one local run on macOS (`make test-e2e`) when the
  Mac has a normal load. `make test-e2e-10` runs the 10 runs locally.
- **Shared machine.** A developer's Mac can also run other projects. Their Docker
  containers (for example `app-postgres` and `app-storage`) and local servers must
  not be touched. E2E tests label every container, network, and volume that they
  create with `hostel-e2e=<run id>`, and remove only objects with their own run ID,
  or objects with a `hostel-e2e` label that are older than 2 hours (left by a crashed
  run). Never run `docker system prune`, `docker rm -f $(docker ps -aq)`, or another
  command that acts on objects that the tests did not create. Tests use free random
  ports on `127.0.0.1` for the server, the gateway, and the machine containers; never
  fixed ports such as 3000, 5432, 7410, or 9000.
- **Real providers.** Namespace tests are opt-in (`HOSTEL_SMOKE_NAMESPACE=1`) and
  cost money. Before a real test, run `devbox list -o json` (or `nsc list -o json`).
  The plan allows one devbox. If a devbox that Hostel did not create exists, stop and
  ask the owner. Destroy everything that the test created, also when the test fails.

## Commands

The `Makefile` from milestone M0 defines them. Expected:

- `make build`: host binary in `dist/hostel`.
- `make dist`: all `hostel-<goos>-<goarch>` binaries, plus the pinned
  `mise-<goos>-<goarch>` binaries (checksums verified).
- `make test-e2e-10`: the whole E2E suite 10 times, as separate runs; stops at the
  first failure.
- `make lint`: fails if `gofmt -l` lists a file; `go vet -tags e2e ./...`.
- `make test-unit`: the allowed unit tests.
- `make test-e2e`: E2E tests (needs Docker; from M1).
