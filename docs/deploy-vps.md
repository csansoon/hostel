# Deploy the server on a VPS

This guide puts the Hostel server on a Linux VPS (milestone M5). The reference
deployment is an exe.dev VM; section 8 shows the changes for another VPS. Machines
behind NAT (for example a Mac at home) connect to the server with `hostel machine connect` (before M9: `hostel helper connect`)
(section 6).

## 1. What you need

- A Linux VM with systemd, amd64 or arm64. 2 CPUs and 4 GB are enough.
- One public HTTPS address for the server. The API and the gateway share it: the
  gateway also accepts SSH inside a WebSocket on `/v1/gateway`.
- For Namespace machines: the `nsc` and `devbox` CLIs, logged in as the server user
  (section 5).
- A GitHub release of Hostel (`install.sh`).

## 2. Create the VM (exe.dev)

On your own machine:

```sh
ssh exe.dev new --name=hostel --no-email
```

The default image (`exeuntu`) runs systemd and has the user `exedev` with `sudo`.
The VM has the address `https://hostel.example.com`. exe.dev terminates TLS and sends
HTTPS requests to one port of the VM (8000 by default). It does not let raw TCP
in; that is why the gateway uses the WebSocket.

Make the HTTPS address public. Without this, exe.dev sends every request to its own
login page:

```sh
ssh exe.dev share set-public hostel
```

The tokens are the only access control of the API, as on any server.

## 3. Install Hostel

On the VM (`ssh hostel.example.com`):

```sh
curl -fsSL -o install.sh https://raw.githubusercontent.com/csansoon/hostel/main/install.sh
sudo sh install.sh --dir /opt/hostel/bin --server /opt/hostel
sudo chown -R exedev /opt/hostel
sudo ln -sf /opt/hostel/bin/hostel /usr/local/bin/hostel
```

`/opt/hostel/bin/hostel` is the server binary, `/opt/hostel/helpers` gets the
binaries for all machines (the default tools are in the binary). The
service's user (`exedev`) owns `/opt/hostel`, so that an admin can update the
server from the app (Front desk › This server › Update the server) or with `hostel server
update --remote`; the server then starts again by itself. On the VM,
`hostel server update` and a restart of the service (section 4) do the same.

A `server.toml` from before M10e can have `platforms_dir`: the server ignores
it, logs a warning, and starts. Delete the key.

## 4. Configure and start the server

As the server user (`exedev`):

```sh
hostel server init --tls off --listen 0.0.0.0:8000 --url https://hostel.example.com
```

It asks for the server name and your name, and creates the data directory
(`~/.local/share/hostel`: the database and the keys). It prints the first admin
invite code (`hostel connect hostel1-...`): run that command on your own computer
within 24 hours (section 4.1). On a VPS behind a proxy with TLS (exe.dev, Caddy),
use `--tls off`; on a machine without a proxy or a domain (an IP address or a
Tailscale name), leave the default `self-signed`: the server then serves HTTPS
with its own certificate, and clients pin its key.

`server.toml` keeps only the keys of the server machine:

```toml
server_id = "exe"
listen = "0.0.0.0:8000"
tls = "off"
public_urls = ["https://hostel.example.com"]
gateway_listen = "127.0.0.1:7422"
gateway_public = "wss://hostel.example.com/v1/gateway"
helper_dir = "/opt/hostel/helpers"
```

- `listen` is the port that the HTTPS proxy sends to.
- `gateway_listen` stays on `127.0.0.1`: guests and machines use the WebSocket in
  `gateway_public`.
- `helper_dir` has the binaries for the machines (`hostel-*`, `mise-*`); the server
  also serves them to machines that join (section 6).

Since M9, the providers, machines, and settings are in the server's
database: an admin changes them from their own computer (section 4.1) with
`hostel config edit`, `hostel config machines`, `hostel config providers`,
`hostel config settings`, and `hostel config tools` (since M10e; the default
tools come with the binary). A server from before
M9 imports them from `server.toml` at its first start (a copy stays in
`server.toml.pre-m9`). After that import, each machine provisions once more.

Check the file with `hostel admin config check` (it prints each problem).

Add the systemd unit `/etc/systemd/system/hostel.service`:

```ini
[Unit]
Description=Hostel server
After=network-online.target
Wants=network-online.target

[Service]
User=exedev
ExecStart=/opt/hostel/bin/hostel server run
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now hostel
journalctl -u hostel -f
```

### 4.1 Connect your computer and invite others

On your own computer, run the command that `hostel server init` printed:

