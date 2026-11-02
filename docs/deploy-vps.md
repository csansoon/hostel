# Deploy the server on a VPS

This guide puts the Hostel server on a Linux VPS (milestone M5). The reference
deployment is an exe.dev VM; section 8 shows the changes for another VPS. Rooms
behind NAT (the home MacBook) connect to the server with `hostel room connect`
(section 6).

## 1. What you need

- A Linux VM with systemd, amd64 or arm64. 2 CPUs and 4 GB are enough.
- One public HTTPS address for the server. The API and the gateway share it: the
  gateway also accepts SSH inside a WebSocket on `/v1/gateway`.
- For Namespace rooms: the `nsc` and `devbox` CLIs, logged in as the server user
  (section 5).
- A GitHub release of Hostel (`install.sh`). While the repository is private, the
  VM needs the GitHub CLI, logged in (`gh auth login`).

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
sudo sh install.sh --dir /usr/local/bin --server /opt/hostel
```

While the repository is private, get `install.sh` with
`gh api repos/csansoon/hostel/contents/install.sh -H 'Accept: application/vnd.github.raw' > install.sh`
and run it as a user that is logged in to `gh` (use `sudo -E` or install into
directories that the user can write).

`/opt/hostel/helpers` gets the binaries for all rooms, and `/opt/hostel/platforms`
the platform scripts. To update Hostel, run the same command again and restart the
service (section 4).

If you copy files from a Mac instead, make the archive with `COPYFILE_DISABLE=1`.
Without it, macOS adds `._*` files, and the server refuses the platform
directories.

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

Edit `~/.config/hostel/server.toml`:

```toml
server_id = "exe"
listen = "0.0.0.0:8000"
tls = "off"
public_urls = ["https://hostel.example.com"]
gateway_listen = "127.0.0.1:7422"
gateway_public = "wss://hostel.example.com/v1/gateway"
helper_dir = "/opt/hostel/helpers"
platforms_dir = "/opt/hostel/platforms"

[providers.home]
kind = "static"

# The home MacBook behind NAT: it runs "hostel room connect" (section 6).
[providers.home.machines.macbook]
address = "hostel@localhost"
os = "macos"
platforms = ["ios"]
labels = ["xcode-26.5", "desktop"]
connect = true
```

- `listen` is the port that the HTTPS proxy sends to.
- `gateway_listen` stays on `127.0.0.1`: guests and rooms use the WebSocket in
  `gateway_public`.
- For `connect = true`, only the user of `address` is used.

Check the file with `hostel admin config check` (it prints each problem).

Add the systemd unit `/etc/systemd/system/hostel.service`:

```ini
[Unit]
Description=Hostel server
After=network-online.target
Wants=network-online.target

[Service]
User=exedev
ExecStart=/usr/local/bin/hostel server run
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

Only for `namespace` providers. As the server user on the VM:

```sh
curl -fsSL https://get.namespace.so/cloud/install.sh | sh
nsc login --browser=false
devbox login --browser=false
```

