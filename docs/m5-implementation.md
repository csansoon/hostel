# M5 implementation plan

Status: in progress (2026-10-27). Milestone M5 in `plan.md`: the server on an
exe.dev VM; a guest on the owner's Mac uses the home MacBook through it (owner
decisions, 2026-10-27). This file gives the facts, the decisions, the build order,
and the risks. `design.md` stays the contract.

## 1. Facts from the experiments (2026-10-27)

exe.dev, the owner's account (personal plan), test VM `hostel-test`
(`ubuntu:24.04`, 2 CPUs, 7 GB).

- **Create.** `ssh exe.dev new --name=<vm> --image=ubuntu:24.04 --no-email --json`
  gives the VM in a few seconds, with `https://<vm>.exe.xyz` and `ssh <vm>.exe.xyz`
  (user root). A value with spaces must be quoted for the remote shell.
- **Network.** Each VM has its own public IP, but the VM itself has only a private
  address (`10.42.0.42`). Raw inbound TCP (a listener on port 2222) is not
  reachable. Port 22 of the hostname is the exe.dev SSH front end, not the VM's.
- **HTTPS proxy.** exe.dev terminates TLS and forwards to one VM port
  (`share port <vm> <port>`). A private VM redirects every request to an exe.dev
  login; `share set-public <vm>` makes it public. The proxy adds
  `X-Forwarded-For`.
- **WebSocket.** Through the public proxy, a WebSocket upgrade returns 101 after
  208 ms, and raw bytes then go both ways: a 32-byte echo in 35 ms, a 1 MB echo in
  542 ms, and an echo after 70 s of idle time.
- **Idle WebSocket.** Through the proxy, a WebSocket stayed open for 15 minutes
  without traffic (checks after 2, 7, and 15 minutes).
- **Images.** `ubuntu:24.04` runs without systemd (PID 1 is `exe-init`); the
  default image (`exeuntu`) runs systemd, with the user `exedev` and `sudo`; its
  proxy port is 8000.
- **Plan.** Personal plan: 2 vCPU, 4 GB, 100 GB for $15 a month; up to 50 VMs in
  that capacity.

## 2. Decisions from the facts

- **Gateway over WebSocket.** The gateway also accepts SSH inside a WebSocket on
  the API listener (`/v1/gateway`). `gateway_public` can be a `wss://` URL; guests
  and `hostel room connect` then dial the WebSocket and run SSH over it. The raw
  TCP gateway stays for local servers and the E2E tests. One public port serves
  the API and the gateway.
- **TLS.** exe.dev terminates TLS; the server listens on plain HTTP on the VM. For
  another VPS, `deploy-vps.md` shows Caddy in front of the server. No TLS code in
  Hostel.
- **`hostel room connect`.** As in `design.md` section 13: the room keeps an
  outbound SSH connection to the gateway (user `room.<provider>.<machine>`, a
  `room` token as the password); the gateway opens streams to the room's local SSH
  server through it, and the static provider dials through it for `connect = true`
  machines. The macOS service is a launchd agent; Linux, a systemd unit.
- **Release builds.** A tag builds the binaries (`make dist`) with checksums into a
  GitHub release, plus an `install.sh`. A public repository makes the install a
  plain download; with a private one, the install needs a GitHub token (`gh`).

## 3. Build order

1. Gateway over WebSocket (server, guest, config), with E2E tests.
2. `hostel room connect` and `connect = true` for static machines, with E2E tests
   (a Docker room without inbound SSH from the test host).
3. Server deployment on an exe.dev VM: `docs/deploy-vps.md` (install, config,
   service, public share, backups of the data directory).
4. Release workflow and `install.sh`.
5. Headless `devbox` and `nsc` login on Linux (research and a check on the VM).
6. Done-when: a guest on the owner's Mac uses the home MacBook (`room connect`)
   through the server on exe.dev.
7. Gate: `e2e-10` in CI, one local macOS run, `pr-ready`.

## 4. Risks

| Risk | Plan |
|---|---|
| The exe.dev proxy closes long idle WebSockets. | SSH keepalives on gateway connections; measure a long idle time. |
| A public API without Hostel's own TLS. | Only exe.dev's TLS terminates; the server listens only on the VM. Tokens stay the only access control, as before. |
| exe.dev changes its proxy. | The raw TCP gateway stays available; Caddy on another VPS is documented. |

## 5. Results
- 2026-10-27, gateway over WebSocket (`TestGatewayWebSocket`) and `hostel room
  connect` (`TestRoomConnect`, over the WebSocket gateway) pass.
- 2026-10-27, server on the exe.dev VM `hostel` (`exeuntu`, systemd service,
  `listen = "0.0.0.0:8000"`, `gateway_public =
  "wss://hostel.example.com/v1/gateway"`, public share): the API answers on
  `https://hostel.example.com`; admin commands work from the owner's Mac. The home
  MacBook runs `hostel room connect` as a launchd agent; it connected over the
  WebSocket gateway, and the server probed it and uploaded the helper through
  that connection (room `running`).
- 2026-10-27, first done-when try: an `ios` lease from a guest on the owner's Mac
  became ready on the MacBook (`prepare` 13 s), but the guest's long-poll ended
  with "connection reset by peer", and the lease expired after the idle timeout.
  The owner's Mac was in clamshell sleep at that time (`pmset -g log`), which
  explains the reset. Open: measure long-polls through the proxy with the Mac
  awake.
- 2026-10-29, fixes from the first try: a waiting guest retries its poll after a
  network error or a proxy error (502, 503, 504) until its deadline
  (`TestPollNetworkError`); the server probes an unreachable room at once when
  it connects (`TestRoomConnect` with reconcile only at the start). Both tests
  fail without the fix.
- 2026-10-29, done-when passed: from the owner's Mac (awake), an `ios` lease
  through `https://hostel.example.com` became ready on the home MacBook in 25 s
  (sync, setup, `run`, the `api` tunnel to a local HTTP server, and a
  screenshot). A second lease waited in the queue for 130 s (three long-polls of
  50, 50, and 30 s through the proxy, no error) and got the slot after the first
  was released.
- 2026-10-29, found by that handoff: the ios `reset` killed the simulator's
  `launchd_sim` (its command line has the UDID) before `simctl shutdown`, and
  about 270 simulator processes of each lease stayed as orphans. After about
  four leases, the room user reached its process limit, and `prepare` failed
  ("insufficient system resources", code 67). Since M3. Fix: shut down first,
  then kill the processes with the UDID, then shut down again. Check on the
  MacBook: three lease cycles through the VM, 0 simulator processes after
  each. The old orphans on the MacBook were removed by hand.
- Notes for `deploy-vps.md`: make archives on macOS with `COPYFILE_DISABLE=1`
  (else AppleDouble `._*` files break the platform check); a second server key
  for a static room must go into `authorized_keys` and
  `~/hostel/authorized_keys.base` (cleanup restores the file from the base).
