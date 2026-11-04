# M4 implementation plan

Status: done, waiting for the owner's approval (2026-10-27). Milestone M4 in `plan.md`: Linux rooms on
Namespace and the `android` platform. This file gives the facts, the decisions,
the build order, and the risks. `design.md` stays the contract. When this plan
adds a detail to the contract, the same commit updates `design.md`.

## 1. Facts from the experiments (2026-10-27)

`devbox` 0.0.204 and `nsc` 0.0.587 on the owner's Mac, Developer plan.

- **Linux devbox and KVM.** A Linux devbox (size M, `builtin:default`) is Ubuntu
  24.04 on x86_64 with 8 vCPU and 15 GB. `/dev/kvm` exists and every user can read
  and write it. Java 21, Docker, and `sudo` without a password are present.
- **Android emulator on a Linux devbox.** The SDK (`cmdline-tools`,
  `platform-tools`, `emulator`, `system-images;android-36;google_apis;x86_64`,
  5.4 GB) downloads in about 30 s. The emulator needs Ubuntu packages that the
  image does not have (for example `libpulse0`). Cold boot of a Pixel 7 AVD:
  38 s. Boot from a saved snapshot: 7 s; with `-read-only`: 4 s.
  `adb exec-out screencap -p` works (565 KB PNG). `sdkmanager` prints that it is
  deprecated and that the "Android CLI" (`android sdk`) replaces it; it still
  works.
- **One devbox.** The Developer plan allows one devbox (running or stopped) for
  all Hostel rooms. A Linux devbox for Android blocks the macOS devbox for iOS.
  Instances do not count against this limit, only against the concurrency limits
  (32 vCPU and 64 GB for Linux).
- **`nsc` instances.**
  - `nsc create --bare --machine_type linux/amd64:2x4 --duration 20m --ssh_key
    <file> --label K=V --purpose TEXT --cidfile F -o json`: 3–6 s. `--ssh_key`
    takes a file path. `-o json` gives `instance_id`.
  - A bare instance is Wolfi Linux, user root, with Docker and `/dev/kvm`
    (root only). It has no Python, `nc`, or `wget`.
  - `nsc list -o json` has `cluster_id`, `labels`, `shape`, and `created_at`, but
    no deadline. With no instance it prints `null`.
  - `nsc destroy <id> --force`: under 1 s; a second destroy of the same ID also
    exits with 0 and prints nothing.
  - `nsc ssh <id> -T -- <command>` runs the command string with a shell on the
    instance, as root.
  - The instance's SSH server on port 22 is the Namespace agent. It accepts the
    `--ssh_key` key, but it refuses remote forwards (`-R`), which Hostel tunnels
    need.
  - `nsc instance port-forward <id> --target_port <port>` prints `Listening on
    127.0.0.1:<port>` and forwards each local connection to that port on the
    instance. It is ready at once and serves several connections.
  - OpenSSH in an Ubuntu container on the instance (`docker run --network host
    --device /dev/kvm ubuntu:24.04`, then `apt-get install openssh-server sudo`):
    ready in 17 s. Through `port-forward`, it accepts our key and remote forwards;
    the tunnel test (an HTTP server on the Mac, reached from the instance)
    worked. `/dev/kvm` in the container is root only by default.
  - `nsc run --image` starts a container on a new instance in 3 s, but in its own
    network namespace (port 22 is not published to the instance), and it has no
    option for `/dev/kvm`. Not used.
  - Over the plan limit (for example `linux/amd64:32x128`), `nsc create` does not
    fail: the call blocks with no output, also with `--wait_timeout 20s`, until the
    caller stops it. Nothing is created.
  - `nsc build` starts a builder instance (label `nsc.purpose: builder`); destroy
    it after use.
- **The MacBook** (`home/macbook`, static room of M2): macOS 26.6.2, arm64,
  8 CPU, 8 GB memory, 157 GB free. No Android SDK, no Java, no Homebrew.

## 2. Decisions from the facts

- **Android on Namespace: `instance` machines** (`spec.product = "instance"`). A
  Linux devbox works too (KVM, the same scripts), but it uses the plan's only
  devbox. Owner decision 2026-10-27: no Android on macOS devboxes.
- **Room in a container on the instance.** The provider creates a bare instance
  with `--duration` (the provider-side bound) and the label `hostel.name=<name>`,
  then runs one command with `nsc ssh`: `docker run -d --name hostel-room
  --restart unless-stopped --network host --device /dev/kvm` with the image from
  `spec.image` (default `ubuntu:24.04`, any image with `apt-get`). A start script
  in the container installs OpenSSH and `sudo` if they are missing, creates the
  user `hostel` with the server key, lets it use `/dev/kvm` and `sudo`, and runs
  `sshd` on port 2222. The room is then an ordinary Linux room: the server key,
  the user `hostel`, and remote forwards work.
