# M14. Signing secrets (plan)

Status: phases 1 to 5 done (section 10); the real check on a Mac (phase 6) and
the E2E gate are next. Owner decisions of 2026-11-26 (section 2).

## 1. Why

A Mac Catalyst app (and any signed Mac app) needs a real signature to run with
its entitlements. Push notifications, keychain access groups, and credential
provider extensions are restricted entitlements: macOS accepts them only with a
provisioning profile. An ad hoc signature removes them, and the app then fails
at run time, often without a clear error (`errSecMissingEntitlement`, -34018).

A developer's own Mac signs with the Apple account in Xcode. A Hostel Mac has
no account, its builds run over SSH (the login keychain is locked), and many
guests use it. Since M10h, an administrator installs signing material on each
Mac by hand (`machine-setup.md`, section 3.7). That has three problems:

1. Manual work on each Mac, again for each new or changed key.
2. The certificate and the App Store Connect API key stay on the machine between
   leases.
3. On-demand machines cannot get it (open question 8 in `requirements.md`).

## 2. Owner decisions (2026-11-26)

- **One team identity.** All guests of a server may use one shared signing
  identity: an "Apple Development" certificate and an App Store Connect API key
  that are only for Hostel. It replaces the M10h rule that signing material is
  part of the machine setup.
- **On the server, for each lease.** The server stores the signing material.
  A lease that asks for signing gets it for the lease only; cleanup removes it.
  Nothing about signing is installed on the Mac.
- **Hostel never registers Macs in the Apple team.** An Apple team can register
  100 Macs in each membership year, and a removed device does not give its place
  back. An administrator registers each Mac by hand, once. Signing is for
  machines with a fixed Provisioning UDID (static Macs); a new virtual machine
  gets a new UDID, so on-demand Macs get no signing.

## 3. The secret store (server)

A small, general store: named files that only admins write and that only tools
read. It is not only for Apple: a Windows package certificate (`machine-setup.md`,
section 4.5) can use it later.

- **Storage.** `<data_dir>/secrets/<name>` (file `0600`, directory `0700`), as
  for the client secret of an azure provider. A name is 1 to 64 characters of
  `a-z`, `0-9`, and `-`. A value is at most 64 KB.
- **API** (admin only):
  - `GET /v1/secrets`: `[{"name", "set", "size", "set_at", "tools"}]`
    (`tools`: the tools that declare it; also a secret that a tool declares
    and that is not set). Never a value. Who set it is in the event log.
  - `PUT /v1/secrets/{name}`: the body is the value.
  - `DELETE /v1/secrets/{name}`: refused while a tool declares it
    (as `hostel config tools remove` is refused while a machine lists the tool).
- **CLI.**
  ```
  hostel config secrets                              # name, size, set at, tools
  hostel config secrets set <name> <file>            # a file
  hostel config secrets set <name> -                 # stdin (a password: not in argv or shell history)
  hostel config secrets remove <name>
  ```
- **Events.** "secret X set by U" and "secret X removed by U", with no value.
  The value is never in a log, an event, an answer, or the configuration
  history.

## 4. Secrets in a lease

- **A tool declares its secrets** in `tool.toml`: `secrets = ["a", "b"]`. Only
  admins change tools, so a member cannot write a tool that reads a secret. The
  repository (`hostel.toml`) cannot ask for a secret.
- **Lease tools.** Until M14, only screen tools have `start` and `stop`. In M14,
  any defined tool with a `start` script starts for each lease that has it (the
  lease asked for it, or a requested tool needs it), and its `stop` runs at
  cleanup. Its `env` and `path` then apply only to those leases, as for a
  screen tool. `screen = true` still means: a screen (options, `screenshot`,
  `whole_machine`). The other way, `apple-signing` as a screen tool without a
  screen, would show it as a screen in `hostel tools` and to agents.
- **Delivery.** After `begin`, the server sends the secrets of the lease's
  tools with the helper command `secrets` (JSON, base64; not in `begin`,
  because `begin` writes its input to `lease.json`). The helper writes them to
  `rooms/<room>/secrets/` (directory `0700`, files `0600`) before the `start`
  scripts. Only the `start` and `stop` scripts of a tool with `secrets` get
  `HOSTEL_SECRETS_DIR`. Jobs and shells of the guest do not get the variable.
  This does not protect the files: they belong to the same user as the guest's
  commands. The owner accepts that each guest of a lease with the tool can read
  them (section 2).
- **Cleanup.** The `stop` scripts run first (step 2). Then the room directory is
  deleted with `secrets/`, as now.
