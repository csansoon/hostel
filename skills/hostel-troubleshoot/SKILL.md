---
name: hostel-troubleshoot
description: Find and fix Hostel problems - machines that are Broken, Offline, Getting ready, or take no new leases; leases that wait or fail; acquire exit codes 69, 75, 78; TLS or host key changes; Namespace limits; and where the logs are. Use it when the user says "the machine is broken", "the Mac is offline", "acquire hangs", "not connected", "no machine can serve", "TLS key changed", "host key changed", or "where are the Hostel logs".
---

# Troubleshoot Hostel

Start with the facts:

```sh
hostel status                  # all machines, leases, and the waiting line
hostel status mac-mini         # one machine: hardware, inventory, leases, jobs, waiting leases, events, machine ID
hostel status ns/linux         # one template: size, tools, machines now
hostel status ios-simulator    # the waiting leases for one tool, with the reason of each lease
hostel config machines            # machines and templates, with their running machines and states
```

People can also open `hostel` (the app): Rooms shows each machine's state and
details.

## Machine states

| App word | `hostel status` | Meaning | Do |
|---|---|---|---|
| Vacant | `free` | Ready for a new lease. | Nothing. |
| Occupied | `in use` | A lease uses it. | Wait, or an admin ends the lease (`hostel release <lease-id>`). |
| Getting ready | `creating`, `provisioning` | The server creates the machine, uploads the helper, installs tools, or runs the `install` and `check` scripts. A first Android install takes up to 30 minutes. | Wait. Read the events with `hostel status <machine>`. |
| No new leases | `..., drained` | An admin drained it. Current leases continue. | `hostel config machines undrain <machine>`. |
| Broken | `broken` | A script, cleanup, or provisioning failed, or the host key changed. The reason is shown. | Fix the cause, then repair (below). |
| Offline | `offline (last seen ...)` | A static machine did not answer the presence check. | Make the machine reachable. It comes back by itself. |
| (on-demand) | `stopped`, `resuming`, `stopping`, `destroying` | A warm on-demand machine is stopped, starts again, stops, or goes away (`warm_running`, `warm_stopped`). | Nothing: the server starts or creates a machine when a lease needs one. |
| Not connected | no machine | A machine with `connect = true` has never connected. | Check its connect service (below). |
| Not ready | no machine | The server has not prepared a static machine yet. | Wait; check `hostel status`. |

Broken on-demand machines are destroyed, and a new one starts when a lease needs
it. Broken static machines stay broken until an admin repairs them.

## Repair a broken machine

Fix the cause first: the reason and the last 4 KB of script output are in the
events (`hostel status <machine>`). Then:

```sh
hostel config machines repair mac-mini
```

Repair runs cleanup again for each broken or dirty room, restores
`authorized_keys`, runs the `check` scripts, and sets the machine to running if
all pass.

**Host key changed.** The server pins each machine's SSH host key. A new key (a
reinstalled machine, or another machine at the same address) makes the machine
Broken with the reason `host key changed`. When you know why the key changed:

```sh
hostel config machines repair mac-mini --accept-host-key
```

In the app: Rooms, select the machine, `a`, Repair (it asks before it accepts a new key).

## Offline machines

The server tries an SSH login to each idle static machine and each offline machine
every `intervals.presence` (30 s). When an offline machine answers, the server
provisions it again and it becomes Vacant. Check:

- The machine is on, awake, and on the network (`pmset` sleep settings on a Mac).
- SSH runs and accepts the server key for `hostel`.
- For a machine that joins the server (`connect = true`): its connect service
  runs and its log says that it connected to the gateway.

| OS | Connect service log |
|---|---|
| Linux | `journalctl -u hostel-machine-connect` |
| macOS | `/var/log/hostel-machine-connect.log` (LaunchDaemon `so.hostel.machine-connect`) |
| Windows | `C:\ProgramData\hostel\machine-connect.log` (task `Hostel machine connect`) |

## Lease problems