Each `login` prints a URL. Open it in a browser on any machine, and select the
workspace. The command ends when the login is complete. `nsc auth check-login`
and `devbox auth check-login` exit with code 0 when the login is valid. The server
does not start without a valid login for each configured product. A login lasts
days (the owner's `nsc` login of 2026-10-20 was still valid on 2026-10-29); when
it ends, the provider calls fail, and you must log in again.

## 6. A static room behind NAT

The room (for example the home MacBook) keeps an outbound connection to the
gateway. It needs no inbound SSH from the internet. Prepare the room first with
`docs/static-room-setup.md`. Then:

1. Put the server's public key (`hostel admin pubkey`) into the room user's
   `~/.ssh/authorized_keys` and also into `~/hostel/authorized_keys.base` if that
   file exists: cleanup restores `authorized_keys` from it.

2. Create a room token for the machine, on a computer connected as an admin:

   ```sh
   hostel admin token create --name macbook --role room --machine home/macbook
   ```

3. Choose the account that runs `room connect`. It must not be the room user or a
   slot user: a lease runs as that user, and with the token a guest could
   replace the room's connection. Use your own account on macOS, a system user
   (for example `hostel-connect`) on Linux, and `LOCAL SERVICE` on Windows.
   Install Hostel for that account, and write the token to a file that only that
   account can read:

   ```sh
   sh install.sh
   mkdir -p ~/.config/hostel
   (umask 077; cat > ~/.config/hostel/room-token)   # paste the token, then Ctrl-D
   ```

   Never put the token in the command line: other users can read it.

4. Run `hostel room connect` as a service.

   macOS (a launch daemon that runs as your account, so it starts at boot,
   `/Library/LaunchDaemons/so.hostel.room-connect.plist`; replace `owner`):

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>so.hostel.room-connect</string>
     <key>UserName</key><string>owner</string>
     <key>ProgramArguments</key>
     <array>
       <string>/Users/owner/.local/bin/hostel</string>
       <string>room</string>
       <string>connect</string>
       <string>--server</string>
       <string>https://hostel.example.com</string>
       <string>--token-file</string>
       <string>/Users/owner/.config/hostel/room-token</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>StandardErrorPath</key><string>/Users/owner/Library/Logs/hostel-room-connect.log</string>
     <key>StandardOutPath</key><string>/Users/owner/Library/Logs/hostel-room-connect.log</string>
   </dict>
   </plist>
   ```

   ```sh
   sudo launchctl bootstrap system /Library/LaunchDaemons/so.hostel.room-connect.plist
   ```

   Linux (`/etc/systemd/system/hostel-room-connect.service`):

   ```ini
   [Unit]
   Description=Hostel room connect
   After=network-online.target
   Wants=network-online.target

   [Service]
   User=hostel-connect
   ExecStart=/home/hostel-connect/.local/bin/hostel room connect --server https://hostel.example.com --token-file /home/hostel-connect/.config/hostel/room-token
   Restart=always
   RestartSec=5

   [Install]
   WantedBy=multi-user.target
   ```

   Windows (as an administrator, in PowerShell; a scheduled task that runs as
   `LOCAL SERVICE` at boot; the token is in `C:\ProgramData\hostel\room-token`,
   which only that account and administrators can read):

   ```powershell
   $dir = 'C:\ProgramData\hostel'
   New-Item -ItemType Directory -Force $dir | Out-Null
   # Copy hostel-windows-amd64.exe to $dir\hostel.exe, and the token to $dir\room-token.
   icacls $dir /inheritance:r /grant 'SYSTEM:(OI)(CI)F' /grant 'Administrators:(OI)(CI)F' /grant '*S-1-5-19:(OI)(CI)M'
   $tr = 'cmd.exe /c ""' + $dir + '\hostel.exe" room connect --server https://hostel.example.com --token-file ' + $dir + '\room-token >> ' + $dir + '\room-connect.log 2>&1"'
   schtasks.exe /Create /F /TN 'Hostel room connect' /SC ONSTART /RU 'NT AUTHORITY\LOCALSERVICE' /RL LIMITED /TR $tr
   schtasks.exe /Run /TN 'Hostel room connect'
   ```

   `Register-ScheduledTask` for another user failed with "Access is denied" in
   an SSH session, and a task as the room user needs its password; `LOCAL
   SERVICE` needs neither.

5. Check: the log shows `connected to the gateway at wss://...`, and
   `hostel status` shows the room `running` within a few seconds. The server
   probes a room at once when it connects.

`--ssh` (default `127.0.0.1:22`) is the room's own SSH server. If the connection
ends, the room connects again (1 s to 30 s); guest commands fail until then, and
the lease stays.

## 7. Network rule for public static rooms

The gateway enforces access only if a room accepts SSH from nothing else
(`design.md`, section 13). For a static room with a public address, do one of
these:

- Use `connect = true`, and close inbound SSH on the room (firewall or Remote
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

If you lose the database, rooms stay safe: on-demand rooms with this server's
prefix are destroyed at the next reconcile of a new server with the same
`server_id`, and leases end. Tokens must be created again.

If you lose `server_ed25519`, put the new public key on each static room.
If you lose `gateway_host_ed25519`, guests see a new gateway host key; they get it
again from the server with each lease.