- **A missing secret.** `hostel acquire` fails at once when a tool of the
  request (or one of its needs) declares a secret that is not set: "the tool
  apple-signing needs the secret asc-key, which is not set; an admin runs
  `hostel config secrets set asc-key <file>`". A tool push with an unset secret
  is not an error, so the order of the admin's steps does not matter. A queued
  lease whose tool got a new secret fails with the same reason when its turn
  comes (the scheduler checks it, and the preparing step again before it uses
  the machine); the machine stays as it is.
- **By OS.** A tool starts on a machine when it is a screen or has a `start`
  script for the machine's OS. The machine gets only the secrets of the tools
  that start on it.
- **Lease users (M11).** With `lease_users`, the files belong to the lease user.
  A keychain of a lease user is not checked on a real Mac (section 8).

## 5. The tool `apple-signing` (macOS)

A default tool (`tools/apple-signing/`): `tool.toml`, `GUIDE.md`,
`start.macos.sh`, `stop.macos.sh`. `needs = ["xcode"]`.

Secrets (names in the default `tool.toml`):

| Secret | Value |
|---|---|
| `apple-signing-p12` | the certificate with its private key (`.p12`) |
| `apple-signing-p12-password` | its password |
| `asc-key` | the App Store Connect API key (`.p8`) |
| `asc-key-id` | its key ID |
| `asc-issuer-id` | the issuer ID of the team |

`start`:

1. Make a keychain in the room (`rooms/<room>/signing.keychain-db`) with a
   random password, with no auto-lock, and unlock it.
2. Import Apple's intermediate certificate "WWDR G3" (public; in the script,
   because a tool directory has only `tool.toml`, `GUIDE.md`, and scripts) and
   the `.p12` with `-x` and `-T /usr/bin/codesign` only, set the partition list
   for `codesign`, and delete the `.p12` and its password from the room.
3. Add the keychain to the user's search list. Several rooms of one Mac change
   the same list, so the change is under a lock, and it keeps the other entries.
4. Check that `security find-identity -v -p codesigning <keychain>` finds the
   identity. Else fail with the reason (for example an expired certificate).
5. Write `tool.env`: `HOSTEL_SIGNING_KEYCHAIN`, `HOSTEL_SIGNING_TEAM` (the team
   ID, from the certificate), `ASC_KEY_ID`, `ASC_ISSUER_ID`, and `ASC_KEY_PATH`
   (a copy of the `.p8` in `secrets/` as `AuthKey_<key ID>.p8`). These are the names of `machine-setup.md`,
   section 3.7.

`stop`: remove the keychain from the search list (under the lock) and delete it.
It does nothing when there is no keychain (a repeated cleanup).

`GUIDE.md` tells agents:

- Ask for it with the desktop: `hostel acquire xcode macos-desktop apple-signing`.
- Build with automatic signing and the key:
  `xcodebuild … -allowProvisioningUpdates -authenticationKeyPath "$ASC_KEY_PATH"
  -authenticationKeyID "$ASC_KEY_ID" -authenticationKeyIssuerID "$ASC_ISSUER_ID"`.
  Xcode makes or downloads the development profiles; they stay on the machine as
  a cache (they are not secret).
- Never `-allowProvisioningDeviceRegistration`, never ad hoc signing
  (`CODE_SIGNING_ALLOWED=NO`, `CODE_SIGN_IDENTITY=-`).
- An error that the device is not in the profile means that the Mac is not
  registered: stop and tell the owner.

## 6. Machine setup and design docs

- `machine-setup.md`, section 3.7, is replaced. Admin steps, once:
  1. On the Apple side: a development certificate only for Hostel (exported as
     `.p12`) and a team API key with the role Developer.
  2. On the server: `hostel config secrets set` for the five secrets.
  3. For each Mac: register its Provisioning UDID in the team by hand
     (`system_profiler SPHardwareDataType | grep "Provisioning UDID"`; it does
     not change when macOS is installed again), then add `apple-signing` to the
     machine's `tools`.
  4. Revoke the certificate and the key when they are not used anymore; replace
     them before the certificate expires (one year) with `secrets set`.
- `design.md`: the secret store (sections 4, 11, 18), lease tools and
  `secrets` in the tool contract (sections 3 and 8), the helper `begin` input,
  and the new default tool.
- `requirements.md`: open question 8 is answered for static Macs.

## 7. E2E scenarios

The E2E machines are Linux containers, so they test the secret store and the
lease tools with a test tool. The Apple scripts get a check on a Mac
(section 9).

1. **Store.** An admin sets a secret from a file and one from stdin; the list
   shows name, size, and tools, never the value. A member gets HTTP 403 for
   list, set, and remove. The random test value is not in the server log, the
   event log, or `hostel config history`.