| Symptom | Cause | Do |
|---|---|---|
| `acquire` exits 75 | Not finished: waiting or preparing. | Run the same command again. |
| Exit 69, `no machine has ...` | No machine has the tool, the option, all requested tools together, the tunnel ports, or (with `--fresh`) is on demand. The message names the machines that have each tool. | `hostel tools`; check the tools on the command line and `tools` in `hostel.toml`. An admin adds the tool to a machine (`hostel-machines`, `hostel-tools`). |
| Exit 78, `this computer is not connected to a Hostel server` | No connection. | The user runs `hostel connect <invite code>` (`hostel-install`). Agents must not do this. |
| The lease waits for a long time | All candidate machines are in use, broken, offline, or drained, or a provider limit is full. | `hostel status <tool>` shows the position and reason. |
| `acquire` exits 1 after the lease is ready | The setup job (`mise install` or `setup`) failed. The lease stays. | Fix the code; `hostel sync --setup`. |
| A tunnel fails with `no local port` or `nothing listens on local port` (`hostel tunnel list`, `hostel info`) | The machine side works, but the guest side does not: the variable is unset, or the local server is not running. | Set the port in `.env`, or start the server. No restart of the lease is necessary. |
| `job <id> is still running` (exit 75) | The command is longer than `--wait`. | `hostel attach <id>`. Never start the job again. |
| The lease ended | Release, `idle_timeout` (10 min without a `hostel` command or tunnel data), `max_lease` (2 h), an admin, or a machine failure. | The guest prints the end reason. Acquire again. |
| A lease fails after 3 attempts | Provisioning or a tool's `start` failed 3 times. | Read the events of the machine; fix the tool or machine. |

## Connection and TLS errors

- **HTTP 426, `this server needs vX or later`**: this computer's Hostel is too
  old for the server. Run `hostel update` (`hostel-install`).
- **`the signature of release … is not valid`** or **`the checksum of … is not
  the one of release …`**: do not install it; report it to the Hostel
  maintainers. A release before v0.2.0 has no signature: install it with the
  install command.
- **`this user cannot write …`** (an update): run `sudo hostel update`; for the
  server, give the service's user `/opt/hostel` (`hostel-server-setup`,
  section 9).

- **`the server at ... has another TLS key than the one that this computer
  pinned`**: the server was set up again (a new TLS key), or the address points
  to another server. Ask an admin. To trust the new server, connect again with a
  new invite code (`hostel connect <code> --yes`).
- **A refused token** (removed user or computer): connect again with a new
  invite code. The app goes to Check in by itself.
- **`the machine code is not valid`**: the code was used, expired (24 h), or is
  wrong. Make a new one: `hostel config machines add <name>`.
- **`only an admin can change the configuration`**: ask an admin, or an admin
  changes your role (`hostel-users`).
- **A configuration change is refused**: another revision came first (run
  `hostel config edit` again), or the error names the invalid key path. The
  keys before M10e (`platforms`, `labels`, `requires`, a `tools` map) are
  errors: machines list tools (`hostel-machines`).
- **The server runs without providers and machines** after an update: the
  stored configuration has the form before M10e. `hostel status` shows the
  warning. Write it again with tools: `hostel config edit`.

## Namespace

- **Limit reached** (for example `per-user devbox limit reached (1)`): the
  account's plan limit is full, also with devboxes that Hostel did not create.
  The template is marked full for `limit_retry` (2 min); the lease waits. Remove
  other devboxes, lower `max_machines`, or raise the plan.
- At the plan's concurrency limit, `nsc create` blocks; Hostel stops after 2
  minutes and treats it as "limit reached".
- **Login expired**: provider calls fail. Run
  `hostel config providers login namespace` again.
- **`devbox` or `nsc` not found**: the server service's `PATH` does not have
  `~/.local/bin` (`hostel-machines`, section "Namespace").

## Logs

- Server: `journalctl -u hostel -f` (JSON lines). An invalid configuration stops
  the start with a message.
- A machine's events (state changes, script output tails, errors):
  `hostel status <machine>`.
- Check the files: `hostel admin config check` (on the server machine) and
  `hostel admin config check --repo hostel.toml` (in a repository).
- Hostel never logs tokens, keys, environment values, or the output of guest
  jobs. Use `hostel attach <job>` for job output.
