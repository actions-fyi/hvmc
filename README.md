# hvmc — Hyper-V Memory Coordinator

hvmc hands out RAM on a Hyper-V host as *memory leases*. A coordinator running in
Kubernetes keeps one ledger for every VM on the host, grows Windows VMs before a
workload starts, and makes requests wait when the host has no memory left. Pods ask
for memory with a single extended resource; clients outside Kubernetes use an HTTP
API. Small native agents on the Hyper-V host and in each managed VM do the actual
work.

**Status:** under construction — nothing is usable yet.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- License: [MIT](LICENSE)

## Development

Requires Go (the toolchain is downloaded automatically from `go.mod`) and GNU make.

```sh
make help       # list targets
make test lint  # what CI runs; test downloads envtest binaries into .envtest/
make generate   # deepcopy and CRDs (config/crd/bases, charts/hvmc) from api/v1alpha1
make helm-lint  # helm lint + golden tests of charts/hvmc (make helm-golden rewrites them)
make ct-lint    # chart-testing lint; needs python3 for yamllint and yamale
make build      # bin/hvmc
```

The Windows service round trip (`internal/agent/svc`) installs, upgrades and removes the real
`hvmc-agent` service, so it is built only with the `servicetest` tag and runs in its own step of
the Windows CI job. To run it yourself, use a disposable Windows machine without hvmc and an
elevated prompt; it fails, rather than skips, when it is not elevated or hvmc is already there:

```powershell
go test -tags servicetest -run '^TestServiceRoundTrip$' -count=1 -v ./internal/agent/svc/
```

The systemd round trip is the same test on Linux, in its own step of the Linux CI job. It needs root,
systemd as PID 1 and a disposable machine without hvmc; it installs a test build of hvmc (tag
`servicetesthooks`) whose hooks let it hang the agent for the watchdog test:

```sh
go build -tags servicetesthooks -o /tmp/hvmc ./cmd/hvmc
go test -c -tags servicetest -o /tmp/svc.test ./internal/agent/svc/
sudo env HVMC_SERVICETEST_BINARY=/tmp/hvmc /tmp/svc.test -test.run '^TestServiceRoundTrip$' -test.v
```
