# M12. Azure provider (plan)

Status: implemented. Owner decisions of 2026-11-18 (section 7: the recommended option of each).
Section 8 has what changed from the plan, and the limits.

## 1. Goal

An owner has a virtual machine in Azure (for example a Windows desktop). The machine
costs money while it runs. Hostel starts it when a lease needs it and deallocates
it when it has been idle for a set time. The owner keeps the machine in the Azure
portal: its image, disk, network, and the Hostel setup on it (`hostel machine
join`). Hostel does not create or delete Azure machines in this milestone.

## 2. Configuration

A new provider kind `azure`. Its entries are existing machines, as for `static`:
each entry under `machines` is one machine, with the same keys (`os`, `tools`,
`rooms`, `env`, `address` or `connect = true`, `lease_users`, ...), and these
keys:

```toml
[providers.cloud]
kind = "azure"
label = "Azure"
tenant = "<directory (tenant) ID>"
client_id = "<application (client) ID>"

[providers.cloud.machines.win]
vm = "/subscriptions/<id>/resourceGroups/<group>/providers/Microsoft.Compute/virtualMachines/<name>"
os = "windows"
connect = true
tools = ["windows-desktop"]
warm_running = "30m"   # idle time before Hostel deallocates the machine (default)
```

- `vm` is the resource ID of the machine (Azure portal, the machine's "JSON view").
- `tenant` and `client_id` are not secret. The client secret is not in the
  configuration document (members can read the document). It is on the server
  only: `hostel config providers login <provider>` asks for it and sends it to the
  server, which keeps it in `data_dir` with access for the server's user only. It
  never goes to a client, a log, or an event.
- `warm_running` exists already for on-demand templates (design, section 9). M12
  also allows it on an Azure machine: the idle time before the deallocation.
  `warm_stopped` does not apply (section 3).

## 3. Behaviour

- **States.** The machine uses the existing states `stopping`, `stopped`, and
  `resuming`. A stopped Azure machine is not destroyed after `warm_stopped`
  (Hostel did not create it); it stays `stopped` until a lease needs it.
- **Start.** When the scheduler assigns a lease to a stopped Azure machine (the
  lease needs its tools, and no running machine is free), the machine goes to
  `resuming`. Hostel calls `start` and waits for the Azure operation. Then the
  machine connects to the server (`connect = true`) or answers on SSH
  (`address`), and the normal provisioning runs (probe, helper, tools). The
  lease is `provisioning` (design, section 4: "the machine is in creation or
  resume").
- **Stop.** A running machine with no lease for `warm_running` goes to
  `stopping`: Hostel calls `deallocate` (not `powerOff`: a stopped machine that
  is not deallocated is still billed) and waits for the operation.
- **At server start and reconcile.** Hostel reads the power state of each Azure
  machine (`instanceView`). A machine that someone started in the portal is
  probed and used. A machine that someone deallocated in the portal is
  `stopped`. A lease on a machine that went away is queued again, as for a static
  machine that becomes unreachable.
- **Drain.** A drained Azure machine takes no new lease. A drained stopped
  machine is not started.
- **Failures.** A failed start or stop is an event with the Azure error code and
  message (no secret). After a failed start, the machine is `broken` (reason
  "start failed"); `hostel config machines repair` tries again. A failed stop is
  tried again at the next pass (a machine that keeps running costs money).

## 4. Azure API

Pure Go, with `net/http` (no Azure SDK, no `az` CLI on the server):

- Token: `POST https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token`,
  client credentials, scope `https://management.azure.com/.default`. The token
  is kept until shortly before it expires.
- `POST <vm>/start`, `POST <vm>/deallocate`, `GET <vm>/instanceView`, on
  `https://management.azure.com`, with a pinned `api-version`. A start or a
  deallocate answers `202` with an `Azure-AsyncOperation` URL; Hostel polls it
  until it ends.
- Deadlines: 30 s for each HTTP request, and the provider call deadline (5 minutes) for a start or a deallocate. A start that takes longer is a failed start; when Azure then reports the machine running, the reconcile probes it.
  Retries only for `429` and `5xx`, with backoff and the `Retry-After` header,
  inside the deadline.
- The base addresses can be changed in the provider (`login_url`,
  `management_url`), for the E2E tests and for national clouds.

## 5. Permissions (machine guide)

The guide (`machine-setup.md`, "Azure") tells the owner to:

1. Register an application in Microsoft Entra ID and make a client secret.
2. Give the application a custom role on the machine only (not on the
   subscription), with these actions: `Microsoft.Compute/virtualMachines/read`,
   `Microsoft.Compute/virtualMachines/start/action`,
   `Microsoft.Compute/virtualMachines/deallocate/action`, and
   `Microsoft.Compute/locations/operations/read`.
3. Add the provider and the machine to the configuration, and run `hostel config
   providers login <provider>`.
4. Optional, recommended: an Azure auto-shutdown schedule on the machine, as a
   limit when the Hostel server is down.

## 6. Tests

- E2E (the gate): a fake Azure API, an HTTP server in the test harness (the
  adapter boundary: `AGENTS.md` lets tests mock third-party services). The
  provider uses it through `login_url` and `management_url`, and the machine is
  a Docker machine that the fake "starts" and "deallocates" (start and stop of
  the container). Scenarios:
  - a lease starts a deallocated machine and runs on it;
  - after `warm_running` without a lease, the machine is deallocated;
  - a second lease starts it again, and the tools are not installed again when
    nothing changed;
  - a machine that someone started outside Hostel is used, and one that someone
    deallocated is `stopped`;
  - a failed start makes the machine `broken`, with the Azure message in the
    events and no secret;
  - the secret is not in the configuration document, `hostel config`, the
    events, or the server log;
  - a drained stopped machine is not started.
- Smoke test (opt-in, costs money, `HOSTEL_SMOKE_AZURE=1`): against a real
  Azure machine: start, a lease, deallocation after a short `warm_running`.

## 7. Owner decisions

1. **Scope.** Recommended: start and deallocate existing machines only (this
   plan). Creating machines from an image (on demand, with `max_machines`) is a
   later milestone: it needs networks, disks, a prepared image with Hostel
   already joined, and a provider-side cost bound.
2. **Authentication.** Recommended: an application (service principal) with a
   client secret, kept on the server, and a custom role on the machine only.
   Other options: a certificate instead of a secret; the `az` CLI logged in on the
   server (one more tool on the server); a managed identity (only when the Hostel
   server runs in Azure).
3. **Idle time.** The default for `warm_running` on an Azure machine: 30
   minutes. A start of a Windows machine takes about 1 to 3 minutes, plus the
   probe.
4. **Backstop.** Recommended: the owner sets an Azure auto-shutdown schedule on
   the machine (for example each night), so that the machine stops also when the
   Hostel server is down. Hostel does not manage the schedule.

## 8. Implementation notes and limits

- **Join.** A new Azure machine joins with a code, as a static machine:
  `hostel config providers add <name> --kind azure --tenant <ID> --client-id
  <ID>`, `hostel config providers login <name>`, then `hostel config machines
  add <machine> --provider <name> --vm <resource ID>`. A `static` provider can
  also become `azure` in place (add the keys): the machine rows and machine
  tokens stay.
- **Failed start.** The machine stays `broken` ("start failed: ...") and is not
  deallocated by `warm_running`: else it would be `stopped` and the next lease
  would start it again without end. `hostel config machines repair` makes it
  `stopped`. When Azure reports it running (for example a start in the portal),
  the reconcile probes it.
- **Broken for another reason** (cleanup failed, host key changed, another OS):
  deallocated after `warm_running`, and still `broken` ("deallocated: ...")
  until a repair.
- **Review fixes (2026-11-19).** A deallocated machine with changed settings is
  not stale (it provisions at its next start); one with a dirty room is started
  when leases wait; a running azure machine is not removed and its `vm` does not
  change; Azure "stopped" (still billed) is deallocated at once; a `vm` that
  Azure does not find is a failed start; the reconcile has a 1 minute deadline
  and one wrong `vm` does not stop it for the others; the secret goes with its
  provider; the fake API ends a start before SSH answers, as Azure does.
  Second review: no start loop for a machine that never answers (above); the
  dirty-room start only for a lease that the machine can serve; the state
  changes that a configuration check depends on take the scheduler lock; the
  removal guard accepts only `stopped` and "deallocated: ".
- **Failed stop.** The machine is `broken` ("stop failed: ..."): it takes no
  lease, and `warm_running` tries the deallocation again.
- **Unreachable machine.** A machine that Azure reports deallocated or stopped
  is `stopped` at the next reconcile (a stopped one is deallocated first). A
  machine that Azure reports running but that stays unreachable for
  `warm_running` is deallocated and stays `broken` ("deallocated:
  unreachable") until a repair: else each waiting lease would start it again
  and pay for it without end. A failed stop of a broken machine keeps its first
  reason, with "(stop failed: ...)", and `warm_running` tries again.
- **Login check.** `providers login` stores the secret first, then tries it
  (`List`); the command fails with the Azure message (its first line only) when
  Azure refuses it.
- **Base addresses.** `login_url` and `management_url` take `http://` only on a
  loopback host (the E2E fake); the client secret goes to `login_url`.
- **Smoke test.** `make test-smoke-azure` (`test/e2e/smoke_azure_test.go`):
  the environment variables are in the test comment. The machine must be
  deallocated at the start; the test deallocates it also when it fails.
- **Azure CLI sign-in** (owner decision, 2026-11-23). `auth = "cli"`: the
  provider gets the token from `az account get-access-token` on the server
  (the account of `az login` as the server's user). For an owner who cannot
  register an application. `TestAzureCLI` uses a fake `az`;
  `HOSTEL_SMOKE_AZURE_AUTH=cli` runs the smoke test with the local `az` sign-in.
- **Not done.** Creating machines from an image; a certificate or a managed
  identity instead of a client secret; the auto-shutdown schedule (the owner
  sets it in the portal).

- **Review of the pull request** (2026-11-24): a machine that stops answering
  near the end of its idle time is deallocated only after `warm_running`
  unreachable (counted from the change to `broken`), not at once; the client
  secret is removed when the sign-in settings change; no change of `kind` while
  a start or a deallocation runs; a connecting stopped machine changes state
  under the scheduler lock.

## 9. Evidence (2026-11-19)

- `make lint` passes.
- `TestAzure` and `TestAzureOutside` (fake Azure API) pass.
- The 10-run gate (local, `make test-e2e-10` in a detached loop): 10 of 10
  passed, 397 s to 654 s each. Two earlier tries stopped on tests that M12
  did not change, at load averages of 30 to 66 (`TestTrafficActivity`, idle
  timeout; `TestStaticOffline`, an SSH `EOF` that is not counted as
  unreachable). `TestAcquireBackground` now uses `poll_timeout = "12s"` (its
  keeper missed 6 s at a load average of 34).
- The smoke test against a real Azure machine did not run on 2026-11-19: it
  needed the owner's application and client secret.
- 2026-11-23: `TestAzureCLI` passes. The smoke test against a real Azure
  Windows machine passed with `HOSTEL_SMOKE_AZURE_AUTH=cli` (341 s): the lease
  was ready 3 min 9 s after `acquire` (start of the deallocated machine and
  provisioning), and Hostel deallocated the machine after `warm_running`
  (1 min). The client-secret sign-in was not tried against real Azure.
