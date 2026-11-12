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
