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
make test lint  # what CI runs
make build      # bin/hvmc
```
