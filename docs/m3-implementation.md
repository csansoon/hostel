# M3 implementation plan

Status: done, waiting for the owner's approval (2026-10-26); the local macOS run of the gate is open (see section 5). Milestone M3 in `plan.md`: a Namespace
devbox as a macOS room. This file gives the build order, the checks with real
devboxes, and the risks. `design.md` stays the contract. When this plan adds a
detail to the contract, the same commit updates `design.md`.

## 1. Facts from the experiments (2026-10-26)

`devbox` 0.0.204 on the owner's Mac, Developer plan. Details in `namespace.md`.

- **Access.** `devbox create` writes `~/.namespace/ssh/<name>.devbox.namespace.key`
  and `.ssh`. The proxy command is `devbox ssh-proxy <name>` (stdin and stdout are
  the SSH stream; it writes "Connecting to ..." to stderr). The SSH server on the
  devbox is the Namespace agent, not OpenSSH: it accepts only that key and ignores
  `~/.ssh/authorized_keys`. The key is stored at Namespace: `devbox configure-ssh
  <name>` writes the same key again when the file is missing.
- **User.** `main_user` in `devbox list -o json`: `devbox` on Linux, `runner` on
  macOS.
- **List.** `devbox list -o json` has `id`, `name`, `main_user`, `created_at`,
  `documented_purpose` (from `--purpose`), but no running or stopped state. With no
  devbox, it prints a text line before the JSON (`No devbox available yet. ...`).
  A name prefix identifies Hostel devboxes.
- **Errors.** Over the limit: `rpc error: code = ResourceExhausted desc = failed to
  create devbox: per-user devbox limit reached (1)`. Delete of a missing devbox:
  `rpc error: code = NotFound desc = no such devbox with id or name "<name>"`.
- **Times.** Create: Linux S 7–12 s, macOS M (`tahoe-slim`) 27 s. Stop: 17–20 s.
  Resume through the SSH proxy: Linux 5 s, macOS 19 s. Delete: 1 s.
- **Persistence.** Linux: the whole system persists (`whole_system_persistency`).
  macOS: only the workspace volume (`/Volumes/devbox`, `~/workspaces`) persists. The
  home directory (with `~/hostel`), the simulators, and DerivedData are lost at
  each stop.
- **Ephemeral.** `--ephemeral`: a stop deletes the devbox (it is gone from the
  list). An idle ephemeral devbox (`--auto_stop_idle_timeout 5m`) was deleted after
  972 s.
- **Idle stop.** With `--auto_stop_idle_timeout 5m`, the devbox stopped after
  894 s. An open SSH connection, a session in the last 15 minutes, or a file in
  `$NAMESPACE_DEVBOX_TASKS_DIR` keeps it active (Namespace docs).
- **Maximum duration.** 3 hours on the Developer plan; then Namespace shuts down
  the instance, also with an open connection (Namespace docs).
- **macOS image `tahoe-slim`.** macOS 26.6.2, arm64, 6 CPU, 14 GB, 137 GB free.
  Xcode 26.2 to 27.1 in `/Applications/Xcode_<version>.app`; the default is 26.4.1.
  iOS runtimes 18.6 to 27.1, with two 26.4 builds. `sudo` works without a password.
  mise is in `/opt/mise/bin`. `screencapture -x` over SSH gives the desktop
  (2560×1600) without a permission prompt.
- **Cost.** Devbox minutes cost $0.004. macOS M uses 15 per minute ($3.60 per
  hour); Linux S uses 1. Storage: $0.20 per GB-month of data.

## 2. Decisions from the facts

- **Ephemeral devboxes only.** A stopped macOS devbox loses everything that a room
  needs, so a stop has no value. The provider creates each devbox with
  `--ephemeral`, and its capabilities say "cannot stop": an idle room is destroyed
  after `warm_running`. Provider-side bounds: the idle stop (it deletes an
  ephemeral devbox: measured 972 s after the last activity) and the plan's
  maximum duration. A devbox that a dead server leaks costs about 16 minutes of
  idle time after its last job.
