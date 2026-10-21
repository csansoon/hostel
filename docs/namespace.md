# Namespace facts

Namespace (namespace.so) is the first on-demand provider. This file records what
we measured and read on 2026-10-20 and 2026-10-21. Verify a fact again before a
design decision depends on it; plans and CLIs change.

The owner's Mac has both CLIs installed and logged in: `devbox` 0.0.204 and `nsc`
0.0.587. The account is on the **Developer** plan.

## Two products on the same compute

| | Devbox (`devbox` CLI) | Compute instance (`nsc` CLI) |
|---|---|---|
| Purpose | A development machine for people and agents | Raw compute, mostly CI runners and builds |
| Storage | Persistent: files stay after stop and resume | Ephemeral: all files go at the deadline (cache volumes are possible) |
| Lifecycle | Stops after idle time; starts again on the next connection | Fixed deadline (`--duration`), then destroyed |
| Reuse | `devbox acquire <tag>` reuses a free devbox with that tag | none |
| Access | SSH through the `devbox` CLI as a proxy; `exec`, `upload`, `download` | `nsc ssh`, `nsc vnc`, `nsc rdp` |
| Desktop control | `devbox desktop screenshot/click/type/press` (macOS only) | VNC only |
| Platforms | Linux, macOS | Linux, macOS, Windows |
| Billing | Devbox minutes | Plan unit minutes |

A devbox runs on a compute instance: the spike devbox appeared in `nsc list` as a
macOS instance.

## Plan limits

| Plan | Devboxes per user / workspace | Max devbox duration | Linux concurrency | macOS concurrency | Windows concurrency |
|---|---|---|---|---|---|
| Developer (current) | 1 / 3 | 3 h | 32 vCPU / 64 GB | 12 vCPU / 28 GB | not available |
| Team | 3 / 30 | 5 h | 64 / 128 | 24 / 56 | 32 / 64 |
| Business | 5 / 50 | 24 h | 160 / 320 | 48 / 112 | 80 / 160 |

- The devbox limit counts running **and** stopped devboxes.
- Concurrency limits apply to devboxes and instances together.
- Devbox minutes for each minute of use: Linux S = 1, M = 2, L = 4, XL = 8;
  macOS M = 15, L = 30.
- Higher limits: support@namespacelabs.com.
- All Hostel leases use the owner's account, so the "per user" limit applies to all
  agents together.

## Spike measurements (2026-10-20)

macOS devbox, image `tahoe-slim` (macOS 26.6.2, virtual M4 Pro, 6 CPU, 14 GB):

| Step | Time |
|---|---|
| `devbox create` | 22 s |
| Xcode 26.5 and the iOS 26.5 simulator | already in the image |
| Simulator first boot / second boot | 27 s / 9 s |
| Upload of the ExampleApp source (`rsync`) | 8 s |
| `tuist install` + `tuist generate` (tuist from mise) | 86 s |
| Cold `xcodebuild` of the ExampleApp iOS app | 53 s |
| Install and launch in the simulator | 5 s |
| `devbox desktop screenshot` | about 10 s |
| Reverse tunnel `ssh -R` through the devbox SSH alias | worked (HTTP 200, 0.16 s) |

Linux compute instance (`nsc create --bare --machine_type linux/amd64:4x16`):

| Step | Time |
|---|---|
| Create | 6 s |
| `/dev/kvm`, Docker, root access | present |
| Android 15 emulator, from nothing to booted (`cirruslabs/android-sdk:35` image) | 99 s |
| Cold ExampleApp `./gradlew assembleDebug` | 3 min 25 s |

macOS compute instance (`nsc create --machine_type macos/arm64:6x14`): 20 s, but
the selector `macos.version=26.x` gave macOS 26.3.1 without Xcode 26.5.

Windows: `nsc create` failed with "not allowed to create windows/amd64 instances"
(Developer plan).

## CLI reference (relevant flags only)

### devbox

