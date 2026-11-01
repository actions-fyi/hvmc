# Spike S1: Hyper-V characterization

**Status:** the script is ready but has not yet run on a real Hyper-V host. Fill in [Results](#results) from the first run.

The host agent drives Hyper-V through a long-lived `pwsh` worker (DESIGN §4.2) and has to turn whatever `Set-VMMemory` does into typed results and errors. S1 records what `Set-VMMemory -StartupBytes` really does on a running static-memory VM, so that the agent's error classification and its CI mock of the Hyper-V module are built on recorded behaviour instead of guesses. It answers:

| Question | Feeds |
|---|---|
| What error records (`FullyQualifiedErrorId`, exception type and message, category, inner exceptions) do a refused shrink, an unaligned size and a missing VM produce? | error classification (§4.2), mock module |
| What are `MemoryAssigned` and `MemoryStartup` after a grow, a shrink, a refused shrink and a partial shrink? Does a failed shrink change `MemoryStartup` even though the assignment stays the same? | shrink path and repair (§3.2, §3.5) |
| Does the coordinator's midpoint retry (§4.2) reach a size the guest accepts? | shrink retry rule |
| How long does each cmdlet call take? Do values change after the cmdlet has returned? | worker timeout (§4.2), `WatchVMs` polling |
| How long until the guest's `GlobalMemoryStatusEx` total reflects a resize, and what is the offset between assigned and guest total? | grow verification (§4.4) |
| Can a virtual service account in *Hyper-V Administrators* run `Get-VM`, `Get-VMMemory`, `Get-CimInstance Win32_OperatingSystem` and `Set-VMMemory`? | service account (§4.1, §4.5) |

Files:

- [`hack/hyperv-characterize.ps1`](../../hack/hyperv-characterize.ps1) is the script.
- [`hack/hyperv-characterize.schema.json`](../../hack/hyperv-characterize.schema.json) is the JSON Schema of its output.
- [`hack/tests/`](../../hack/tests/) holds Pester tests for everything that does not need Hyper-V.
- [`internal/agent/host/testdata/`](../../internal/agent/host/testdata/README.md) is where reviewed results go as fixtures.

## Prerequisites

- A Windows Server 2016 or later host with the Hyper-V role (runtime memory resize needs 2016+) and PowerShell 7 (`pwsh`).
- A session whose user is in *Hyper-V Administrators* or is an Administrator. `-ServiceAccountProbe` needs an elevated session.
- A **disposable** VM that runs nothing you care about, with:
  - static memory (Dynamic Memory off), running, with `MemoryStartup` equal to `MemoryAssigned`;
  - a Windows guest, which is the case that matters to hvmc. A Linux guest also works, but it cannot shrink at runtime (DESIGN §1), so every shrink step records a refusal; label such a fixture as a Linux guest;
  - a `MemoryStartup` well above the refused-shrink target (default 256 MiB); 2 GiB or more is sensible;
  - a distinctive name (for example `hvmc-s1-disposable`). Anonymisation replaces the name wherever it appears as a separate word, so a VM called `Memory` would also replace the word "Memory" in error messages.
- Host free memory of at least `StepMiB + LargeStepMiB + HostMarginMiB` (5376 MiB with the defaults).

## Opt-in marker

The script refuses to touch a VM unless its Hyper-V **Notes** contain `hvmc-probe` as a separate word. The match is case-sensitive, so `no-hvmc-probe` and `hvmc-probes` do not count. To add the marker while keeping the existing notes:

```powershell
$vm = Get-VM -Name hvmc-s1-disposable
Set-VM -VM $vm -Notes (($vm.Notes, 'hvmc-probe') -join "`n").Trim()
```

Remove the marker when you are done. The script never records the content of Notes, only whether the marker was found.

## What the script changes and restores

It changes only `MemoryStartup` of the opted-in VM, with `Set-VMMemory -VMName <name> -StartupBytes <n>`, the same call shape the host agent uses. With B = `MemoryStartup` at the start, S = `-StepMiB` and L = `-LargeStepMiB`:

| # | Step | Target | Expected |
|---|---|---|---|
| 0 | `get-vm-missing` | `Get-VM -Id 00000000-0000-4000-8000-0000000000ff` (read only) | refusal (not found) |
| 1 | `set-unaligned` | B + 1 MiB (not 2 MiB aligned) | refusal (invalid argument) |
| 2 | `grow-step` | B + S | success |
| 3 | `grow-large` | B + S + L | success |
| 4 | `shrink-to-baseline` | B | success |
| 5 | `shrink-refused` | `-RefuseTargetMiB` (below what the guest can release) | refusal |
| 6 | `shrink-partial-1..3` | midpoint between the last refused target and the current assignment, rounded up to S (or to 2 MiB); stops at the first success | any |
| 7 | service account probe | B' + S and back, where B' is `MemoryStartup` at that moment (only with `-ServiceAccountProbe`) | success |
| 8 | `restore-1`, `restore-2` | B, always, from a `finally` block (also on Ctrl+C), retried once | success |

Safety checks run before anything is changed. Every check is printed, and if any fails the script exits with code 2 without changing anything or writing a file:

- `hyperv-available`: Windows, and `Get-VM`, `Get-VMMemory` and `Set-VMMemory` resolve to the Hyper-V module (not, for example, a VMware module).
- `vm-unique`: exactly one VM has the exact name. Wildcards are rejected at argument validation.
- `vm-running`, `static-memory`, `opt-in-marker`.
- `startup-matches-assigned`: equal and 2 MiB aligned. If they differ, an earlier resize left the VM inconsistent and "the original `MemoryStartup`" would be ambiguous.
- `refuse-target`: at least 32 MiB, 2 MiB aligned and below B.
- `host-free-memory`: host free memory (`Win32_OperatingSystem.FreePhysicalMemory`) is at least S + L + `-HostMarginMiB`.
- With `-GuestCheck`: `guest-credential`. With `-ServiceAccountProbe`: `elevated`, `probe-service-absent` and `probe-dir-absent`.

Before **every** write the script checks again that the name still resolves to exactly the same VM ID, that the VM is still Running, and that the growth still leaves the host margin. If any of these fails, the sequence is aborted and the restore still runs.

While the refused shrink runs, the guest can come under memory pressure for a moment. That is why the VM must be disposable. If the restore fails (exit code 3), the script prints the command that puts the original value back:

```powershell
Set-VMMemory -VMName <name> -StartupBytes <original bytes>
```

## Running it

```powershell
# 1. See the checks and the planned targets; changes nothing, writes nothing.
pwsh -NoProfile -File hack/hyperv-characterize.ps1 -VMName hvmc-s1-disposable -DryRun

# 2. The run itself.
pwsh -NoProfile -File hack/hyperv-characterize.ps1 -VMName hvmc-s1-disposable -OutDir .\s1

# 3. With guest timing (Windows guest, PowerShell Direct) and the service account probe (elevated).
pwsh -NoProfile -File hack/hyperv-characterize.ps1 -VMName hvmc-s1-disposable -OutDir .\s1 `
    -GuestCheck -GuestCredential (Get-Credential) -ServiceAccountProbe
```

| Parameter | Default | Meaning |
|---|---|---|
| `-VMName` | (required) | Exact VM name, no wildcards. |
| `-OutDir` | `./hvmc-s1` | Directory for the result file. |
| `-StepMiB` | 256 | Resize step (hvmc's `step`). Multiple of 2. |
| `-LargeStepMiB` | 1024 | Extra growth of the second grow step. Multiple of 2. |
| `-RefuseTargetMiB` | 256 | Target of the shrink the guest should refuse. |
| `-HostMarginMiB` | 4096 | Host memory that must stay free (hvmc's default `reserve`). |
| `-SettleSeconds` | 5 | After each call, sample the VM every 0.5 s this long to see late changes. |
| `-GuestCheck`, `-GuestCredential` | off | Time until the guest total reflects each resize (PowerShell Direct; Windows guests). |
| `-GuestTimeoutSeconds` | 60 | Guest wait limit (hvmc's verification timeout). |
| `-ServiceAccountProbe` | off | See [below](#service-account-probe). |
| `-DryRun` | off | Checks and plan only. |

Exit codes: 0 completed (or dry run passed), 1 aborted, 2 refused by a safety check, 3 `MemoryStartup` not restored.

Tests of the parts that do not need Hyper-V (PowerShell 7.4+ and Pester 5, on any OS):

```sh
pwsh -NoProfile -Command "Invoke-Pester -Path hack/tests -Output Detailed"
```

### Service account probe

The host agent runs as the virtual account `NT SERVICE\hvmc-agent`, a member of *Hyper-V Administrators* (DESIGN §4.1). A virtual account exists only while a service of that name exists, so the probe uses a temporary service. With `-ServiceAccountProbe` the script:

1. Creates `%ProgramData%\hvmc-s1-probe\` with `probe.ps1` and `input.json` (VM name and step).
2. Creates the service `hvmc-s1-probe` (manual start) with binary `"<this pwsh.exe>" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "...\probe.ps1"`, the same process shape as the agent's worker, then runs `sc.exe config hvmc-s1-probe obj= "NT SERVICE\hvmc-s1-probe"`.
3. Grants that account Modify on the work directory and adds it to *Hyper-V Administrators*. The group is resolved from its well-known SID `S-1-5-32-578`, so localized group names work.
4. Runs `sc.exe start hvmc-s1-probe`. Because `pwsh` is not a service binary, the Service Control Manager reports a start timeout (1053, after about 30 s) or an early exit (1067). That is expected and recorded. The probe writes its result after every operation, so a probe killed at the timeout still leaves what it did.
5. The probe records its identity (`isAdministrator`, whether the token holds the *Hyper-V Administrators* SID) and runs `Import-Module Hyper-V`, `Get-VM`, `Get-VMMemory`, `Get-CimInstance Win32_OperatingSystem`, `Set-VMMemory` to grow by one step and `Set-VMMemory` back, each with duration and full error records.
6. Cleans up in a `finally` block: kills the probe process if it is still running, removes the group membership, deletes the service and removes the work directory. Then it verifies all of that (`cleanupVerified`).

If a run is interrupted hard (power loss, killed console), clean up by hand from an elevated prompt. Remove the membership **before** deleting the service, while the account name still resolves:

```powershell
$group = ([Security.Principal.SecurityIdentifier]'S-1-5-32-578').Translate([Security.Principal.NTAccount]).Value.Split('\')[-1]
net.exe localgroup "$group" "NT SERVICE\hvmc-s1-probe" /delete
sc.exe delete hvmc-s1-probe
Remove-Item -Recurse -Force "$env:ProgramData\hvmc-s1-probe"
```

To probe by hand instead, run those steps yourself: create the service, set `obj=`, add the group membership, start the service, read `result.json`, then run the cleanup above.

## Output

Each run writes one file, `hyperv-characterize-<yyyyMMddTHHmmssZ>.json` (UTF-8, pretty-printed), to `-OutDir` and prints a summary table. The authoritative schema is [`hack/hyperv-characterize.schema.json`](../../hack/hyperv-characterize.schema.json) (JSON Schema 2020-12); `schemaVersion` changes when the format changes incompatibly. All sizes are bytes and all durations milliseconds.

| Field | Content |
|---|---|
| `schemaVersion`, `kind`, `script` | `1`, `hvmc.s1.hyperv-characterization`, script path and version. |
| `run` | `startedAt`, `finishedAt` (UTC ISO 8601), `outcome` (`completed`, `aborted`, `interrupted`), `abortReason`, `exitCode`. |
| `anonymization` | The placeholders used and `leakCheck` (`passed` or `failed`, see below). |
| `parameters` | The effective parameters, in bytes. |
| `invoker` | `isAdministrator`, `tokenHasHyperVAdministrators` of the user who ran the script. No user name. |
| `host` | OS caption, version and build, PowerShell version, Hyper-V module version, total and free memory. |
| `safety` | The checks that passed, as `{name, passed, detail}`. |
| `plan` | Every target derived from the baseline (`baselineBytes`, `growStepTargetBytes`, ...). |
| `baseline` | Shapes of the `Get-VM`, `Get-VMMemory` and `Win32_OperatingSystem` objects (type names, and per property its .NET type and value; enums as strings), the memory state, and with `-GuestCheck` the guest total and `offsetBytes` = assigned − guest total. |
| `steps[]` | One entry per step, see below. |
| `serviceAccountProbe` | `null` without `-ServiceAccountProbe`; otherwise setup actions, `serviceStart` (exit code and output of `sc.exe start`), the probe's `result` (`identity`, `operations[]` with `name`, `result`, `durationMs`, `summary`, `errors[]`), `error`, `cleanup` and `cleanupVerified`. |
| `restore` | `originalStartupBytes`, `needed`, `restored`, final startup and assigned, and `error`. |

Each entry in `steps[]`:

| Field | Content |
|---|---|
| `index`, `name` | Order and step name from the table above. |
| `operation`, `direction`, `expected` | `Set-VMMemory` or `Get-VM`; `grow`, `shrink` or `none`; `success`, `refusal` or `any`. |
| `targetBytes`, `startedAt`, `durationMs` | Requested size, start time, and the cmdlet's wall-clock duration. |
| `result` | `succeeded` or `failed` (the cmdlet threw). |
| `observed` | `applied` (succeeded, assignment equals target), `succeeded-not-applied`, `refused` (failed, assignment unchanged), `partially-applied` (failed, assignment changed); `read-ok`/`read-failed` for `get-vm-missing`. |
| `matchesExpectation` | Whether `result` matched `expected`. |
| `startupDiverged` | `MemoryStartup` ≠ `MemoryAssigned` after the step: the §3.5 repair case. |
| `before`, `after` | Memory state just before the call and right after it returns: `state`, `status`, `assignedBytes`, `startupBytes` (from `Get-VM`) and `vmMemoryStartupBytes` (from `Get-VMMemory`). |
| `settled` | State at the end of the settle window, with `changes[]` (`atMs` after the cmdlet returned) and `lastChangeAtMs`. |
| `errors[]` | Every distinct error record: `fullyQualifiedErrorId`, `exception` (`type`, `message`, `hResult`, plus `properties` with the exception's own scalar properties), `innerExceptions[]`, `category` (`category`, `activity`, `reason`, `targetName`, `targetType`), `errorDetailsMessage`, `targetObjectType`. |
| `warnings[]` | Warning messages the cmdlet wrote. |
| `guest` | With `-GuestCheck`, for steps that changed the assignment: `expectedTotalBytes` (assigned − offset), `toleranceBytes` (step / 2, as in §4.4), `reached`, `reflectedAfterMs` (after the cmdlet returned), `reflectedAfterCmdletStartMs`, `finalTotalBytes`, `polls`, `timeoutMs`, `error`. |

### Anonymisation

Before the file is written, every string in the document is rewritten:

- the VM name becomes `vm-under-test`;
- the VM ID becomes `00000000-0000-4000-8000-000000000001` (dashed and undashed forms, any case);
- the host's NetBIOS, DNS and FQDN names become `hyperv-host`.

Matching is case-insensitive and only on separate words, so `webserver` survives a VM called `web`. Afterwards the serialized JSON is searched for every original value, also in its JSON-escaped form. If one is still there, `anonymization.leakCheck` is `failed` and a warning is printed.

The script never records the VM's Notes, file paths, the invoking user's name or the `CSName`, `RegisteredUser` and `SerialNumber` properties of `Win32_OperatingSystem`. Error messages are recorded verbatim apart from the rewrites, so **read the file before committing it**.

How a reviewed result becomes a fixture is described in [`internal/agent/host/testdata/README.md`](../../internal/agent/host/testdata/README.md).

## Results

> Placeholder: replace with the values from the first real run and commit that run's JSON as a fixture. If the results contradict DESIGN, change DESIGN in the same PR.

Host: Windows Server _build_ · Hyper-V module _version_ · PowerShell _version_ · guest: _OS_ · VM generation/version: _..._

| Step | Duration (ms) | Observed | Startup diverged | `FullyQualifiedErrorId` | Exception type | Proposed gRPC code (§4.2) |
|---|---|---|---|---|---|---|
| `get-vm-missing` | | | n/a | | | `NotFound`? |
| `set-unaligned` | | | | | | `InvalidArgument`? |
| `grow-step` | | | | | | OK |
| `grow-large` | | | | | | OK |
| `shrink-to-baseline` | | | | | | OK |
| `shrink-refused` | | | | | | `Aborted` |
| `shrink-partial-1` | | | | | | |
| `shrink-partial-2` | | | | | | |
| `shrink-partial-3` | | | | | | |
| `restore-1` | | | | | | OK |

Late changes after the cmdlet returned (`settled.changes`): _none / describe_.

Guest reflection (`-GuestCheck`):

| Step | Offset (MiB) | Reflected after return (ms) | Reached within 60 s |
|---|---|---|---|
| `grow-step` | | | |
| `grow-large` | | | |
| `shrink-to-baseline` | | | |

Service account (`-ServiceAccountProbe`), token has *Hyper-V Administrators*: _yes/no_ · `sc.exe start` exit code: _..._

| Operation | Result | Duration (ms) | Error id |
|---|---|---|---|
| `Import-Module Hyper-V` | | | |
| `Get-VM` | | | |
| `Get-VMMemory` | | | |
| `Get-CimInstance Win32_OperatingSystem` | | | |
| `Set-VMMemory grow` | | | |
| `Set-VMMemory shrink back` | | | |

Conclusions for DESIGN: _§4.1 service account sufficient? §4.2 classification rules (which fields identify a refused shrink), §3.5 repair needed?, worker timeout adequate?_

## What S1 does not answer

- **VMMS down (`Unavailable`).** The script never stops `vmms`. That error shape has to be captured by hand on a lab host (`Stop-Service vmms`, run a read, `Start-Service vmms`; this disrupts every VM's management) or synthesized for the mock.
- **Host out of memory (`ResourceExhausted`).** The safety checks deliberately prevent growing beyond free memory, so this error shape is not recorded.
- **A hung cmdlet.** The worker's timeout and kill/restart path cannot be provoked safely; the mock has to simulate it.
- **Dynamic Memory** (`SetDynamicLimits`) is not exercised, and `fixed`-VM refusals are enforced by the agent, not by Hyper-V.
- **The worker framing.** The script runs the cmdlets in one ordinary `pwsh` process. It shows the error objects the worker will serialize, not the worker's length-prefixed JSON or its `Select-Object` projections.
- **Concurrent resizes** of different VMs (§3.2 allows them in parallel) are not tested.
- **Localization.** Messages are localized. Classification should rely on `FullyQualifiedErrorId`, exception type and HResult; one run on a non-English host would confirm which of these are stable.
- **Virtual account set-up details** that the real agent installer (§4.5) needs, such as whether `sc.exe config obj=` alone is enough or a service SID type must be set, are only checked as far as the probe gets.
- **Error codes outside DESIGN §4.2.** `NotFound` (missing VM) and `InvalidArgument` (unaligned size) are not in the §4.2 list. The mapping above is a proposal until the agent's error model (`proto/hvmc/agent/v1/common.proto`) exists.