- **The devbox key is the room key.** `RoomAccess.Signers` is the key file from the
  `devbox` CLI, not the server key. If the file is missing, the provider runs
  `devbox configure-ssh <name>`. Destroy deletes the two files.
- **No idle stop during a job.** The job supervisor writes
  `$NAMESPACE_DEVBOX_TASKS_DIR/hostel-<slot>.<job>` and deletes it when the job
  ends; cleanup deletes the markers of killed supervisors. A marker for the whole
  lease would keep a leaked devbox up to the plan maximum (3 hours, about $11), so
  only jobs have one (review, 2026-10-26).
- **Xcode selection by machine env.** For example
  `env = { DEVELOPER_DIR = "/Applications/Xcode_26.5.app/Contents/Developer",
  HOSTEL_IOS_RUNTIME = "26.5" }` with the label `xcode-26.5`.
- **`max_lifetime` is required for a devbox machine** (the plan's maximum
  duration, `3h` on the Developer plan).
- **The desktop screenshot** uses the existing `macos` script (`screencapture`).
  No provider screenshot is necessary.

## 3. Build order

1. `internal/provider/namespace.go`: the `namespace` provider for `spec.product =
   "devbox"`, through the `devbox` CLI on the server's `PATH`:
   - `Provision`: if `devbox list` has the name, wait until it accepts SSH and
     return it; else `devbox create
     --name <name> --platform <p> --size <s> --image <i> --ephemeral
     --auto_stop_idle_timeout 15m --no_checkout --purpose "hostel room <name>"`. On
     an error, list again: a devbox with the name counts as success.
     `ResourceExhausted` is `ErrLimit`.
   - `Destroy`: `devbox delete <name> --force`; `NotFound` is `ErrNotFound`.
   - `List`: `devbox list -o json`, names with the prefix.
   - `Access`: user `main_user`, `Dial` through `devbox ssh-proxy <name>`, the
     devbox key.
   - Deadlines: create 10 minutes, other calls 1 minute.
   - At server start: `devbox auth check-login`; a failure stops the start.
2. The task marker in the job supervisor, and its removal in cleanup.
3. `test/fakedevbox`: a fake `devbox` command for the E2E tests. It makes Docker
   rooms, like `test/fakeprovider`, and copies the real output formats (the list
   JSON, the text line of an empty list, the error lines). The E2E tests put it on
   the server's `PATH`. Scenarios: acquire and release on a devbox room; the
   limit; a missing key file; reconcile after `kill -9` of the server during a
   create.
4. Opt-in smoke test (`HOSTEL_SMOKE_NAMESPACE=1`) against the real account:
   acquire `macos` on a devbox, run a command, take a desktop screenshot, release,
   wait until the devbox is deleted. Then a `kill -9` of the server during a lease
   and a restart: no devbox remains.
5. The M2 ExampleApp flow on a devbox (iOS build, install, launch, sign-in through the
   tunnel, screenshot), by hand, with the evidence in section 5.

## 4. Risks

| Risk | Plan |
|---|---|
| The `devbox` CLI output changes (it is not a stable API). | The provider parses only `id`, `name`, `main_user`; the fake copies the real formats; the smoke test shows a change. |
| The CLI asks a question when a flag is missing. | Every call has all flags, stdin from `/dev/null`, and a deadline. |
| A leaked devbox costs money. | Ephemeral devboxes; reconcile deletes devboxes with the server's prefix and no room; the smoke test checks `devbox list` at the end. |
| The 1-devbox limit of the plan. | `max_rooms = 1` for the machine; the limit error is `ErrLimit`. |
| After a restart during `devbox create`, the new server finds the devbox by name while it still boots; the first connection fails. | `Provision` waits until an SSH handshake works when its own create did not make the devbox (seen in `TestNamespaceKill` under load, 2026-10-26). `TestNamespaceKill` checks that the room stays the same and that the new server makes no second create call. |
| An idle devbox is deleted between leases. | `warm_running` (default 5 minutes) is shorter than the idle stop (about 15 minutes); reconcile marks a deleted room as lost. |

