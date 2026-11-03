# M10c implementation plan

Status: planned (2026-11-03). Milestone M10c in `plan.md`: the public release.
Owner decisions: `requirements.md`, section 3, 2026-11-03.

## 1. Facts (2026-11-03)

- The repository is private; it has no LICENSE, no tag, and no release.
- `make release` builds the release files (all binaries, the pinned mise
  binaries, `hostel-platforms.tar.gz`, `SHA256SUMS`); `.github/workflows/release.yml`
  creates a GitHub release on a `v*` tag; `install.sh` installs from a release
  and checks each file against `SHA256SUMS` (it needs `gh` while the repository
  is private).
- An audit of the whole history (2026-11-03) found no secrets. It found
  personal and internal details: the owner's server address and VM names, the
  owner's machine names and hardware, details of ExampleApp, and the owner's name in
  examples.

## 2. Decisions

- **License:** MIT.
- **README** for people: what Hostel is (the three pieces), who it is for, the
  one-line install, a short start (a server, a room, connect, a lease), the
  Hostel app, and where the skills and the design are.
- **Skills** for agents, in the open `SKILL.md` format (`skills/<name>/SKILL.md`,
  front matter `name` and `description`): `hostel` (the entry point: which skill
  for which question), `hostel-install`, `hostel-server-setup`, `hostel-rooms`,
  `hostel-platforms`, `hostel-users`, `hostel-use`, `hostel-troubleshoot`. Each
  skill has the commands, the configuration keys, and the checks; the design
  stays in `docs/design.md`. They are written in plain technical English.
- **Install:** `curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh`
  (macOS, Linux; `--server DIR` for a server) and
  `irm https://raw.githubusercontent.com/csansoon/hostel/main/install.ps1 | iex`
  (Windows). Both download the latest release and check `SHA256SUMS`.
- **Docs:** the current files use generic names (`hostel.example.com`,
  `win-pc`, `mac-mini`, "Alex Example") and no details of the first app beyond
  what Hostel needs. The milestone files stay in `docs/` (a `docs/internal/`
  folder would be public too).
- **`hostel server init`** (gaps that the skills found): `--helper-dir` writes
  `helper_dir` (for `install.sh --server`); with `--tls off` and an `https://`
  address, it writes `gateway_public = "wss://<host>/v1/gateway"`.
- **History:** after the PR is merged, `git filter-repo --replace-text` on a
  mirror clone with the same replacements, a force-push of all branches, and a
  request to GitHub Support for the old pull request refs. The owner confirms the
  list of replacements before the force-push.
- **Going public:** after the owner's OK, `gh repo edit --visibility public`,
  then the tag `v0.1.0` (the release workflow builds the release).

## 3. Build order

1. LICENSE, the decisions, this plan.
2. The skills.
3. README, `install.ps1`, `install.sh` without the private-repository note.
4. The cleanup of the current files (generic names, fewer details).
5. Review, PR, merge; the history rewrite; public; `v0.1.0`; the done-when check.

## 4. Tests

- E2E: the suite stays as it is (no behavior change).
- `install.sh` and `install.ps1`: checked against the `v0.1.0` release on a
  clean machine (a Docker container for Linux, `win-pc` over SSH for Windows,
  this Mac) at the done-when.
- The skills: a fresh agent (a subagent without this conversation) follows
  only the README and the skills to set up a server with a Linux machine with
  SSH as a room (a container; Hostel ships no Docker provider) and run a lease,
  at the done-when.