2. **Missing secret.** `acquire` of a test tool whose secret is not set fails at
   once with the message of section 4; after `secrets set`, the same request
   gets a lease.
3. **Delivery.** The test tool's `start` writes the SHA-256 of the secret (not
   the value) to `tool.env`; the guest sees the right hash. The file mode is
   `0600`. A job of the guest has no `HOSTEL_SECRETS_DIR`.
4. **Replace.** After a new `secrets set`, the next lease gets the new hash.
5. **Cleanup.** After `release`, the test tool's `stop` ran (a marker file
   outside the room) and `rooms/<room>/secrets/` does not exist.
6. **Lease tool without the request.** A lease on the same machine that did not
   ask for the test tool gets no secrets, no `start`, and none of the tool's
   `env`.
7. **Remove.** `secrets remove` is refused while the test tool declares the
   secret; a secret that no tool declares can be removed.
8. **Needs.** A lease for a tool that needs the test tool gets the secret.
9. **A new secret while a lease waits.** The waiting lease fails with the
   reason, and the machine stays `running`.
10. **Screenshots.** A lease whose only started tool is not a screen has no
    screen; with a screen tool too, `hostel screenshot` needs no `--tool`.

## 8. Risks

- **The API key role.** The plan uses the role Developer. Reports say that
  `-allowProvisioningUpdates` with an API key works reliably with Admin. If
  Developer cannot make the profiles, the next try is App Manager. The check in
  section 9 finds it out.
- **Another identity in the login keychain.** If the machine user's login
  keychain has another "Apple Development" identity of the same team (for
  example from an earlier Xcode sign-in), automatic signing can select it. The
  Mac check looks for this; the guide's fix is to remove that identity.
- **Lease users.** A keychain for a new lease user with no login session is not
  checked. The Mac check uses the machine user (desktop leases need it anyway).
- **Guests can copy the secrets.** Accepted (section 2). Use material only for
  Hostel, so that it can be revoked alone. The start script makes the window
  small for the certificate (the `.p12` is on disk for a few seconds), but a
  guest can sign with the identity during the lease, and the `.p8` stays in the
  room for `xcodebuild`.
- **Device registration by a guest.** Hostel never registers devices, but a
  guest with the API key can try. If the role Developer cannot register devices,
  this risk is gone; the check does not try it (it would use one of the 100
  places).

## 9. Phases

| Phase | Work |
|---|---|
| 1 | This plan, owner approval |
| 2 | The secret store: storage, API, CLI, events; E2E scenarios 1 and 7 |
| 3 | Lease tools with `start` and `stop`, `secrets` in `tool.toml`, delivery at `begin`, the acquire check; E2E scenarios 2 to 6 |
| 4 | `apple-signing`: scripts and guide. A check on a local Mac with the scripts and a self-signed test certificate (start, `codesign` of a test binary over SSH, stop, search list as before) |
| 5 | `machine-setup.md`, `design.md`, `requirements.md` |
| 6 | Real check: the production server runs the branch build; the owner's secrets are set there; a static Mac lists `apple-signing`. A Mac Catalyst Debug build with automatic signing, the app starts with its entitlements, a screenshot. After `release`: no keychain, the search list as before, no secret files |
| 7 | The E2E gate (`make test-e2e`, then `make test-e2e-10`), pull request when the owner asks |

## 10. Implementation notes

- **Phases 2 and 3** (commit `10e7b25`, review fixes in `3c50135`): the secret
  store, lease tools, and delivery. `StartedTool.no_screen` marks a started tool
  that is not a screen, so that `hostel screenshot` ignores it. A requested
  version of a lease tool stays in `HOSTEL_TOOLS`.
- **Phase 4** (`3c50135`): `tools/apple-signing`. Checked on a local Mac (macOS
  27.2) with the real Hostel certificate (a `.p12` made again from its key, with
  a known password; no Apple service was called): `start` makes the keychain
  with one valid identity and the right team ID; `codesign` finds the identity
  through the search list, over a shell with no terminal, without a prompt;
  `stop` deletes the keychain, and the search list is the same as before; a
  repeated `stop` does nothing.
- **Found in phase 4:** with `-T /usr/bin/security` in the import (as in the
  M10h guide), `security export` gives out the private key, also with `-x`.
  Without it, an export has no key. The tool and the new machine guide do not
  use it.
- **Review** (a subagent, of `10e7b25`): a missing secret at the preparing step
  made the machine `broken`; a requested version of a lease tool was lost; the
  server and the helper decided "starts for the lease" differently for a tool
  with a `start` script for one OS only; all start scripts got
  `HOSTEL_SECRETS_DIR`. All are fixed, with E2E scenarios 8 to 10.