```sh
hostel connect hostel1-...
hostel status
```

Invite another person (they install `hostel` and run the printed command):

```sh
hostel config users invite                  # a member
hostel config users invite --role admin     # an admin
hostel config users                         # users and their computers
```

`hostel config users code` makes a code for your own other computer. If nobody
can sign in as admin, run `hostel server invite` on the server machine. For a
lost computer, or one that could not disconnect, run
`hostel config users remove "<name>" --computer <computer>`: only that
computer is disconnected.

A server from before M8 keeps working: its tokens became users (one user for
each token name and role; an admin token's user is admin). A guest token never
gets admin rights: when an admin token has the same name, the guest tokens'
user is "<name> (member)" (or "<name> (member 2)", and so on, when that name
is taken). Add `public_urls` to `server.toml` before you invite
people.

## 5. Namespace login (optional)

Only for `namespace` providers. Install the Namespace CLIs as the server user on
the VM:

```sh
curl -fsSL https://get.namespace.so/cloud/install.sh | sh        # nsc (instances)
curl -fsSL get.namespace.so/devbox/install.sh | bash             # devbox (macOS devboxes)
```

Both go to `~/.local/bin`. The server service must find them there:

```sh
sudo mkdir -p /etc/systemd/system/hostel.service.d
printf '[Service]\nEnvironment=PATH=%s/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' "$HOME" \
  | sudo tee /etc/systemd/system/hostel.service.d/path.conf
sudo systemctl daemon-reload && sudo systemctl restart hostel
```

Then log the server in from your own computer:

```sh
hostel config providers login namespace
hostel config providers add ns --kind namespace
```

The server runs `devbox login --browser=false` and `nsc login --browser=false`;
the client prints each URL. Open it in a browser, and select the workspace. The
command ends when the login is complete; the credentials stay on the server.
`--cli nsc` or `--cli devbox` logs in one CLI only. The server refuses a
Namespace provider without a valid login for each product that its machines
use. A login lasts days (the owner's `nsc` login of 2026-10-20 was still valid
on 2026-10-29); when it ends, the provider calls fail, and you must log in
again.

## 6. A static machine behind NAT

The machine (for example a Mac at home) keeps an outbound connection to the
gateway. It needs no inbound SSH from the internet.

**With a code (M9).** On your own computer:

```sh
hostel config machines add studio-mac --tool xcode --tool ios-simulator --tool macos-desktop
```

It prints one command for macOS and Linux and one for Windows. Run it on the
new machine as an administrator. `hostel machine join` creates the user
`hostel`, installs the server's key for it, adds the machine to the
configuration (`connect = true`), and starts the service that runs `hostel
machine connect` (a systemd unit as `hostel-connect`, a LaunchDaemon as root,
or a scheduled task as `LOCAL SERVICE`; the token is in `/etc/hostel` or
`C:\ProgramData\hostel`, which the machine user cannot read). Then do the steps
that it prints (Remote Login, Xcode, the desktop: `docs/static-machine-setup.md`).
If a step fails, fix it, and run the same command again: it continues.

**By hand** (the steps that `machine join` does). Prepare the machine first with
`docs/static-machine-setup.md`. Then:

1. Put the server's public key (`hostel admin pubkey`) into the machine user's
   `~/.ssh/authorized_keys` and also into `~/hostel/authorized_keys.base` if that
   file exists: cleanup restores `authorized_keys` from it.

2. Create a machine token for the machine, on a computer connected as an admin:

   ```sh
   hostel admin token create --name macbook --role machine --machine home/macbook
   ```

3. Choose the account that runs `machine connect`. It must not be the machine user or a
   room user: a lease runs as that user, and with the token a guest could
   replace the machine's connection. Use your own account on macOS, a system user
   (for example `hostel-connect`) on Linux, and `LOCAL SERVICE` on Windows.
   Install Hostel for that account, and write the token to a file that only that
   account can read:

   ```sh
   sh install.sh
   mkdir -p ~/.config/hostel
   (umask 077; cat > ~/.config/hostel/machine-token)   # paste the token, then Ctrl-D
   ```

   Never put the token in the command line: other users can read it.

4. Run `hostel helper connect` as a service.

   macOS (a launch daemon that runs as your account, so it starts at boot,
   `/Library/LaunchDaemons/so.hostel.machine-connect.plist`; replace `owner`):

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>so.hostel.machine-connect</string>
     <key>UserName</key><string>owner</string>
     <key>ProgramArguments</key>
     <array>
       <string>/Users/owner/.local/bin/hostel</string>
       <string>machine</string>
       <string>connect</string>
       <string>--server</string>
       <string>https://hostel.example.com</string>
       <string>--token-file</string>
       <string>/Users/owner/.config/hostel/machine-token</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>StandardErrorPath</key><string>/Users/owner/Library/Logs/hostel-machine-connect.log</string>
     <key>StandardOutPath</key><string>/Users/owner/Library/Logs/hostel-machine-connect.log</string>
   </dict>
   </plist>
   ```

   ```sh
   sudo launchctl bootstrap system /Library/LaunchDaemons/so.hostel.machine-connect.plist
   ```

   Linux (`/etc/systemd/system/hostel-machine-connect.service`):

   ```ini
   [Unit]
   Description=Hostel machine connect
   After=network-online.target
   Wants=network-online.target

   [Service]
   User=hostel-connect
   ExecStart=/home/hostel-connect/.local/bin/hostel helper connect --server https://hostel.example.com --token-file /home/hostel-connect/.config/hostel/machine-token
   Restart=always
   RestartSec=5

   [Install]
   WantedBy=multi-user.target
   ```

   Windows (as an administrator, in PowerShell; a scheduled task that runs as
   `LOCAL SERVICE` at boot; the token is in `C:\ProgramData\hostel\machine-token`,
   which only that account and administrators can read):

   ```powershell
   $dir = 'C:\ProgramData\hostel'
   New-Item -ItemType Directory -Force $dir | Out-Null
   # Copy hostel-windows-amd64.exe to $dir\hostel.exe, and the token to $dir\machine-token.
   icacls $dir /inheritance:r /grant 'SYSTEM:(OI)(CI)F' /grant 'Administrators:(OI)(CI)F' /grant '*S-1-5-19:(OI)(CI)M'
   $tr = 'cmd.exe /c ""' + $dir + '\hostel.exe" machine connect --server https://hostel.example.com --token-file ' + $dir + '\machine-token >> ' + $dir + '\machine-connect.log 2>&1"'
   schtasks.exe /Create /F /TN 'Hostel machine connect' /SC ONSTART /RU 'NT AUTHORITY\LOCALSERVICE' /RL LIMITED /TR $tr
   schtasks.exe /Run /TN 'Hostel machine connect'
   ```

   `Register-ScheduledTask` for another user failed with "Access is denied" in
   an SSH session, and a task as the machine user needs its password; `LOCAL
   SERVICE` needs neither.

5. Check: the log shows `connected to the gateway at wss://...`, and
   `hostel status` shows the machine `running` within a few seconds. The server
   probes a machine at once when it connects.

`--ssh` (default `127.0.0.1:22`) is the machine's own SSH server. If the connection
ends, the machine connects again (1 s to 30 s); guest commands fail until then, and
the lease stays.

## 7. Network rule for public static machines

The gateway enforces access only if a machine accepts SSH from nothing else
(`design.md`, section 13). For a static machine with a public address, do one of
these:

- Use `connect = true`, and close inbound SSH on the machine (firewall or Remote
  Login off for other networks).
- Allow inbound SSH only from the server's address. On exe.dev, the VM has no
  fixed outbound address that you control; use `connect = true` there.

## 8. Another VPS

Hostel has no TLS code. Put a TLS proxy in front of the server, for example
Caddy:

```
hostel.example.com {
	reverse_proxy 127.0.0.1:8000
}
```

Then `listen = "127.0.0.1:8000"` and
`gateway_public = "wss://hostel.example.com/v1/gateway"`. Caddy passes WebSocket
upgrades without more settings. If the VPS lets raw TCP in, you can instead open
the gateway port (`gateway_listen = "0.0.0.0:7422"`,
`gateway_public = "hostel.example.com:7422"`).

## 9. Backups

The state is in the data directory: `hostel.db` (SQLite in WAL mode) and the
keys (`server_ed25519`, `gateway_host_ed25519`). Back up the keys once. Back up the
database with SQLite, not with a file copy:

```sh
sqlite3 ~/.local/share/hostel/hostel.db ".backup /var/backups/hostel.db"
```

If you lose the database, machines stay safe: on-demand machines with this server's
prefix are destroyed at the next reconcile of a new server with the same
`server_id`, and leases end. Tokens must be created again.

If you lose `server_ed25519`, put the new public key on each static machine.
If you lose `gateway_host_ed25519`, guests see a new gateway host key; they get it
again from the server with each lease.
