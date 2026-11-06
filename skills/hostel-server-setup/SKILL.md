---
name: hostel-server-setup
description: Set up, configure, back up, or update a Hostel server on a Linux VPS or a home machine. Use it when the user says "set up a Hostel server", "deploy hostel", "run hostel server", "put Hostel behind Caddy/a proxy", "change a server setting", "server.toml", "back up Hostel", or "update the server".
---

# Set up a Hostel server

The server keeps the queue, the users, the configuration, and the SSH gateway.
Run one server for a team. Agents and people connect to it from their own
computers (`hostel-install`).

## 1. Choose the machine and the TLS mode

- A Linux machine with systemd (amd64 or arm64; 2 CPUs and 4 GB are enough), or
  a home machine that stays on. The repository documents only the systemd
  service; on another OS, run `hostel server run` with your own service manager.
- One HTTPS address that clients can reach. The API and the gateway share it:
  the gateway accepts SSH inside a WebSocket on `/v1/gateway`.

| TLS mode | Use it when | Clients check |
|---|---|---|
| `self-signed` (default) | No domain or no proxy: an IP address, a LAN name, a VPN name. The server serves HTTPS with its own certificate. | The pinned key from the invite code. |
| `off` | A proxy with TLS is in front of the server (Caddy, a cloud load balancer). | The proxy's certificate, in the normal way. |

## 2. Install

As an administrator on the server machine:

```sh
curl -fsSL -o install.sh https://raw.githubusercontent.com/csansoon/hostel/main/install.sh
sudo sh install.sh --dir /opt/hostel/bin --server /opt/hostel
sudo chown -R alice /opt/hostel          # the user that runs the service
sudo ln -sf /opt/hostel/bin/hostel /usr/local/bin/hostel
```

`/opt/hostel/bin/hostel` is the server binary. The service's user owns
`/opt/hostel`, so that an admin can update the server from the app or from a
computer (section 9). `/opt/hostel/helpers` gets the binaries for all rooms (`hostel-<os>-<arch>`,
`mise-<os>-<arch>`). The server uses the tools in its binary: the install
script does not copy tool definitions.

## 3. Initialize

Run `hostel server init` as the user that runs the service (not root). In a
terminal it asks for the server name, the addresses, and your name when the
flags do not give them. Flags:

| Flag | Meaning |
|---|---|
| `--server-id` | The server's name, `[a-z0-9]{1,20}` (default: from the host name). It is part of the names of rooms that the server creates. |
| `--listen` | The API address (default `127.0.0.1:7410`). |
| `--tls` | `self-signed` (default) or `off`. |
| `--url` | An address that clients use. Repeat it for several. Invite codes contain them. |
| `--user` | Your name, for the first admin. |
| `--helper-dir` | The binaries for the rooms: `/opt/hostel/helpers` after `install.sh --server /opt/hostel` (default: `data_dir/helpers`). |
| `--config` | The file to write (default `~/.config/hostel/server.toml`). |

Self-signed, on its own port (open TCP 7410 in the firewall):

```sh
hostel server init --server-id demo --listen 0.0.0.0:7410 \
  --url https://hostel.example.com:7410 --user "Alice Example" \
  --helper-dir /opt/hostel/helpers
```

Behind a proxy with TLS:

```sh
hostel server init --server-id demo --tls off --listen 127.0.0.1:8000 \
  --url https://hostel.example.com --user "Alice Example" \
  --helper-dir /opt/hostel/helpers
```

With `--tls off` and an `https://` address, `init` also writes
`gateway_public = "wss://hostel.example.com/v1/gateway"`: guests and joined
machines reach the SSH gateway through the proxy. The proxy must forward
`/v1/gateway` (a WebSocket) to the server.

`init` writes `server.toml`, `hostel.schema.json`, the data directory
(`~/.local/share/hostel`: the database, `server_ed25519`, `gateway_host_ed25519`,
and the TLS certificate), and prints the server's SSH public key and the first
admin invite:

```
Connect your computer as the first admin. Run this on your computer:

  hostel connect hostel1-...
```

Run that command on your own computer within 24 hours (`hostel-install`).

## 4. Edit `server.toml`

`server.toml` has only the keys that the server needs before it opens its
database: `server_id`, `listen`, `gateway_listen`, `gateway_public`, `tls`,
`public_urls`, `data_dir`, `helper_dir`. A `platforms_dir` key (written by
the install script before M10e) is ignored with a warning: delete it. `init` writes
`helper_dir` (with `--helper-dir`) and, behind a proxy, `gateway_public`. Check
them:

