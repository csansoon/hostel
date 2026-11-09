---
name: hostel-users
description: Manage the people on a Hostel server - invite a coworker, make someone admin or member, connect your own other computer, disconnect a lost laptop, or remove a user. Use it when the user says "invite a coworker", "add Alice to Hostel", "make Bob an admin", "connect my other laptop", "I lost my laptop", "remove a user", or "nobody can sign in as admin".
---

# Users, roles, and invites

Each person on a server is a **user** with a unique name and a role. Each
computer of a user has its own token, so an admin can disconnect one computer.
Agents on a computer use that computer's connection: they are not users.

## Roles

| Action | member | admin |
|---|---|---|
| See status, machines, the waiting line, tools, users, configuration | yes | yes |
| Acquire and use leases; release their own lease | yes | yes |
| Release, move, or change the priority of another user's lease | no | yes |
| Change tools, machines, providers, settings | no | yes |
| Invite, change, or remove users | no | yes |

- Members see the configuration with secret values (`env`, `command_env`,
  `github_token`) as `***`.
- The server refuses to remove or demote the last admin.

## List the users

```sh
hostel config users          # NAME, ROLE, COMPUTERS, SINCE
```

In the app: Front desk › Guests.

## Invite a person (admin)

```sh
hostel config users invite                         # a member
hostel config users invite --role admin            # an admin
hostel config users invite --name "Alice Example"  # fix the name
```

It prints:

```
Send this to the new user. They run it on their computer:

  hostel connect hostel1-...

The code works once, for 24 hours.
```

Without `--name`, the user gives their name at connect (default:
`git config user.name`). The server refuses a name that another user has.

In the app: Front desk › Guests › Invite a guest, then `m` (member) or `a` (admin). The
app shows the code once, and prints it again when you quit, to copy.

Send the person to `hostel-install`: they install `hostel` and run the
`hostel connect` command (or paste the code in the app's Check in).

## A code for your own other computer (any user)

```sh
hostel config users code
```

Run the printed `hostel connect hostel1-...` on the other computer. It connects
as the same user, with its own token. In the app: Front desk › Guests › Add
your other computer.

## Change a role (admin)

```sh
hostel config users set "Alice Example" --role admin
hostel config users set "Alice Example" --role member
```

In the app: Front desk › Guests, select the user, `r`.

## Remove one computer (admin)

For a lost computer, or one that could not disconnect. Only that computer is
disconnected; the user stays.

```sh
hostel config users remove "Alice Example" --computer alice-laptop
```

The computer names are in the COMPUTERS column of `hostel config users`.

On your own computer, `hostel disconnect` removes its connection and deletes
its token on the server. In the app: Front desk › Check out.

## Remove a user (admin)

```sh
hostel config users remove "Alice Example"
```

The server deletes their tokens (all their computers are disconnected),
removes their queue entries, and ends their active leases with cleanup. In the
app: Front desk › Guests, select the user, `x`.

## When nobody can sign in as admin

On the server machine, as the user that runs the server:

```sh
hostel server invite                                   # an admin
hostel server invite --role member --name "Bob Example"
hostel server invite --config /path/to/server.toml
```

It reads the database directly and does not need the API or a connection.

## Notes

- An invite code contains the server's addresses (`public_urls`), the pinned
  TLS key (for `tls = "self-signed"`), and a one-time secret. If the addresses
  change, make new codes.
- Repeated wrong codes from one address are rate limited ("too many failed
  attempts; wait a minute").
- Machines do not use user invites. They use machine codes from
  `hostel config machines add` (`hostel-machines`).
- `hostel admin token create --name <name> --role machine --machine <provider>/<machine>`
  makes a machine token for a machine that you connect by hand. Users always
  connect with invite codes.