- **Access.** `Dial` starts `nsc instance port-forward <id> --target_port 2222`,
  reads the local port, connects, and stops the process when the connection
  closes.
- **Idempotent create.** Find the instance by the label first. A `docker run` that
  finds `hostel-room` already there counts as done. `Provision` waits until an SSH
  handshake works (as for a devbox).
- **Deadlines.** `--duration` = the creation deadline from the server. `Extend`
  runs `nsc instance extend-duration <id> --ensure_minimum <deadline - now>`
  (`CanExtend`). Instances cannot stop.
- **Limits.** `nsc create` blocks at the plan limit, so the create call has a
  deadline (2 minutes), and `max_rooms` must keep Hostel under the plan's
  concurrency. A timed-out create is a failed attempt; reconcile destroys an
  instance with the prefix and no room.
- **The `android` platform** (`platforms/android/`), `.linux.sh` and `.macos.sh`:
  - `provision` installs the SDK in `$HOSTEL_CACHE_DIR/android-sdk` (Java from the
    machine-level mise tools), the Linux packages for the emulator (with `sudo`,
    when they are missing), and one golden AVD for each device with a "clean"
    snapshot.
  - `prepare` starts one emulator for each lease device from the clean snapshot
    with `-read-only` (each lease gets a clean device; reset does not have to
    delete data), on a console port for the slot and the device, and writes
    `ANDROID_SERIAL`, `ANDROID_SERIALS`, and `ANDROID_HOME` to `device.env`.
  - `reset` stops the slot's emulators. `screenshot` uses `adb exec-out
    screencap`. `health` checks the SDK, the golden AVDs, KVM (Linux), and disk.
  - Machine env: `HOSTEL_ANDROID_API` (default 36) and `HOSTEL_ANDROID_TAG`
    (default `google_apis`).
- **Tests.** A fake `nsc` CLI (the fake provider under the name `nsc`) at the
  adapter boundary. Its `nsc ssh` runs the real command string with a `docker`
  shim, so the E2E tests run the real start script in the test room image. The
  emulator scripts run only on real machines (like the `ios` scripts); the E2E
  tests use `fakedevice`.

## 3. Build order

1. `platforms/android/` scripts; check them on a Linux devbox and on the MacBook
   through Hostel.
2. The `instance` product in `internal/provider/namespace.go`, with the config
   check and `design.md`.
3. The fake `nsc` CLI and E2E tests: lease, restart during the create, destroy,
   reconcile of a leaked instance, the tunnel. (No short E2E test reaches
   `Extend`: the creation deadline covers the first leases. The real
   `extend-duration` call is checked by hand.)
4. A smoke test against the real account (`HOSTEL_SMOKE_NAMESPACE=1`).
5. Done-when check: an Android app on a Namespace instance against the local
   API, with a screenshot.
6. Gate: `e2e-10` in CI and one local macOS run; `pr-ready`.

## 4. Risks

| Risk | Plan |
|---|---|
| A create at the plan limit blocks. | A 2 minute deadline on the create call; `max_rooms`; reconcile destroys leaks; `--duration` bounds every instance. |
| `apt-get` in the room container is slow or fails. | Measured 17 s; the image can already have OpenSSH (then nothing is installed). |
| 8 GB of memory on the MacBook is small for an emulator and a Gradle build. | Measure; give the AVD 2 GB; document the limit. |
| `sdkmanager` is deprecated. | It still works; move to the Android CLI when it stops working. |

## 5. Results

- 2026-10-27, `android` scripts on a Linux devbox (size M, through a temporary
  Hostel server, machine `spec.product = "devbox"`, `platforms = ["android"]`):
  - `devbox create` 9 s; room provisioning 22 s; `android` provision 166 s (2
    Ubuntu packages, the SDK, 2 golden AVDs); `prepare` 6 s. `acquire` returned
    after 212 s.
  - `adb devices` in a job shows `emulator-5554` (Android 16, x86_64);
    `hostel screenshot` gave a 1080×2424 PNG.
  - A setting changed in the first lease was gone in the next lease on the warm
    room (cleanup and `prepare`: 10 s). The devbox was deleted after
    `warm_running`; the key files were gone.
- 2026-10-27, `android` scripts on the MacBook (static room, `tools = { java =
  "21" }`): the first provision took 12 minutes (Java, about 6 GB over the home
  connection, 2 golden AVDs); `prepare` 3 s; Android 16, arm64-v8a; a
  screenshot. Free memory during the lease: about 60 MB of 8 GB. Cleanup 3 s; no
  emulator remained.
- 2026-10-27, `instance` product: E2E tests with the fake `nsc` CLI pass
  (`TestNamespaceInstance`, `TestNamespaceInstanceKill`,
  `TestNamespaceInstanceLimit`, `TestNamespaceInstanceLogin`).
- 2026-10-27, defect found by `TestNamespaceInstanceKill` (2 failures in 8 to 40
  runs) and by the first real smoke runs: a room step blocked without an error.
  A Go stack dump showed two goroutines of the SSH client in `Close` of the same
  connection; each stopped the `port-forward` process and waited for it, and the
  second wait on the same process blocks forever. The devbox proxy connection
  had the same pattern. Fix: `Close` stops the process once. After the fix: 40
  of 40 runs passed.
- 2026-10-27, smoke test `TestSmokeNamespaceInstance` against the real account
  (instance `linux/amd64:2x4`, default image `ubuntu:24.04`): passed. The lease
  was ready after 93 s (create, room container with OpenSSH from `apt-get`,
  room provisioning); the room user `hostel` on Ubuntu; a tunnel from the
  instance to a guest HTTP server; `kill -9` of the server and a restart during
  the lease; release; the instance was destroyed. Total 1 min 59 s. `nsc list`
  was empty before and after.
- 2026-10-27, done-when check: an Android app on a Namespace instance
  (owner's server, machine `namespace/android`, `linux/amd64:8x16`, default image,
  `tools = { java = "21" }`), against the API of the app's worktree on the Mac
  through the tunnel:
  - `acquire android --require kvm` returned after 351 s: `nsc create` and the
    room container 43 s, room provisioning 43 s, `android` provision 3 min 45 s
    (packages, SDK, 2 golden AVDs), `prepare` 7 s, then the sync (3,347 files)
    and `mise install` (Java, pnpm, tuist for Linux).
  - Cold `./gradlew installDebug`, with the app's API URL property set to
    `http://10.0.2.2:$HOSTEL_TUNNEL_API_PORT`: 2 min 31 s.
  - The app started; the sign-in with a test account went through the tunnel to
    the local API; the app showed its first screen after sign-in, in two
    screenshots.