```sh
grep -E 'helper_dir|gateway_public' ~/.config/hostel/server.toml
hostel admin config check
```

Without `helper_dir`, the server cannot give rooms their helper, and `hostel
machine join` cannot download the binary. Without `gateway_public` behind a
proxy, every guest SSH session and every joined machine fails.

A Caddy example (Caddy passes WebSocket upgrades without more settings):

```
hostel.example.com {
	reverse_proxy 127.0.0.1:8000
}
```

With `tls = "self-signed"`, the default `gateway_public` is `/v1/gateway` on the
same address; do not set it. Check the file (it prints each problem and the
effective values):

```sh
hostel admin config check
```

A change of `server.toml` needs a restart of the server.

## 5. The systemd service

`/etc/systemd/system/hostel.service` (replace `alice` with the user that ran
`init`):

```ini
[Unit]
Description=Hostel server
After=network-online.target
Wants=network-online.target

[Service]
User=alice
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

`hostel server run --config <file>` selects another `server.toml`. The log is
JSON on stderr. If the configuration is invalid, the server does not start and
the log says why.

For Namespace rooms, the `devbox` and `nsc` CLIs must be on the service's
`PATH` (`hostel-rooms`, section "Namespace").

## 6. The configuration in the database

Everything else (providers, machines with their tools, and the settings) is the
configuration document in the database. An admin changes it from any
connected computer. A valid change applies at once, without a restart.

```sh
hostel config                    # show the document (members see secrets as ***)
hostel config edit               # change it in $VISUAL / $EDITOR
hostel config apply file.toml    # replace it with a file (or - for stdin)
hostel config history            # the revisions, with user and time
hostel config settings           # the settings that are not defaults
hostel config settings set idle_timeout=20m
```

Settings: `idle_timeout` (10m), `poll_timeout` (5m), `limit_retry` (2m),
`port_process_offline` (10m), `intervals.scheduler` (10s),
`intervals.reconcile` (5m), `intervals.tunnel_check` (30s),
`intervals.presence` (30s), `mise.idiomatic_tools` (`["node", "python"]`),
`mise.github_token`, `mise.agent_device` (true).

The server refuses a change if another revision came first (edit again) or if
it is invalid (the error names the key path). Machines and providers:
`hostel-rooms`. Tools: `hostel-tools`. Users: `hostel-users`.

## 7. Invites on the server machine

If nobody can sign in as admin, make an invite on the server machine itself (it
does not use the API):

```sh
hostel server invite                     # an admin
hostel server invite --role member --name "Bob Example"
```

## 8. Backups

The state is in the data directory (`~/.local/share/hostel`). Back up the keys
once (`server_ed25519`, `gateway_host_ed25519`, `tls_cert.pem`, `tls_key.pem`).
Back up the database with SQLite, not with a file copy:

```sh
sqlite3 ~/.local/share/hostel/hostel.db ".backup /var/backups/hostel.db"
```

- A lost database: users must connect again; on-demand rooms with this
  `server_id` are destroyed at the next reconcile.
- A lost `server_ed25519`: put the new public key (`hostel admin pubkey`) on each
  static machine.
- A lost TLS key (self-signed): each client must connect again with a new
  invite code.

## 9. Update

From v0.2.0, the server updates itself. Each way checks the signature of the
release, replaces the server binary and the room binaries in `helper_dir`, and
refuses a release that is not newer. The server sends the new helper to each
room before its next lease.

- **From the app** (an admin): Front desk › Update the server shows when a
  newer release exists. The server starts again by itself with the new binary.
  Leases and their jobs continue; a `hostel run` that streams output at that
  moment loses the stream (`hostel attach <job>` continues it).
- **From a connected computer** (an admin): `hostel server update --remote`.
  It waits until the server runs the new version.
- **On the server machine**, as the service's user (or with `sudo` when that
  user cannot write the files), then restart:

  ```sh
  hostel server update
  sudo systemctl restart hostel
  ```

A remote update needs that the service's user can write the binary's directory
and `helper_dir` (section 2); otherwise it fails with "this user cannot write
…". `--version vX.Y.Z` selects a release. A server before v0.2.0, or one
installed in `/usr/local/bin`: install again, then restart:

```sh
curl -fsSL -o install.sh https://raw.githubusercontent.com/csansoon/hostel/main/install.sh
sudo sh install.sh --dir /opt/hostel/bin --server /opt/hostel
sudo systemctl restart hostel
```

Machines that joined from behind NAT run their own copy of `hostel` for the
connection: the app shows "Out of date" on them (`hostel-rooms`).

## Next

- Add rooms: `hostel-rooms`.
- Invite people: `hostel-users`.
