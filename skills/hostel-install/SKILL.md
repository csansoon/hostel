---
name: hostel-install
description: 'Install, update, or check the hostel CLI on macOS, Linux, or Windows, and connect the computer to a Hostel server. Use it when the user says "install hostel", "update hostel", "hostel command not found", "connect to our Hostel server", "I have an invite code", "disconnect", or "open the Hostel app".'
---

# Install the Hostel CLI and connect

The `hostel` binary is the same for people, agents, the server, and room
machines. This skill installs it on a person's computer and connects that
computer to a server. For a server machine, see `hostel-server-setup`. For a
room machine, see `hostel-rooms`.

## macOS and Linux

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh
```

The script downloads the latest GitHub release, checks the file against
`SHA256SUMS` of the release, and installs `hostel` into `~/.local/bin`.

Options (put them after `sh -s --`):

| Option | Effect |
|---|---|
| `--dir DIR` | Install the binary into `DIR` (default `~/.local/bin`). |
| `--version vX.Y.Z` | Install this release (default: the latest). |
| `--server DIR` | Also install the room binaries into `DIR/helpers` and the platform scripts into `DIR/platforms`. Only for a server machine. |

Examples:

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh -s -- --dir /usr/local/bin
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh -s -- --version v0.1.0
```

If the script prints `note: ... is not on your PATH`, add the directory to
`PATH` in the shell profile, for example:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Supported: macOS and Linux on amd64 or arm64. The environment variable
`HOSTEL_INSTALL_URL` replaces the download URL (a mirror).

## Windows

In PowerShell (no administrator rights necessary):

```powershell
irm https://raw.githubusercontent.com/csansoon/hostel/main/install.ps1 | iex
```

The script installs `hostel.exe` into `%LOCALAPPDATA%\Programs\hostel`, checks it
against `SHA256SUMS`, and adds that directory to the user `PATH`. Open a new
terminal after the first install. `$env:HOSTEL_VERSION` (for example `v0.1.0`)
selects a release; `$env:HOSTEL_INSTALL_URL` sets a mirror.

Note: `install.ps1` is new. If it fails, download `hostel-windows-amd64.exe`
(or `-arm64.exe`) from the GitHub release, rename it to `hostel.exe`, and put it
in a directory on `PATH`.

## Check the install

```sh
hostel --version
hostel help
```

## Connect the computer to a server

An admin of the server gives you an invite code (`hostel1-...`). It works once,
for 24 hours. See `hostel-users` for how an admin makes one.

```sh
hostel connect hostel1-...
hostel connect hostel1-... --name "Alice Example"
```

- The default name comes from `git config user.name`. In a terminal, `connect`
  asks for it.
- The output shows the server, the pinned TLS key (for a server with its own
  certificate), your role, and the computer name.
- The connection goes to `~/.config/hostel/config.toml` (mode 0600). On Windows
  it is in the same place below `%USERPROFILE%`.
- A computer has at most one connection. If one exists, `connect` asks before it
  replaces it. `--yes` replaces it without the question.
- Agents on this computer use the same connection. They do not need their own.

Check it:

```sh
hostel status
```

Disconnect (the server deletes this computer's token):

```sh
hostel disconnect
```

For tests and CI, the environment variables `HOSTEL_SERVER`, `HOSTEL_TOKEN`,
and `HOSTEL_SERVER_KEY` override the file.

## The Hostel app

Run `hostel` with no arguments in a terminal. It opens a full-screen app:

- Not connected: *Check in* (paste the invite code, then your name) or *Leave*.
- *Lobby*: the platforms, their free machines, the queue, and the wait.
- *Rooms*: the machines, their state, and who uses them.
- *Front desk*: the server and you; *Invite a guest* (admins), *Add your other
  computer* (members), *Guest list*, *Add a room* (admins), *Check out*
  (disconnect).

Keys: `1` `2` `3` go to Lobby, Rooms, Front desk; `?` lists all keys; `q` quits.
`NO_COLOR=1` turns colors off. Without a terminal (an agent, a pipe), `hostel`
prints the help instead. Agents use `hostel status` (plain text).

## Update

From v0.2.0:

```sh
hostel update --check   # is a newer release available?
hostel update           # install it (it checks the release signature)
hostel --version
```

The app (`hostel`) shows Update in its menu when a newer release exists. A
command prints `the server runs Hostel vB and this computer has vA; run "hostel
update"` (at most once a day) when the server is newer. If the binary is in a
directory that the user cannot write, `hostel update` says so: run `sudo hostel
update`, or install again. The connection stays.

Before v0.2.0, run the install command again (on Windows the `irm ... | iex`
line):

```sh
curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh
```

`hostel update` exit codes: 0 done or up to date, 1 error, 69 GitHub cannot be
reached. `HOSTEL_NO_UPDATE_CHECK=1` stops the background check of the app.

For the server machine, also restart the service after the update
(`hostel-server-setup`, section "Update").

## Next

- Use a room: `hostel-use`.
- Something fails: `hostel-troubleshoot`.
