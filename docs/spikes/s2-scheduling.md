# Spike S2: scheduling chain on kind

This spike checks, on a real cluster, the parts of DESIGN §6 and §7 that the webhook, extender and chart work depends on. It covers extended-resource validation and defaulting, kubelet admission, the CEL-matched mutating webhook, a second upstream kube-scheduler with a binding extender, how fast a pod rejected by the extender is retried, and leader-label routing.

- Harness: `hack/spike-s2/` (`run.sh`, stub `extender/` and `webhook/`, `kind.yaml`, `manifests/`).
- CI: `.github/workflows/spike-s2.yaml`. It runs on pushes to `wp/s2-*` and on manual dispatch.
- The numbers below come from run [41338162301](https://github.com/matfax/hvmc/actions/runs/41338162301). Ranges in brackets cover the three identical runs [41336931060](https://github.com/matfax/hvmc/actions/runs/41336931060), [41337632731](https://github.com/matfax/hvmc/actions/runs/41337632731) and 41338162301.

## Setup and versions

| Component | Version |
|---|---|
| kind | v0.33.0 (binary checked against its SHA-256) |
| Node image | `kindest/node:v1.37.0@sha256:a1ed56cf…80ae5`: 1 control plane and 2 workers, so that the scheduler scores nodes |
| API server, kubelets, kubectl | v1.37.0 |
| Second scheduler | `registry.k8s.io/kube-scheduler:v1.37.0`. The tag comes from the server version, as the chart would derive it. |
| Stub modules | Go 1.27.3 toolchain (`go 1.26.0` directive), `k8s.io/*` v0.37.2 |
| Runner | GitHub `ubuntu-latest` (ubuntu-26.04 image 20261025) |

The second scheduler runs as a Deployment in `hvmc-spike` with its own ServiceAccount and the bindings DESIGN §7.2 lists. Its KubeSchedulerConfiguration is the one in §7.2, with these differences:

- HTTP instead of HTTPS.
- `httpTimeout: 30s`.
- `--v=5`, so that queue moves show up in the log.

The extender's verbs are `filter`, `prioritize` and `bind`. It sets `nodeCacheCapable: true` and `ignorable: false`, and declares `managedResources: [{name: hvmc.fax.fyi/ram, ignoredByScheduler: true}]`.

The stub extender logs every call with a wall-clock and a monotonic timestamp. It binds through `pods/binding`. Pod annotations can make its filter reject a pod: for the first N calls, or until `hvmc.fax.fyi/queue-generation` reaches a value. The reason text it returns can be constant or can change on every call. It also watches the test namespace, so that latencies are measured on one monotonic clock: from the moment a pod update reaches an informer to the next filter call. The runner's wall clock stepped by about 0.7 s during one run, which ruled out comparing wall-clock times across processes.

The stub webhook applies the mutations of §7.1 and logs every AdmissionReview it receives.

## Results

| # | Check | Result |
|---|---|---|
| 1 | Pod with only `limits[hvmc.fax.fyi/ram]` | **PASS**. Accepted. `requests` defaults to a copy of the limits (`{"hvmc.fax.fyi/ram":"1Gi"}`). There is no `requests.memory`, and qosClass is `BestEffort`. With `limits.memory: 2Gi`, `requests.memory` defaults to `2Gi`. Rejected variants: `requests` ≠ `limits` (`must be equal to hvmc.fax.fyi/ram limit of 1Gi`) and requests without limits (`Limit must be set for non overcommitable resources`). |
| 2 | Kubelet admission without the resource in allocatable | **PASS**. The node's allocatable has no `hvmc.fax.fyi/ram`. A pod bound straight to it (`nodeName`) was Ready after 610 ms [590–610]. |
| 3 | Webhook (CEL `matchConditions`, CREATE) | **PASS**. 0 webhook calls for 7 unrelated pods: plain, `limits.memory` only, `requests.cpu` only, explicit `schedulerName: hvmc-scheduler`, and 2 Deployment pods created through `generateName`. 4 calls for 4 hvmc pods, 2 of them server-side dry runs. The webhook sets `schedulerName: hvmc-scheduler`, `requests.memory: 64Mi`, `limits.memory` from the RAM limit when it is missing, and the `managed` annotation. The init-container branch of the CEL expression matches too. The webhook saw `requests.memory: 2Gi`, already defaulted, for a pod that set only `limits.memory: 2Gi`. The stored pod's qosClass is `Burstable`, so qosClass is computed after mutating admission. A pod naming another scheduler is denied. |
| 4 | Second scheduler and extender | **PASS**. The extender received calls only for the two hvmc pods: 1 filter, 1 prioritize and 1 successful bind each. Filter to bind took 4–8 ms [4–9]. Each pod was bound to the node the extender bound. Its `Scheduled` event came from `hvmc-scheduler`, and the pod became Ready. A pod with `schedulerName: hvmc-scheduler` but no RAM limit was scheduled by `hvmc-scheduler` through the default binder, with 0 extender calls. A plain pod went to `default-scheduler`. |
| 4-rbac | §7.2 RBAC as written | **FAIL**. The scheduler never became leader within 45 s: `leases.coordination.k8s.io "hvmc-scheduler" is forbidden: … cannot get resource "leases" … in the namespace "hvmc-spike"`. After adding a Role (leases `create`, plus `get`/`update` on `resourceNames: [hvmc-scheduler]`), it acquired the lease at once. The scheduler log then showed no other `forbidden` errors. |
| 5a | Rejected once, no intervention | **PASS**. The second filter call came 8 ms after the first [7–9 ms], and the pod was bound 13 ms after the first call. The trigger is the scheduler's own `PodScheduled=False` status write. That write is an update to the pod and requeues it at once (`TargetPodUpdate`). |
| 5b | Constant reason, no intervention, 60 s | **PASS** (measured). 3 filter calls in 60 s [2–4], at intervals of 6 ms and 10 152 ms [other runs: 7; 7, 6 831, 49 998]. After the self-triggered retry, the pod waits in the unschedulable pool. Only an unrelated cluster event or the 5-minute flush moves it out. |
| 5c / 5d | Patching `hvmc.fax.fyi/queue-generation` after 60 s or 15 s parked | **PASS**. The next filter call came 2 ms and 1 ms after the extender's informer saw the update [1–2 ms]. The script's upper bound, from the start of the patch command to the call appearing in the log, is 130–140 ms [130–150]. The pod was then bound. |
| 5e | Reason text that changes on every call, no intervention, 20 s | **FAIL** (contradicts §7.3 as written). 1 105 filter calls in 20 s: 55 per second, interval p50 20 ms, minimum 4 ms. The scheduler saw 1 127 updates to the pod. Each new reason rewrites the `PodScheduled` condition message, and that update requeues the pod. Backoff does not hold it back, because `SchedulerPopFromBackoffQ` pops from the backoff queue when the active queue is idle. The rate is capped only by the scheduler's API client rate limit. |
| 5f | Unrelated pod creations while a pod is parked | **PASS** (measured). 5 of 5 creations were followed by a filter call of the parked pod, 5–9 ms after the extender saw the new pod. The 5 creations caused 23 filter calls [20–23], about 4–5 per unrelated pod (add, bind, status updates). |
| 6 | Leader-label routing | **PASS**. After the new pod's label was applied, the EndpointSlice's ready set equalled the new leader within 60 ms in all 5 moves [50–70 ms, at about 50–100 ms polling resolution]. Measured from the removal of the old label, the switch took 190–210 ms [180–210]. That figure includes two `kubectl` round trips. |

## What each result means for DESIGN

1. **§1, extended resources (check 1)**: confirmed. Validation forces `requests = limits` for `hvmc.fax.fyi/ram`, and defaulting copies limits into requests, so `requests[hvmc.fax.fyi/ram]` is always present. When only `limits.memory` is set, `requests.memory` defaults to it. One addition: a pod that carries only the RAM limit is `BestEffort` as submitted. Because qosClass is computed after mutating admission (check 3), the mutated pod is `Burstable`, and node-pressure eviction does not treat it as BestEffort.
2. **§1, kubelet admission (check 2)**: confirmed on 1.37. Kubelet admits a pod whose extended resource is missing from node allocatable, and no node-status patching is needed.
3. **§7.1 (check 3)**: confirmed. CEL `matchConditions` keep every other pod away from the webhook, including controller-created pods with `generateName`. Defaulting has already set `requests.memory` by admission time, so the normalization is needed and works. One wording precision is needed: §7.1 says "each container". The spike rewrote only the containers and init containers that carry a `hvmc.fax.fyi/ram` limit, so that sidecars keep their own memory requests. §7.1 should say which it means. The spike's behaviour is the recommendation.
4. **§7.2 RBAC (check 4-rbac)**: **requires a change**. `system:kube-scheduler` grants `get`/`update` only on the lease named `kube-scheduler`, so with `resourceName: hvmc-scheduler` the scheduler never becomes leader, and no hvmc pod would ever be scheduled. Add to §7.2 and to the `hvmc-scheduler` RBAC in §11: a Role in the release namespace with `coordination.k8s.io` `leases` `create`, plus `get` and `update` restricted to `resourceNames: [hvmc-scheduler]`, and a RoleBinding to the scheduler's ServiceAccount. Nothing else was missing: the scheduler log showed no other `forbidden` errors.
5. **§7.2 and §7.3 routing (check 4)**: confirmed. `managedResources` restricts every extender verb to pods that request the resource. `ignoredByScheduler` lets `NodeResourcesFit` pass nodes without the resource. Bind through the extender and `pods/binding` works. A pod that only names `hvmc-scheduler` never reaches the extender. Prioritize is skipped when only one node passes Filter, so Prioritize can only rank nodes and must not carry side effects.
6. **§7.3 Filter reason text (check 5e)**: **requires a change**. A reason that changes from one call to the next makes the scheduler retry the pod in a tight loop of about 55 filter calls per second per pod. The format in §7.3, `"hvmc: queued #<pos>, need <n>Gi (headroom <h>, pool <p>)"`, changes whenever the queue position, headroom or pool changes. Change §7.3 so that the `FailedNodes` reason is constant while the pod's blocking cause is the same. For example: `"hvmc: waiting for memory lease pod-<uid> (need <n>Gi)"`, `"hvmc: node not mapped to a managed VM"`, or `"hvmc: host agent <state>"`. A reason may change only when the cause itself changes; each change costs one retry. Queue position, headroom and pool go into MemoryLease status and Events instead.
7. **§7.3 requeue nudge (checks 5a–5d)**: confirmed. The nudge is required and fast. With a constant reason, a parked pod is retried once by its own status write. After that it waits in the unschedulable pool, not in backoff, until an unrelated cluster event, an update to the pod itself, or the 5-minute flush (`podMaxInUnschedulablePodsDuration`). Measured: 2–4 calls in 60 s in a quiet cluster. The `queue-generation` patch is followed by a filter call 1–2 ms after the update reaches informers, under 150 ms end to end. Suggested wording for §7.3: "…instead of waiting for an unrelated cluster event or the 5-minute unschedulable flush". The e2e requeue assertion can use a tight bound, for example under 1 s from the patch to the next filter call.
8. **§7.3 Filter cost (check 5f)**: **requires a change** (an added constraint). kube-scheduler records an extender rejection as the pseudo-plugin `Extender`, with a catch-all enqueue extension (kubernetes/kubernetes#122019). Every cluster event therefore requeues every parked hvmc pod: 4–5 filter calls per unrelated pod creation, starting 5–9 ms after it. Filter load grows with the number of waiting pods times cluster churn, and backoff does not bound it. Add to §7.3:
   - Filter is a read-only ledger evaluation, with no agent I/O and no API writes after the first call for a pod UID. MemoryLease creation is idempotent and remembered per UID.
   - Expose a metric for the filter call rate.
9. **§6 leader label (check 6)**: confirmed, no change. The EndpointSlice follows a label move within about 60 ms. The roughly 15 s failover gap is dominated by the lease duration. kube-proxy and dataplane programming after the EndpointSlice update were not measured.

## Not covered

- TLS between scheduler and extender (§7.2 `enableHTTPS`, `tlsConfig`).
- Two scheduler replicas and scheduler failover.
- `httpTimeout` against a slow bind, bind errors and the retry path.
- Preemption. DefaultPreemption ran on every failed attempt and found no candidates.
- kube-scheduler disables opportunistic batching for a profile that has extenders. It logs "Disabling signatures for profile because it has extenders configured". This had no effect here.

## Reproduce

On any machine with Docker, kind, kubectl, Go, jq, openssl and envsubst, run `hack/spike-s2/run.sh`. Set `KEEP_CLUSTER=1` to keep the cluster afterwards. The script prints one `PASS|FAIL check <n>:` line per check, followed by the extender, webhook and scheduler logs it used.
