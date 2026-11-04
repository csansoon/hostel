---
name: hostel-rooms
description: Add, change, or remove the machines that give Hostel its rooms - a static host over SSH, a machine behind NAT that joins the server, Namespace on-demand machines, or your own external provider. Use it when the user says "add a Mac", "add a Windows machine", "add my Linux box", "the machine is behind NAT", "use Namespace", "more rooms", "drain/remove/repair a machine", or "destroy a room".
---

# Add and manage rooms

Machines are in the configuration document on the server. You must be an
admin (`hostel-users`). Each change applies at once.

```sh
hostel config providers        # NAME, KIND, MACHINES
hostel config rooms            # MACHINE, KIND, OS, PLATFORMS, LABELS, ROOMS
hostel config edit             # change the TOML in $EDITOR
```

A machine is named `<provider>/<machine>`, or only `<machine>` when the name is
unique. Names match `^[a-z][a-z0-9-]*$`. Platform requirements (Xcode, Android
SDK, KVM, desktop) are in `hostel-platforms`.

## A machine behind NAT (it joins the server)

Use this for most own machines: a Mac mini at home, a Windows PC, a Linux box.
The machine keeps an outbound connection to the server. It needs no inbound
SSH from the internet.

1. On your computer, make a machine code (24 hours, works once):

   ```sh
   hostel config rooms add mac-mini --platform ios --label xcode-26.5 --label desktop
   hostel config rooms add win-pc --label desktop
   ```

   `--platform` and `--label` can repeat. `--provider` selects the static
   provider (default `home`; the server adds it if it is missing). In the app:
   Front desk › Add a room.

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

The token is in `/etc/hostel` or `C:\ProgramData\hostel`; the room user cannot
read it. If a step fails, fix it and run the same command again: it continues.
`--no-service` skips the service and prints the `hostel machine connect`
command to run yourself (`--server <url> --token-file <file> [--server-key k]
[--ssh 127.0.0.1:22]`).

3. Do the steps that `join` prints and that only a person can do:
   - macOS: System Settings › General › Sharing › Remote Login on, for `hostel`;
     Xcode for iOS; for `desktop`, automatic login of `hostel` and no password
     after the screen saver.
   - Windows: OpenSSH Server runs and starts automatically
     (`Get-Service sshd`); for `desktop`, the "Hostel desktop" task.
   - Linux: the SSH server runs on port 22 and allows key login for `hostel`.

4. Check: `hostel status` shows the room within a minute.

**Update** the connection binary of a machine that joined: the app shows "Out
of date" on the machine when its connect service is older than the server
(`hostel status` JSON: `connect_version`). On the machine, as an administrator:

```sh
sudo /etc/hostel/hostel machine update                 # macOS, Linux
C:\ProgramData\hostel\hostel.exe machine update         # Windows, administrator PowerShell
```

It downloads the server's binary for the machine, replaces the service's copy,
and restarts the service. The room helper needs nothing: the server sends it
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
platforms = ["ios", "android"]       # device platforms; the OS platform is automatic
labels = ["xcode-26.5", "desktop"]
tools = { java = "21" }              # machine-level mise tools
env = { HOSTEL_IOS_RUNTIME = "26.5" }
# slots = [{ name = "a" }, { name = "b" }]   # two leases at a time (default: one slot "a")
```

- `slots`: each slot `n` gets the ports `7500 + 10n` to `7509 + 10n` for
  tunnels. A slot can set `ports`, `env`, and its own OS `user`.
- `env` and `tools` on the provider apply to all its machines; machine keys
  replace keys with the same name.
- The server pins the machine's SSH host key at the first connection.
- Windows by hand: `docs/static-room-setup.md`, section "Windows" (the user,
  and the access rules of `authorized_keys` that OpenSSH requires).
- For a public address: allow inbound SSH only from the server, or use
  `connect = true` (the join flow) and close inbound SSH.

Change one key without the editor (`platforms` and `labels` are comma lists):

```sh
hostel config rooms set mac-mini labels=xcode-26.5,desktop
hostel config rooms set mac-mini platforms=ios
```

## Namespace (on-demand cloud machines)

Namespace (namespace.so) creates rooms when a lease needs them: a **devbox**
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

Then add machine templates with `hostel config edit`:

```toml
[providers.ns]
kind = "namespace"
max_rooms = 3                        # all machines of this provider together
# limits = [{ machines = ["mac"], max_rooms = 1 }]

[providers.ns.machines.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
platforms = ["ios"]
labels = ["xcode-26.5", "desktop"]
env = { DEVELOPER_DIR = "/Applications/Xcode_26.5.app/Contents/Developer", HOSTEL_IOS_RUNTIME = "26.5" }
max_rooms = 1
max_lifetime = "3h"                  # required for a devbox: the plan's maximum duration

[providers.ns.machines.linux]
spec = { product = "instance", machine_type = "linux/amd64:8x16" }   # optional: image (default ubuntu:24.04)
platforms = ["android"]
labels = ["kvm"]
tools = { java = "21" }
max_rooms = 2
```

Cost and limits (check the current plan at Namespace):

- Hostel acts only on rooms with its name prefix `hostel-<server_id>-`.
- A devbox is ephemeral: it is deleted after about 15 minutes idle, and at its
  `max_lifetime`. An instance has a fixed deadline that the server extends.
- A macOS devbox costs much more per minute than a Linux one.
- Plan limits apply to all leases together (they share one login). On the
  Developer plan: one devbox per user, a 3 h maximum, no Windows. A "limit
  reached" error marks the machine full for `limit_retry` (2 minutes).
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

[providers.docker.machines.small]
os = "linux"                         # required
spec = { image = "debian:13" }       # passed to your command as is
max_rooms = 2
```

## Limits and warm rooms (on-demand machines)

| Key | Where | Default |
|---|---|---|
| `max_rooms` | provider (all its machines) / machine | none / 1 |
| `limits` | provider: `[{ machines = [...], max_rooms = N }]` | `[]` |
| `max_lease` | provider, machine | `2h` |
| `max_lifetime` | provider, machine | none |
| `warm_running` | provider, machine: keep a free room running this long | `5m` |
| `warm_stopped` | provider, machine: keep a stopped room this long | `24h` |

## Drain, repair, remove, destroy

```sh
hostel config rooms drain mac-mini        # no new leases; current leases continue
hostel config rooms undrain mac-mini
hostel config rooms repair mac-mini       # check a broken room again
hostel config rooms repair mac-mini --accept-host-key
hostel config rooms destroy ns/mac        # an on-demand room without a lease (give the room name if several)
hostel config rooms remove mac-mini       # remove the machine; its room tokens are revoked
hostel config providers remove ns         # only without on-demand rooms
```

- A machine with an active lease cannot be removed: drain it and wait.
- An on-demand machine cannot be removed while it has rooms: destroy them first.
- `hostel admin room drain|undrain|repair|destroy <room name or ID>` does the
  same by room.

## Offline machines

Every `intervals.presence` (30 s) the server checks each idle static room. A
room that does not answer goes **Offline** and takes no lease. When it answers
again, the server prepares it again by itself. No command is necessary.
`hostel status` prints `offline (last seen ...)`. See `hostel-troubleshoot`.