## 5. Results

- 2026-10-26, review of the provider (a subagent): a task marker for the whole
  lease would keep a devbox that a dead server leaked up to the plan maximum.
  Fix: one marker for each running job. Also fixed: text after the list JSON,
  a key file race, `ResourceExhausted` only from a create is the limit, and tests
  that did not check their claims (the same room after `kill -9`, the foreign
  devbox stays, the marker after cleanup).
- 2026-10-26, smoke test (`make test-smoke-namespace`) against the real account:
  passed. A new macOS devbox (`tahoe-slim`, M): the `macos` lease was ready after
  66 s; `run` as `runner` on macOS 26.6.2; the job's task marker; a desktop
  screenshot (2560×1600); `kill -9` of the server and a restart during the lease;
  release; the devbox was deleted. Total 2 min 26 s. `devbox list` was empty
  before and after.
- 2026-10-26, the M2 ExampleApp flow on a devbox (server on the owner's Mac; machine
  `namespace/mac` with `DEVELOPER_DIR` for Xcode 26.5 and the label `devbox`):
  - The first three tries failed: the server ran the `ios` health script at room
    provisioning, before the `ios` provision script, which an on-demand room runs
    only at the first lease. Fix: `health` runs only for platforms that the room is
    ready for, at provisioning and at cleanup (the server sends the list in the
    cleanup request). The `fakedevice` health script now needs the result of its
    provision script, so the E2E tests of on-demand rooms show this case.
  - Lease timeline: create 18 s, room provisioning (helper, mise, agent-device)
    41 s, `ios` provision (two golden simulators, Xcode 26.5, iOS 26.5) 74 s,
    prepare 14 s: ready 2 min 27 s after the request. With the ExampleApp setup job
    (`tuist install`, `tuist generate`), `acquire` returned after 240 s.
  - Cold ExampleApp build (`xcodebuild ... build`): 57 s (MacBook: 2 min 45 s).
  - Install and launch; the tunnel from the devbox to the local API (HTTP 200);
    the app signed in as `user@dev.local`, an account that exists only in this
    worktree's database (agent-device read it in the sidebar);
    `hostel screenshot`.
  - Release, then `kill -9` of the server while the room was warm, and a restart:
    the server deleted the devbox when `warm_running` ended; `devbox list` was
    empty, and the key files were gone.
  - Cost of the session: the devbox ran about 14 minutes (about $0.85).
- 2026-10-26, stability gate in CI (owner decision: the workflow "E2E gate", 10
  parallel jobs): 10 of 10 passed on d7bf59f (612–747 s each) and 10 of 10 on
  90363d3 (after a test fix). The local macOS runs failed only on timeouts under
  a load average of 100–465 from other projects: first `TestNamespaceLimit` (a
  fixed 6 s window; fixed: it waits for the limit message with a deadline), then
  `TestForwards` (an SSH request ended with EOF), `TestNamespaceKill`, and
  `TestWarmDestroy` (`acquire` waits ended). A local run with a normal load is
  still to do.
- 2026-10-27, review of the pull request (a subagent). Fixed:
  - Only `ResourceExhausted` with the text "devbox limit reached" is the limit.
    A rate limit on create was a limit too, so the server evicted an idle room
    for nothing (`TestNamespaceCreateError`).
  - `TestNamespaceKill` did not prove that the restarted server finds the
    devbox: the fake made the container only after its delay. Now the fake
    devbox exists from the start and boots on its own, and the test checks for
    one create call.
  - The key is always read under the name lock (no half-written file).
  - `Provision` stops waiting for SSH when the devbox is not in the list.
  - The gate workflow has a concurrency group: a new start with the label stops
    the old run.

  Not changed:
  - A running job keeps its devbox up to the plan maximum when the server is
    gone for good (design: one marker for each job only).
  - A failed login check stops the server start (as for `external`).
  - `max_lifetime` has no upper limit: plans differ.