- 2026-10-27, defect found in that check: after the release, `health` failed
  with exit code 127: `emulator -version` starts QEMU, which needs
  `libpng16.so.16`, and `ubuntu:24.04` does not have it (the emulator itself ran;
  the devbox image has the library). The room was destroyed as broken. Fix:
  `provision` installs `libpng16-16t64` and fails when `emulator -version` fails.
  Checked on a new instance: lease ready after 264 s; cleanup with `health`
  passed.
- 2026-10-27, review of the pull request (a subagent). Fixed:
  - The android scripts took the slot index from the first letter of the slot
    name, so two slots could get the same console port. Now the port is
    `HOSTEL_ANDROID_PORT` (slot or machine env, default 5554) + 2 × device index.
  - A create was "limit reached" when its error text contained "timed out";
    now only Hostel's deadline (a sentinel error) is.
  - The room container start had 1 minute, with the image pull; now 10 minutes.
  - After a create, other instances with the room's name (from a killed
    server's call) are destroyed.
  - CLI processes have a `WaitDelay`, so a child process that keeps a pipe open
    cannot block `Wait`.
  - A provision that fails during the golden boot stops its emulator; the next
    provision first stops an emulator that an earlier one left on its port.
  - The "not ready" message names the room, not a devbox.

  Not changed:
  - `Destroy` and `List` choose the product from the machine in the current
    configuration. A room of a machine that leaves the configuration is not
    destroyed by the server (as for all providers); `--duration` bounds the
    cost of an instance.
  - No E2E test calls `Extend` (above).
- 2026-10-27, `Extend` checked by hand on a real instance (2×4, `--duration
  10m`): `nsc instance extend-duration <id> --ensure_minimum 30m`, the command of
  the provider, exited with 0 and printed the new deadline 30 minutes ahead; an
  unknown ID exits with 1. The instance was destroyed at once.
- 2026-10-27, stability gate in CI on 6cdcd4d: `check` and `e2e` passed, and
  10 of 10 gate runs passed (9 min 39 s to 12 min 48 s each). Local macOS run of
  the whole suite (`make test-e2e`) on 6cdcd4d, load average about 3 at the
  start: passed in 534 s. (An earlier local run on 6cdcd4d gave a log that was
  byte-identical to the run on 95244b0, also the time 491.412 s; it is not
  counted.)
- 2026-10-27, review by Codex on the pull request. Fixed:
  - A late second instance with a room's name stayed while the room was active
    (reconcile accepted every listed room with the name). Reconcile now destroys
    a listed room whose provider ID is not the recorded one
    (`TestNamespaceInstance`; it fails on the old code). The fake gives each
    instance its own container.
  - The room start script installed nothing when the image had OpenSSH; now it
    installs each missing program (OpenSSH, sudo, git, curl). The test room image
    has sudo and curl.
  - The room start script kept an `authorized_keys` from the image; now it
    always writes only the server key.
- 2026-10-27, after the Codex fixes, on 98de7bd: CI and 10 of 10 gate runs passed
  (9 min 27 s to 13 min 56 s). Local macOS run: passed in 390 s (load average
  about 6 at the start). Two earlier local runs on 98de7bd failed only on
  timeouts while other projects raised the load to 214; they are not counted.
