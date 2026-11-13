---
name: hostel-machines
description: Add, change, or remove the machines of Hostel - a static host over SSH, a machine behind NAT that joins the server, Namespace on-demand machines, or your own external provider. Use it when the user says "add a Mac", "add a Windows machine", "add my Linux box", "the machine is behind NAT", "use Namespace", "more machines", "drain/remove/repair a machine", or "destroy a machine".
---

# Add and manage machines

Machines are in the configuration document on the server. You must be an
admin (`hostel-users`). Each change applies at once.

```sh
hostel config providers        # NAME, KIND, MACHINES
hostel config machines            # MACHINE, KIND, OS, TOOLS, MACHINES
hostel config edit             # change the TOML in $EDITOR
```

A machine is named `<provider>/<machine>`, or only `<machine>` when the name is
unique; a template has the same form. Names match `^[a-z][a-z0-9-]*$`. Each machine
or template lists the tools that it gives (`tools`). What each tool needs on the machine (Xcode, KVM, a desktop
session) is in `hostel-tools`.

## A machine behind NAT (it joins the server)

Use this for most own machines: a Mac mini at home, a Windows PC, a Linux box.
The machine keeps an outbound connection to the server. It needs no inbound
SSH from the internet.

1. On your computer, make a machine code (24 hours, works once):

   ```sh
   hostel config machines add mac-mini --tool xcode --tool ios-simulator --tool macos-desktop
   hostel config machines add win-pc --tool windows-desktop --tool dotnet:9
   ```

   `--tool` (`name` or `name:option`) can repeat. `--provider` selects the static
   provider (default `home`; the server adds it if it is missing). In the app:
   Rooms, then `+`.

2. Run the printed command on the new machine as an administrator: `sudo` on
   macOS and Linux, an administrator PowerShell on Windows. The command
   downloads `hostel` from your server and runs `hostel machine join <code>`.

`hostel machine join` creates the user `hostel` (no administrator rights),
installs the server's key for it, adds the machine to the configuration
(`connect = true`, OS from the machine), and installs the service that runs
`hostel machine connect`:

| OS | Service | Log |
|---|---|---|
| Linux | systemd unit `hostel-machine-connect.service` (user `hostel-connect`) | `journalctl -u hostel-machine-connect` |
| macOS | LaunchDaemon `so.hostel.machine-connect` | `/var/log/hostel-machine-connect.log` |
| Windows | scheduled task `Hostel machine connect` (`LOCAL SERVICE`) | `C:\ProgramData\hostel\machine-connect.log` |

The token is in `/etc/hostel` or `C:\ProgramData\hostel`; the machine user cannot
read it. If a step fails, fix it and run the same command again: it continues.
`--no-service` skips the service and prints the `hostel machine connect`
command to run yourself (`--server <url> --token-file <file> [--server-key k]
[--ssh 127.0.0.1:22]`).

3. Do the steps that `join` prints and that only a person can do:
   - macOS: System Settings › General › Sharing › Remote Login on, for `hostel`;
     Xcode for `xcode` and `ios-simulator`; for `macos-desktop`, automatic login of `hostel` and no password
     after the screen saver.
   - Windows: OpenSSH Server runs and starts automatically
     (`Get-Service sshd`); for `windows-desktop`, the "Hostel desktop" task.
   - Linux: the SSH server runs on port 22 and allows key login for `hostel`.

4. Check: `hostel status` shows the machine within a minute.

**Update** the connection binary of a machine that joined: the app shows "Out
of date" on the machine when its connect service is older than the server
(`hostel status` JSON: `connect_version`). On the machine, as an administrator:

```sh
sudo /etc/hostel/hostel machine update                 # macOS, Linux
C:\ProgramData\hostel\hostel.exe machine update         # Windows, administrator PowerShell
```

It downloads the server's binary for the machine, replaces the service's copy,
and restarts the service. The machine helper needs nothing: the server sends it
before each lease.

## A static machine that the server reaches over SSH

