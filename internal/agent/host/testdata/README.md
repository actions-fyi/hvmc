# Host agent test fixtures

This directory holds recorded Hyper-V behaviour for the host agent's tests: the error classification (DESIGN §4.2) and the mock Hyper-V module that stands in for `Get-VM`, `Get-VMMemory` and `Set-VMMemory` in CI. The fixtures come from spike S1 ([`docs/spikes/s1-hyperv.md`](../../../../docs/spikes/s1-hyperv.md)). Until a contributor has run S1 on a real host, the directory holds only this file.

## Layout

```
testdata/
  s1/<osBuildNumber>-<guest>-<yyyymmdd>.json   one S1 run each, as written by the script
  synthetic/<scenario>.json                    hand-made cases S1 cannot record (optional)
```

- `<osBuildNumber>` is `host.osBuildNumber` from the file, for example `20348`.
- `<guest>` is the guest OS family and version, for example `windows-2022` or `linux`. The script does not detect it, so state it in the file name.
- `<yyyymmdd>` is the date of the run.

## Format

Every file under `s1/` is unmodified output of [`hack/hyperv-characterize.ps1`](../../../../hack/hyperv-characterize.ps1) and validates against [`hack/hyperv-characterize.schema.json`](../../../../hack/hyperv-characterize.schema.json) (`schemaVersion` 1). The fields are explained in the spike document under "Output". What the consumers use:

| Consumer | Uses |
|---|---|
| Error classification tests (`internal/agent/host`) | For each step with `result: failed`: `errors[].fullyQualifiedErrorId`, `errors[].exception.type`, `errors[].exception.hResult`, `errors[].innerExceptions[]` and `errors[].category.category`, mapped to the expected gRPC code. The step `name` says which case it is (`shrink-refused` → `Aborted`, and so on). Do not classify on `message`, because messages are localized. |
| Mock Hyper-V module (Windows CI) | For each `Set-VMMemory` step: given `before` and `targetBytes`, produce `settled` (assigned and startup after the call, including `startupDiverged`) and, if `result` is `failed`, throw an error record with the recorded id, exception type, message and category. `durationMs` gives realistic delays. `observed` names the behaviour to replay: `applied`, `refused`, `partially-applied` or `succeeded-not-applied`. |
| Mock objects | `baseline.vm`, `baseline.vmMemory` and `baseline.operatingSystem` list each property's .NET type and value, so mock objects have the same property names and types as the real ones. |
| Guest verification tests | `baseline.guest.offsetBytes` and `steps[].guest.reflectedAfterMs` for realistic offsets and delays (§4.4). |
| Service account | `serviceAccountProbe.result.operations[]` documents which cmdlets the virtual account may run; it is evidence for DESIGN §4.1 rather than test input. |

Placeholders are always the same: VM name `vm-under-test`, VM ID `00000000-0000-4000-8000-000000000001`, host name `hyperv-host`. Tests may rely on them.

## Adding an S1 run

1. Run the script on your own Hyper-V host against a disposable, opted-in VM (see the spike document).
2. Check `anonymization.leakCheck` is `passed`, then read the whole file. Error messages are verbatim apart from the placeholders. If they contain anything else that identifies your installation (a domain, a path, a user name), replace it with a generic value and say so in the commit message. Change nothing else.
3. Validate it: `Test-Json -Path <file> -SchemaFile hack/hyperv-characterize.schema.json`.
4. Copy it to `s1/` under the naming scheme above and fill the Results section of `docs/spikes/s1-hyperv.md` from it.

## Synthetic fixtures

Some cases cannot be recorded safely by S1: VMMS down, host out of memory, a hung call. If tests need them, put hand-made files under `synthetic/`. Use the same `errors[]` element shape as the schema's `errorInfo`, and add a top-level `"synthetic": true` with a `"source"` string that says where the shape came from (a manual capture on a lab host, or documentation). Never put synthetic data under `s1/`.
