# M11 implementation: hardening against backdoors

## 1. Why

A guest must not keep access to a machine after its lease, also with a backdoor
that it installed during the lease (design, section 18, "Backdoors"). In version 1
the guest and the server use the same OS user, and machines are reused. So a
process without `HOSTEL_LEASE`, a crontab entry, a user agent or unit, a changed
shell startup file, or a changed binary in a shared cache can survive cleanup.

## 2. Owner decisions (2026-11-13)

1. **Lease users are opt-in for each machine** (`lease_users = true`), on macOS and
   Linux. A lease whose tools need the account of the machine user (a desktop, the
   iOS simulator, the Android emulator with its golden AVDs) keeps the machine user
   and gets the cleanup audit. Windows does not change.
2. **A narrow sudo rule** gives the root step. `hostel machine join` installs a
   root-owned binary and a sudoers rule that lets the machine user run only
   `hostel lease-user ...`. The machine guide shows the same steps for a machine
   that the server reaches over SSH.
3. **The egress firewall comes later**, not in M11.
4. **The audit removes new persistence and continues.** It records an event that
   names each item. The machine becomes `broken` only when an item cannot be
   removed.

## 3. Order

| Phase | Content |
|---|---|
| 1 | Docs: this plan, decisions, design section 18 |
| 2 | Cleanup audit of persistence for the machine user |
| 3 | No reuse: an on-demand machine is destroyed after each lease (`reuse = false`) |
| 4 | Lease users: the root command, the sudo rule, join, the lease flow |
| 5 | Read-only shared caches and a writable cache for each lease user |
| 6 | Machine guide, a real Mac, evidence |

## 4. Phase 2: the cleanup audit

The helper takes a snapshot of the persistence locations of the machine user at
`begin`, when no other room of the machine has a lease (as for `authorized_keys`).
At cleanup, when no other room has a lease, it compares and repairs:

| Location | Repair |
|---|---|
| `crontab -l` | install the snapshot again (or `crontab -r` when there was none) |
| `~/.config/systemd/user/` (Linux) | stop and disable new units, delete new files |
| `~/Library/LaunchAgents/` (macOS) | boot out new agents, delete new files (`macos-desktop` already does this for its leases) |
| Shell startup files (`.profile`, `.bashrc`, `.bash_profile`, `.bash_login`, `.zshrc`, `.zshenv`, `.zprofile`, `.zlogin`, `.config/fish/config.fish`) and `~/.ssh/rc`, `~/.ssh/environment` | restore the snapshot copy, or delete a new file |

The cleanup result names each repaired item; the server writes them to the event
log (names only, no file contents). A failed repair fails the cleanup, so the
machine becomes `broken` (R24).

E2E: a lease adds a crontab entry, a systemd user unit file, a line in `.bashrc`,
and `~/.ssh/rc`. After release, the next lease finds none of them, and the event log
names them. The machine image gets `cron` (the `crontab` command).

## 5. Phase 3: no reuse

A template key `reuse` (on-demand only, default `true`). With `reuse = false`, the
server destroys the machine after the cleanup of each lease, instead of making it
free. No backdoor survives, at the cost of a cold start for each lease. E2E with the
`external` fake provider: two leases get two different machines, and the first one
is destroyed.

## 6. Phase 4: lease users

- **Root command.** `hostel lease-user create|delete|check` in a root-owned copy of
  the binary (`/etc/hostel/hostel`, where the macOS connect service already has it).
  - `create <name>` with the server's public key on stdin: makes a user without
    administrator rights, a home directory with mode 0700, the key in its
    `authorized_keys`, and membership of the group `hostel-leases`. Linux:
    `useradd`; macOS: `sysadminctl` or `dscl`.
  - `delete <name>`: ends all processes of the user, removes its crontab and `at`
    jobs, its systemd user manager (Linux) or launchd user domain (macOS), its files
    in `/tmp` and `/var/tmp`, and the user and its home directory.
  - `check`: prints the version, so that the server can see that the root step is
    installed.
  - The command accepts only names `hostel-l<lease prefix>` and deletes only users
    that it created (a marker in the user's record). It never takes a path.
- **Sudo rule.** `/etc/sudoers.d/hostel`:
  `<machine user> ALL=(root) NOPASSWD: /etc/hostel/hostel lease-user *`.
  `hostel machine join` writes it when the machine has `lease_users`; the machine
  guide shows it for SSH machines.
- **Lease flow.** With `lease_users = true` and no tool of the lease that needs the
  machine user (a new tool key `machine_user = true`, set for `macos-desktop`,
  `ios-simulator`, `android-emulator`, `windows-desktop`):
  1. `preparing`: the server runs `sudo -n /etc/hostel/hostel lease-user create`
     as the machine user, then does the room steps (begin, `start`, the guest's
     connections) as the lease user, as for room users (design, section 7).
  2. Cleanup: the room steps as the lease user, then `lease-user delete` as the
     machine user, then `check` as the machine user.
  3. A repeated step is safe: `create` of an existing lease user of the same lease
     changes nothing; `delete` of a missing user succeeds.
- **Provisioning.** With `lease_users`, the server runs `lease-user check`. When
  the root step is missing or older than the helper needs, the machine becomes
  `broken` with a message that tells the admin what to install.

E2E: the machine image gets the root binary and the sudo rule (the harness copies
the binary in as root). Scenarios: a process started with `env -i` (no
`HOSTEL_LEASE`) that holds a connection open, a crontab entry, a systemd user unit,
and a file in `/tmp`; none survives the release, and the user is gone.

## 7. Phase 5: shared caches

- The machine user's `hostel/` directory gets the group `hostel-leases`, with group
  read and traverse rights on `bin`, `tools`, `mise`, and `cache`, and none on
  `rooms`, `authorized_keys.base`, and the state files. The home directory gets
  group traverse rights only.
- A lease user gets its own `hostel/` directory with `rooms/` and its own writable
  `cache/` (deleted with the user). `HOSTEL_CACHE_DIR` points to it.
- mise: `MISE_DATA_DIR` in the lease user's cache, and
  `MISE_SHARED_INSTALL_DIRS` set to the machine's mise installs, so the machine's
  tools work without a new install and cannot be changed. A repository tool that
  the machine does not have goes into the lease user's own directory.
- Tool paths with `{cache}` (for example the Android SDK) use the machine's cache,
  read-only.

E2E: a lease user cannot write a file in the shared cache or change a mise tool;
the next lease runs the original binary.

## 8. Phase 6: guide and evidence

- `machine-setup.md`: lease users (join option, the sudo rule for SSH machines,
  what does not work with them).
- A real Mac: a lease with a lease user (create, an Xcode build or a shell command,
  delete). Its sudo rule needs the owner, because the test Mac's machine user is
  not an administrator.
- The whole E2E suite in one local run and in the 10-run gate.

Done when: the E2E scenarios of section 6 pass on Linux machines, the audit
scenarios of section 4 pass, the macOS lease user is shown on a real Mac, and the
E2E suite passes.

## 9. Risks

- A tool that needs the machine user's account but has no `machine_user` key fails
  in a lease user. Mitigation: the key on the known tools; the guide tells admins to
  test their own tools.
- `sysadminctl` on macOS can be slow or ask for a secure token. Mitigation: try it on
  the real Mac early (phase 4); use `dscl` if necessary.
- Cold mise installs for repository tools in each lease user. Mitigation: shared
  installs for the machine's tools; the guide tells admins to add the tools that
  repositories need to the machine.

## 10. Not in this milestone

- The egress firewall (owner decision, 2026-11-13).
- Windows (lease users and the audit).
- Root escalation on a machine (design, section 18).