Use this when the server can open TCP to the machine (same LAN or VPN). On the
machine, create a standard user `hostel` with the server's SSH key. The key is
the output of `hostel admin pubkey` (on an admin's computer), and `hostel server
init` printed it too ("server public key"). On Linux:

```sh
sudo useradd -m -s /bin/bash hostel
sudo install -d -m 700 -o hostel -g hostel ~hostel/.ssh
echo 'ssh-ed25519 AAAA... hostel-server' | sudo tee ~hostel/.ssh/authorized_keys
sudo chown hostel:hostel ~hostel/.ssh/authorized_keys && sudo chmod 600 ~hostel/.ssh/authorized_keys
```

Allow SSH key login for that user. Keep TCP
forwarding on in `sshd` (the OpenSSH default; tunnels need it). Then add it
with `hostel config edit`:

```toml
[providers.home]
kind = "static"

[providers.home.machines.mac-mini]
address = "hostel@mac-mini.local"    # user@host[:port]; default user hostel, port 22
os = "macos"                         # required: linux, macos, or windows
tools = ["xcode", "ios-simulator", "android-emulator", "jdk:21", "macos-desktop"]   # android-sdk comes with android-emulator
env = { HOSTEL_IOS_RUNTIME = "26.5" }
# rooms = [{ name = "a" }, { name = "b" }]   # two leases at a time (default: one room "a")
```

- `rooms`: each room `n` gets the ports `7500 + 10n` to `7509 + 10n` for
  tunnels. A room can set `ports`, `env`, and its own OS `user`.
- `tools`: defined tools (`hostel-tools`) and mise tools with their version
  (`jdk:21`, `node:22`). The tools that a tool needs are added.
- `env` and `tools` on the provider apply to all its machines and templates. A
  machine's `env` keys replace keys with the same name; a machine's `tools` list replaces
  the provider's list.
- The server pins the machine's SSH host key at the first connection.
- Windows by hand: `docs/machine-setup.md`, section 8 (the user,
  and the access rules of `authorized_keys` that OpenSSH requires).
- For a public address: allow inbound SSH only from the server, or use
  `connect = true` (the join flow) and close inbound SSH.

Change one key without the editor (`tools` is a comma list):

```sh
hostel config machines set mac-mini tools=xcode,ios-simulator,jdk:21
```

## Namespace (on-demand cloud machines)

Namespace (namespace.so) creates machines when a lease needs them: a **devbox**
for macOS (or Linux), an **instance** for Linux. Install both CLIs as the
server's user, and put `~/.local/bin` on the service's `PATH`:

```sh
curl -fsSL https://get.namespace.so/cloud/install.sh | sh        # nsc
curl -fsSL get.namespace.so/devbox/install.sh | bash             # devbox
sudo mkdir -p /etc/systemd/system/hostel.service.d
printf '[Service]\nEnvironment=PATH=%s/.local/bin:/usr/local/bin:/usr/bin:/bin\n' "$HOME" \
  | sudo tee /etc/systemd/system/hostel.service.d/path.conf
sudo systemctl daemon-reload && sudo systemctl restart hostel
```

From your computer (it prints a URL; open it and select the workspace):

```sh
hostel config providers login namespace        # --cli devbox or --cli nsc for one CLI
hostel config providers add ns --kind namespace
```

Then add templates with `hostel config edit`:

```toml
[providers.ns]
kind = "namespace"
max_machines = 3                        # all machines of this provider together
# limits = [{ templates = ["mac"], max_machines = 1 }]

[providers.ns.templates.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
tools = ["xcode", "ios-simulator", "macos-desktop"]
# tahoe-slim has several Xcode versions; select one (the xcode check then reports 26.5).
env = { DEVELOPER_DIR = "/Applications/Xcode_26.5.app/Contents/Developer", HOSTEL_IOS_RUNTIME = "26.5" }
max_machines = 1
max_lifetime = "3h"                  # required for a devbox: the plan's maximum duration

[providers.ns.templates.linux]
spec = { product = "instance", machine_type = "linux/amd64:8x16" }   # optional: image (default ubuntu:24.04)
tools = ["android-sdk", "android-emulator", "jdk:21"]
max_machines = 2
```

Cost and limits (check the current plan at Namespace):

- Hostel acts only on machines with its name prefix `hostel-<server_id>-`.
- A devbox is ephemeral: it is deleted after about 15 minutes idle, and at its
  `max_lifetime`. An instance has a fixed deadline that the server extends.
- A macOS devbox costs much more per minute than a Linux one.
- Plan limits apply to all leases together (they share one login). On the
  Developer plan: one devbox per user, a 3 h maximum, no Windows. A "limit
  reached" error marks the template full for `limit_retry` (2 minutes).
- Only `linux` instance types are supported. Android on Namespace uses Linux.
- A login lasts days. When it ends, provider calls fail: log in again.

## The external provider (your own command)

`kind = "external"` runs your command for each operation (`capabilities`,
`provision`, `resume`, `stop`, `extend`, `destroy`, `list`, `access`) with JSON
on stdin and stdout. `docs/design.md`, section 7, "External provider
contract", has the full contract. The repository's `test/fakeprovider` (Docker
containers) is a test tool, not a supported provider.

```toml
[providers.docker]
kind = "external"
command = ["/usr/local/bin/my-provider"]
command_env = { DOCKER_HOST = "unix:///var/run/docker.sock" }
timeouts = { provision = "5m", default = "1m" }

[providers.docker.templates.small]
os = "linux"                         # required
spec = { image = "debian:13" }       # passed to your command as is
max_machines = 2
```

## Limits and warm machines (on-demand machines)

| Key | Where | Default |
|---|---|---|
| `max_machines` | provider (all its machines) / template | none / 1 |
| `limits` | provider: `[{ templates = [...], max_machines = N }]` | `[]` |
| `max_lease` | provider, machine, template | `2h` |
| `max_lifetime` | provider, machine, template | none |
| `warm_running` | provider, template: keep a free machine running this long | `5m` |
| `warm_stopped` | provider, template: keep a stopped machine this long | `24h` |
| `reuse` | provider, template: `false` destroys an on-demand machine after its lease (no backdoor survives; each lease waits for a new machine) | `true` |

## Drain, repair, remove, destroy

```sh
hostel config machines drain mac-mini        # no new leases; current leases continue
hostel config machines undrain mac-mini
hostel config machines repair mac-mini       # check a broken machine again
hostel config machines repair mac-mini --accept-host-key
hostel config machines destroy ns/mac        # the machine of a template, without a lease (give the machine name if several)
hostel config machines remove mac-mini       # remove the machine; its machine tokens are revoked
hostel config providers remove ns         # only without on-demand machines
```

- A machine with an active lease cannot be removed: drain it and wait.
- A template cannot be removed while it has machines: destroy them first.
- `hostel admin machine drain|undrain|repair|destroy <machine name or ID>` does the
  same by machine.

## Offline machines

Every `intervals.presence` (30 s) the server checks each idle static machine. A
machine that does not answer goes **Offline** and takes no lease. When it answers
again, the server prepares it again by itself. No command is necessary.
`hostel status` prints `offline (last seen ...)`. See `hostel-troubleshoot`.