| Command | Use |
|---|---|
| `devbox create --name N --platform linux\|macos --size s\|m\|l\|xl --image I --ephemeral --auto_stop_idle_timeout 30m --no_checkout --purpose TEXT` | Create. macOS sizes: m, l. `--from FILE` reads a spec (JSON, YAML, TOML). `--persistent` defaults to true. |
| `devbox acquire TAG [create flags]` / `devbox release --lease_id ID` | Lease by tag; creates a devbox if no free devbox has the tag. |
| `devbox list -o json` | List. Without `-o json`, it needs a terminal. |
| `devbox exec NAME [-d] -- CMD` | Run a command. `-d` detaches and prints an exec ID. |
| `devbox logs NAME EXEC_ID [--no_follow]` | Stream the logs of an exec. |
| `devbox ssh NAME [-T] -- CMD` | SSH session. |
| `devbox configure-ssh` | Writes `~/.namespace/ssh/<name>.ssh`; `~/.ssh/config` includes `~/.namespace/ssh/*.ssh`. The proxy is `~/.namespace/ssh/devbox-ssh-proxy`, a link to the `devbox` binary. |
| `devbox upload` / `devbox download` | Copy files. |
| `devbox desktop screenshot NAME --file F` / `click --x --y` / `type` / `press` / `connect` | macOS desktop control. |
| `devbox port-forward` | Forward ports from a devbox to localhost. |
| `devbox shutdown NAME --force` | Stop (alias `stop`). |
| `devbox expire NAME --force` | Delete (aliases `delete`, `destroy`). |
| `devbox image list -o json` | Images. Without `-o json`, it needs a terminal. |
| `devbox auth check-login` | Exit code 0 if logged in. |

### nsc

| Command | Use |
|---|---|
| `nsc create --bare --machine_type OS/ARCH:CPUxMEM --duration D --label K=V --ssh_key PUBKEY --selectors K=V --cidfile F -o json --wait_timeout D` | Create an instance. `--unique_tag` and `--volume cache:TAG:MOUNT:SIZE` exist. |
| `nsc list -o json [--label K=V]` | List. Without `-o json`, it needs a terminal. |
| `nsc ssh ID [-T] [-- CMD]` | SSH session. |
| `nsc extend ID --ensure_minimum D` | Extend the deadline. |
| `nsc destroy ID --force` | Destroy. |
| `nsc proxy`, `nsc vnc`, `nsc rdp` | Other access methods. |
| `nsc auth check-login` | Exit code 0 if logged in. `nsc auth generate-dev-token` and `exchange-oidc-token` exist (possible headless login; verify in M5). |

## Traps

- **Relative paths.** A devbox SSH session starts in `~/workspaces`. A relative
  remote path lands there. Use absolute paths.
- **Agent forwarding.** The generated devbox SSH config sets `ForwardAgent yes`.
  Hostel must not forward an agent.
- **Long silent commands.** `nsc ssh` dropped a long Gradle command that printed
  nothing ("remote command exited without exit status"). Run long commands detached
  and poll or stream logs.
- **Leftover files.** `devbox expire` leaves `~/.namespace/ssh/<name>.{ssh,key}` on
  the local machine.
- **Limit errors.** A second devbox fails with "per-user devbox limit reached (1)"
  (gRPC `ResourceExhausted`). Hostel must treat this as "limit reached".
- **Image versions.** Selectors can give an older macOS without the needed Xcode.
  Check the toolchain in the `provision` script and compare it with the labels.

## Facts to verify (with milestone)

| Question | Milestone |
|---|---|
| The exact proxy command in a generated `.ssh` file, and how the server can `Dial` a devbox with it. | M3 |
| What `--ephemeral` does at stop. Can an ephemeral devbox stop and resume, or only be deleted? | M3 |
| The storage cost of a stopped devbox. | M3 |
| Can a name prefix or `--purpose` identify Hostel devboxes in `devbox list -o json`? | M3 |
| How the server can `Dial` an `nsc` instance's SSH server (`--ssh_key` with a public address, `nsc proxy`, or a relay through `nsc ssh`). | M4 |
| KVM on a Linux devbox (it was tested only on an instance). | M4 |
| Does the Android emulator run inside a macOS devbox (virtualization inside a virtual machine)? | M4 |
| Headless login for `devbox` and `nsc` on a Linux VPS. | M5 |
| SSH server on a Windows instance (after a plan change). | M6 |

## Sources

- https://namespace.so/docs/devbox/index.md
- https://namespace.so/docs/devbox/lifecycle.md
- https://namespace.so/docs/workspaces/billing-and-limits.md
- https://namespace.so/docs/architecture/compute/resource-limits.md
- https://namespace.so/docs/architecture/compute/windows.md
- https://namespace.so/docs/reference/devbox-cli/acquire.md
- `devbox <command> --help` and `nsc <command> --help`
