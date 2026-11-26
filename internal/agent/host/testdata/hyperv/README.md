# Hyper-V fixtures

Fixtures for the host agent's worker tests. The mock `Hyper-V` module
(`../mock/Hyper-V`, Windows CI under pwsh) and the Go fake worker
(`fakeworker_test.go`, every platform) replay them with the same semantics.

- `<scenario>.json` is the host and its VMs at the start of a test:
  `computerName`, `totalBytes`, `overheadBytes` (host free memory is
  `totalBytes − overheadBytes − Σ assignedBytes`), optional
  `reportedFreeBytes` (what `Win32_OperatingSystem` reports instead), `vmms`
  (`Running` or `Stopped`), `hang` (cmdlets that never return) and `vms`
  (the properties of `Get-VM` / `Get-VMMemory`, plus `shrinkFloorBytes`, the
  lowest assignment the guest releases memory down to, and `startupDrift`,
  a refused shrink that still sets the startup value).
- `errors.json` holds the error records the cmdlets throw: message, error
  category, error ID and HResult.

**These fixtures are reconstructed, not captured.** The scenarios (`host`:
grows, Dynamic Memory, VM states; `refused-shrink`, `partial-shrink`,
`vmms-down`, `hang`, `out-of-memory`) and the error records, including their
messages, error IDs and HResults, are written from documented cmdlet
behaviour. None was recorded on a real Hyper-V host. They are meant to be
replaced by records from real hosts, exported by the host agent's
diagnostics (`GetDiagnostics`, DESIGN §4.2). Recordings can replace them
file by file as long as they keep this schema; messages are never parsed,
so recordings in any language work.
