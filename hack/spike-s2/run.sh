#!/usr/bin/env bash
# Spike S2 driver: scheduling chain on kind (docs/spikes/s2-scheduling.md).
#
# Creates a kind cluster, deploys the stub extender, the stub webhook and a
# second upstream kube-scheduler, and runs the six checks of the spike. Every
# check prints one line "PASS|FAIL check <n>: <measurements>". FAIL means the
# check produced a measured result that contradicts the DESIGN expectation;
# ERROR means it produced no result. Exit status is non-zero only on ERROR.
#
# Requires: docker, kind, kubectl, go, jq, openssl, envsubst.
# Environment: CLUSTER (kind cluster name), KEEP_CLUSTER=1 to keep it.
# SC2016: envsubst takes literal ${VAR} lists. SC2329: helpers defined inside
# checks are called indirectly through wait_until.
# shellcheck disable=SC2016,SC2329
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
CLUSTER=${CLUSTER:-hvmc-spike-s2}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
NS=hvmc-spike                      # spike components
TNS=spike-test                     # test pods
RAM=hvmc.fax.fyi/ram
PAUSE_IMAGE=registry.k8s.io/pause:3.10
WORK=$(mktemp -d)
export PAUSE_IMAGE

RESULTS=()

# Monotonic milliseconds (CLOCK_BOOTTIME via /proc/uptime, 10 ms steps): the
# runner's wall clock was seen stepping by ~0.7 s during a run.
now_ms() {
  local up
  read -r up _ </proc/uptime
  echo $((10#${up/./} * 10))
}
log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }
section() { printf '\n========== %s ==========\n' "$*"; }

# record <PASS|FAIL|ERROR> <check> <text>
record() {
  local line
  line=$(printf '%-5s check %s: %s' "$1" "$2" "$3")
  RESULTS+=("$line")
  printf '\n>>> %s\n\n' "$line"
}

die() {
  record ERROR setup "$*"
  finish
}

# wait_until <timeout_s> <command...>: poll every 200 ms until command succeeds.
wait_until() {
  local deadline=$(($(now_ms) + $1 * 1000))
  shift
  until "$@" >/dev/null 2>&1; do
    (($(now_ms) > deadline)) && return 1
    sleep 0.2
  done
}

# pod_json <name> <resources-json> [spec-merge-json] [annotations-json]
pod_json() {
  local spec=${3:-'{}'} ann=${4:-'{}'}
  jq -n --arg name "$1" --arg ns "$TNS" --arg image "$PAUSE_IMAGE" \
    --argjson res "$2" --argjson spec "$spec" --argjson ann "$ann" '
    {apiVersion: "v1", kind: "Pod",
     metadata: ({name: $name, namespace: $ns, labels: {spike: "s2"}}
                + (if $ann == {} then {} else {annotations: $ann} end)),
     spec: ({terminationGracePeriodSeconds: 0,
             containers: [{name: "c", image: $image, resources: $res}]} + $spec)}'
}

# Error text of a rejected create, without the kubectl prefix.
api_error() { sed -e 's/^Error from server ([A-Za-z]*): //' -e 's/^error when creating "STDIN": //' | tr '\n' ' ' | cut -c1-300; }

ext_log() { kubectl -n "$NS" logs deploy/stub-extender --tail=-1 2>/dev/null | jq -Rc 'fromjson? | select(.msg=="filter" or .msg=="prioritize" or .msg=="bind" or .msg=="pod-update")'; }
wh_log() { kubectl -n "$NS" logs deploy/stub-webhook --tail=-1 2>/dev/null | jq -Rc 'fromjson? | select(.msg=="admission")'; }
wh_count() { wh_log | wc -l | tr -d ' '; }
# intervals: successive differences of a number column, comma separated.
intervals() { awk 'NR>1 {printf "%s%d", (NR>2 ? "," : ""), $1-prev} {prev=$1}'; }
scheduled_by() { kubectl -n "$TNS" get events --field-selector "involvedObject.name=$1,reason=Scheduled" -o json | jq -r '.items[0] | (.reportingComponent // .source.component // "none")'; }
pod_ready() { [[ $(kubectl -n "$TNS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) == True ]]; }

# ---------------------------------------------------------------- setup

setup() {
  section "build stub images"
  for bin in extender webhook; do
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$WORK/$bin" "./hack/spike-s2/$bin") || die "go build $bin"
    docker build --provenance=false --sbom=false -q -f "$HERE/Dockerfile" --build-arg "BIN=$bin" \
      -t "spike.local/stub-$bin:dev" "$WORK" || die "docker build $bin"
  done

  section "kind cluster"
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  kind create cluster --name "$CLUSTER" --config "$HERE/kind.yaml" --wait 180s || die "kind create cluster"
  for bin in extender webhook; do
    kind load docker-image --name "$CLUSTER" "spike.local/stub-$bin:dev" || die "kind load $bin"
  done
  for node in $(kind get nodes --name "$CLUSTER"); do
    docker exec "$node" crictl pull "$PAUSE_IMAGE" >/dev/null || die "pre-pull $PAUSE_IMAGE on $node"
  done

  SERVER_VERSION=$(kubectl version -o json | jq -r .serverVersion.gitVersion)
  # Like the chart: cluster version with any distribution suffix stripped.
  SCHEDULER_IMAGE=registry.k8s.io/kube-scheduler:${SERVER_VERSION%%[+-]*}
  export SCHEDULER_IMAGE
  mapfile -t WORKERS < <(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  ((${#WORKERS[@]} == 2)) || die "expected 2 worker nodes, got ${#WORKERS[@]}"
  section "versions"
  echo "kind:            $(kind version)"
  echo "server:          $SERVER_VERSION"
  echo "kubelets:        $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.nodeInfo.kubeletVersion} {end}')"
  echo "kubectl:         $(kubectl version --client -o json | jq -r .clientVersion.gitVersion)"
  echo "scheduler image: $SCHEDULER_IMAGE"
  echo "node image:      $(grep -m1 'image:' "$HERE/kind.yaml" | awk '{print $2}')"
  kubectl apply -f "$HERE/manifests/namespaces.yaml" >/dev/null || die "namespaces"
}

# ---------------------------------------------------------------- check 1

check1() {
  section "check 1: API validation and defaulting of a pod with only limits[$RAM]"
  local out res qos memres memreq neq reqonly
  if ! out=$(pod_json c1-ram-only "{\"limits\":{\"$RAM\":\"1Gi\"}}" | kubectl create --dry-run=server -o json -f - 2>"$WORK/err"); then
    record FAIL 1 "pod with only limits[$RAM] rejected: $(api_error <"$WORK/err")"
    return
  fi
  res=$(jq -c '.spec.containers[0].resources' <<<"$out")
  qos=$(jq -r '.status.qosClass // "unset"' <<<"$out")
  memres=$(pod_json c1-ram-mem "{\"limits\":{\"$RAM\":\"1Gi\",\"memory\":\"2Gi\"}}" |
    kubectl create --dry-run=server -o json -f - 2>/dev/null | jq -c '.spec.containers[0].resources')
  memreq=$(jq -r '.requests.memory // "unset"' <<<"$memres")
  neq=$(pod_json c1-neq "{\"limits\":{\"$RAM\":\"1Gi\"},\"requests\":{\"$RAM\":\"512Mi\"}}" |
    kubectl create --dry-run=server -f - 2>&1) && neq="ACCEPTED (unexpected)" || neq="rejected: $(api_error <<<"$neq")"
  reqonly=$(pod_json c1-req-only "{\"requests\":{\"$RAM\":\"1Gi\"}}" |
    kubectl create --dry-run=server -f - 2>&1) && reqonly="ACCEPTED (unexpected)" || reqonly="rejected: $(api_error <<<"$reqonly")"
  echo "defaulted resources (ram only):     $res (qosClass $qos)"
  echo "defaulted resources (ram + memory): $memres"
  echo "requests != limits:                 $neq"
  echo "requests only:                      $reqonly"
  if [[ $(jq -r --arg r "$RAM" '.requests[$r] // ""' <<<"$res") == 1Gi && $(jq -r '.requests.memory // ""' <<<"$res") == "" ]]; then
    record PASS 1 "accepted; requests defaulted to $(jq -c .requests <<<"$res") (copy of limits, no requests.memory, qosClass $qos); with limits.memory=2Gi requests.memory defaults to $memreq; requests!=limits $neq; requests-only $reqonly"
  else
    record FAIL 1 "accepted but defaulted to $res"
  fi
}

# ---------------------------------------------------------------- check 2

check2() {
  section "check 2: kubelet admission without $RAM in node allocatable"
  local node=${WORKERS[0]} has t0 t1 status
  has=$(kubectl get node "$node" -o json | jq --arg r "$RAM" '.status.allocatable | has($r)')
  t0=$(now_ms)
  pod_json c2-kubelet "{\"limits\":{\"$RAM\":\"1Gi\"}}" "{\"nodeName\":\"$node\"}" | kubectl create -f - >/dev/null ||
    { record ERROR 2 "could not create pod"; return; }
  if kubectl -n "$TNS" wait --for=condition=Ready pod/c2-kubelet --timeout=120s >/dev/null; then
    t1=$(now_ms)
    record PASS 2 "pod bound directly to $node (allocatable has $RAM: $has) Ready after $((t1 - t0)) ms; kubelet admitted it"
  else
    status=$(kubectl -n "$TNS" get pod c2-kubelet -o json | jq -c '{phase: .status.phase, reason: .status.reason, message: .status.message}')
    record FAIL 2 "pod on $node (allocatable has $RAM: $has) not Ready within 120 s: $status"
  fi
}

# ---------------------------------------------------------------- deploy

deploy_stubs() {
  section "deploy stub extender and webhook"
  (
    cd "$WORK" || exit 1
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=spike-s2-ca -keyout ca.key -out ca.crt 2>/dev/null &&
      openssl req -newkey rsa:2048 -nodes -subj "/CN=stub-webhook.$NS.svc" -keyout tls.key -out tls.csr 2>/dev/null &&
      printf 'subjectAltName=DNS:stub-webhook.%s.svc,DNS:stub-webhook.%s.svc.cluster.local\n' "$NS" "$NS" >san.ext &&
      openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile san.ext -out tls.crt 2>/dev/null
  ) || die "certificates"
  kubectl -n "$NS" create secret tls stub-webhook-tls --cert="$WORK/tls.crt" --key="$WORK/tls.key" >/dev/null || die "tls secret"
  kubectl apply -f "$HERE/manifests/extender.yaml" -f "$HERE/manifests/webhook.yaml" >/dev/null || die "apply stubs"
  kubectl -n "$NS" rollout status deploy/stub-extender deploy/stub-webhook --timeout=120s || die "stubs not ready"
  CA_BUNDLE=$(base64 -w0 "$WORK/ca.crt") envsubst '${CA_BUNDLE}' <"$HERE/manifests/webhook-config.yaml" |
    kubectl apply -f - >/dev/null || die "apply webhook configuration"
  # The API server picks the configuration up asynchronously.
  webhook_active() {
    pod_json probe-webhook "{\"limits\":{\"$RAM\":\"1Gi\"}}" | kubectl create --dry-run=server -o json -f - |
      jq -e '.spec.schedulerName == "hvmc-scheduler"'
  }
  wait_until 60 webhook_active || die "webhook not active after 60 s"
  log "webhook active"
}

# ------------------------------------------------- scheduler + RBAC finding

scheduler_logs() { kubectl -n "$NS" logs deploy/hvmc-scheduler --tail=-1 2>/dev/null; }
lease_acquired() { scheduler_logs | grep -qi 'successfully acquired lease'; }

check_scheduler_rbac() {
  section "check 4 (pre): hvmc-scheduler with exactly the DESIGN §7.2 RBAC"
  envsubst '${SCHEDULER_IMAGE}' <"$HERE/manifests/scheduler.yaml" | kubectl apply -f - >/dev/null || die "apply scheduler"
  kubectl -n "$NS" rollout status deploy/hvmc-scheduler --timeout=120s || die "scheduler pod not running"
  local t0 forbidden
  t0=$(now_ms)
  if wait_until 45 lease_acquired; then
    record PASS 4-rbac "leader lease acquired $(($(now_ms) - t0)) ms after rollout with the DESIGN §7.2 bindings alone"
  else
    forbidden=$(scheduler_logs | grep -m1 -i 'forbidden' | cut -c1-300)
    record FAIL 4-rbac "no leader lease within 45 s with the DESIGN §7.2 bindings alone: ${forbidden:-no forbidden error logged}; adding Role hvmc-scheduler-leader-election (leases create; get/update on resourceName hvmc-scheduler)"
    kubectl apply -f "$HERE/manifests/scheduler-lease-rbac.yaml" >/dev/null || die "apply lease RBAC"
    kubectl -n "$NS" rollout restart deploy/hvmc-scheduler >/dev/null
    kubectl -n "$NS" rollout status deploy/hvmc-scheduler --timeout=120s || die "scheduler restart"
    wait_until 60 lease_acquired || die "scheduler still has no leader lease after adding the lease Role"
    log "lease acquired after adding the lease Role"
  fi
  sleep 10
  forbidden=$(scheduler_logs | grep -i 'forbidden' | grep -oE 'cannot [a-z]+ resource [^ ]+( in API group [^ ]+)?( in the namespace [^ ]+)?' | sort | uniq -c | sort -rn)
  echo "forbidden errors in the running scheduler's log (after leader election):"
  echo "${forbidden:-  none}"
}

# ---------------------------------------------------------------- check 3

check3() {
  section "check 3: webhook mutates hvmc pods at CREATE and sees no other pod"
  local c0 c1 c2 ram ramm init other seen ok=1 notes=()
  c0=$(wh_count)
  # Unrelated pods: none may reach the webhook.
  pod_json u-plain '{}' | kubectl create -f - >/dev/null
  pod_json u-mem '{"limits":{"memory":"128Mi"}}' | kubectl create -f - >/dev/null
  pod_json u-cpu '{"requests":{"cpu":"10m"}}' | kubectl create -f - >/dev/null
  pod_json c4-noram '{}' '{"schedulerName":"hvmc-scheduler"}' | kubectl create -f - >/dev/null
  kubectl -n "$TNS" create deployment u-deploy --image="$PAUSE_IMAGE" --replicas=2 >/dev/null
  deploy_pods() { [[ $(kubectl -n "$TNS" get pods -l app=u-deploy -o name | wc -l) -eq 2 ]]; }
  wait_until 60 deploy_pods || notes+=("deployment pods missing")
  sleep 3
  c1=$(wh_count)
  # hvmc pods: two real, two server-side dry runs (dry runs reach webhooks with sideEffects: None).
  ram=$(pod_json c3-ram "{\"limits\":{\"$RAM\":\"1Gi\"}}" | kubectl create -o json -f - 2>>"$WORK/err3")
  ramm=$(pod_json c3-ram-mem "{\"limits\":{\"$RAM\":\"1Gi\",\"memory\":\"2Gi\"}}" | kubectl create -o json -f - 2>>"$WORK/err3")
  init=$(pod_json c3-init '{}' "{\"initContainers\":[{\"name\":\"i\",\"image\":\"$PAUSE_IMAGE\",\"resources\":{\"limits\":{\"$RAM\":\"1Gi\"}}}]}" |
    kubectl create --dry-run=server -o json -f - 2>>"$WORK/err3")
  other=$(pod_json c3-other "{\"limits\":{\"$RAM\":\"1Gi\"}}" '{"schedulerName":"other-scheduler"}' |
    kubectl create --dry-run=server -o json -f - 2>"$WORK/err") && other="ACCEPTED: $(jq -c .spec.schedulerName <<<"$other")" || other="denied: $(api_error <"$WORK/err")"
  [[ -s $WORK/err3 ]] && { echo "stderr of hvmc pod creates:"; cat "$WORK/err3"; }
  sleep 2
  c2=$(wh_count)

  local fmt='{schedulerName: .spec.schedulerName, managed: .metadata.annotations["hvmc.fax.fyi/managed"], resources: (.spec.initContainers // [] | map(.resources)) + (.spec.containers | map(.resources))}'
  echo "c3-ram     -> $(jq -c "$fmt" <<<"$ram" 2>/dev/null || echo "$ram")"
  echo "c3-ram-mem -> $(jq -c "$fmt" <<<"$ramm" 2>/dev/null || echo "$ramm")"
  echo "c3-init    -> $(jq -c "$fmt" <<<"$init" 2>/dev/null || echo "$init")"
  echo "c3-other   -> $other"
  seen=$(wh_log | jq -r --arg n c3-ram-mem 'select(.name==$n and .dryRun==false) | .containersIn.c["requests.memory"]')
  echo "webhook saw requests.memory of c3-ram-mem at admission: ${seen:-none}"
  echo "webhook calls: before=$c0 after-7-unrelated=$c1 after-4-hvmc=$c2"
  wh_log | jq -c '{unix_ms, name, generateName, dryRun, allowed}'

  ((c1 == c0)) || { ok=0; notes+=("unrelated pods reached the webhook: $((c1 - c0)) calls"); }
  ((c2 == c1 + 4)) || { ok=0; notes+=("expected 4 calls for hvmc pods, got $((c2 - c1))"); }
  jq -e '.spec.schedulerName == "hvmc-scheduler" and .spec.containers[0].resources.requests.memory == "64Mi"
         and .spec.containers[0].resources.limits.memory == "1Gi" and .metadata.annotations["hvmc.fax.fyi/managed"] == "true"' <<<"$ram" >/dev/null 2>&1 ||
    { ok=0; notes+=("c3-ram not mutated as expected"); }
  jq -e '.spec.containers[0].resources.requests.memory == "64Mi" and .spec.containers[0].resources.limits.memory == "2Gi"' <<<"$ramm" >/dev/null 2>&1 ||
    { ok=0; notes+=("c3-ram-mem not mutated as expected"); }
  jq -e '.spec.schedulerName == "hvmc-scheduler" and .spec.initContainers[0].resources.requests.memory == "64Mi"' <<<"$init" >/dev/null 2>&1 ||
    { ok=0; notes+=("init-container-only pod not matched/mutated"); }
  [[ $other == denied* ]] || { ok=0; notes+=("pod naming another scheduler was not denied"); }
  local text="webhook calls: 0 for 7 unrelated pods (plain, limits.memory, requests.cpu, explicit schedulerName, 2 Deployment pods via generateName: $((c1 - c0)) observed), $((c2 - c1)) for 4 hvmc pods; schedulerName and requests.memory=64Mi set at CREATE (container and init-container match); requests.memory seen by the webhook for limits.memory=2Gi: ${seen:-none} (already defaulted); other scheduler $other"
  if ((ok)); then record PASS 3 "$text"; else record FAIL 3 "$text; problems: ${notes[*]}"; fi
}

# ---------------------------------------------------------------- check 4

check4() {
  section "check 4: hvmc-scheduler calls the extender only for hvmc pods; bind via extender"
  local ok=1 notes=() pods p f pr b bnode nname by t_f t_b
  for p in c3-ram c3-ram-mem c4-noram u-plain; do
    wait_until 90 pod_ready "$p" || { ok=0; notes+=("$p not Ready within 90 s"); }
  done
  ext_log | jq -c 'del(.time, .level)'
  pods=$(ext_log | jq -r 'select(.msg != "pod-update") | .pod' | sort -u | tr '\n' ' ')
  [[ $pods == "$TNS/c3-ram $TNS/c3-ram-mem " ]] || { ok=0; notes+=("extender saw pods: $pods"); }
  local per=()
  for p in c3-ram c3-ram-mem; do
    f=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="filter" and .pod==$p)' | jq -s length)
    pr=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="prioritize" and .pod==$p)' | jq -s length)
    b=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="bind" and .pod==$p and .ok==true)' | jq -s length)
    bnode=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="bind" and .pod==$p and .ok==true) | .node' | tail -1)
    nname=$(kubectl -n "$TNS" get pod "$p" -o jsonpath='{.spec.nodeName}')
    by=$(scheduled_by "$p")
    t_f=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="filter" and .pod==$p) | .mono_ms' | head -1)
    t_b=$(ext_log | jq -r --arg p "$TNS/$p" 'select(.msg=="bind" and .pod==$p) | .mono_ms' | tail -1)
    per+=("$p: filter=$f prioritize=$pr bind-ok=$b node=$nname scheduledBy=$by filter->bind=$((${t_b:-0} - ${t_f:-0}))ms")
    ((f >= 1 && pr >= 1 && b == 1)) || { ok=0; notes+=("$p: extender calls filter=$f prioritize=$pr bind-ok=$b"); }
    [[ -n $bnode && $bnode == "$nname" ]] || { ok=0; notes+=("$p: bound node $nname != extender bind node $bnode"); }
    [[ $by == hvmc-scheduler ]] || { ok=0; notes+=("$p scheduled by $by"); }
  done
  local noram_by plain_by
  noram_by=$(scheduled_by c4-noram)
  plain_by=$(scheduled_by u-plain)
  [[ $noram_by == hvmc-scheduler ]] || { ok=0; notes+=("c4-noram scheduled by $noram_by"); }
  [[ $plain_by == default-scheduler ]] || { ok=0; notes+=("u-plain scheduled by $plain_by"); }
  local text="extender saw only [$pods]; ${per[*]}; c4-noram (schedulerName hvmc-scheduler, no $RAM) scheduledBy=$noram_by with 0 extender calls (default binder); u-plain scheduledBy=$plain_by"
  if ((ok)); then record PASS 4 "$text"; else record FAIL 4 "$text; problems: ${notes[*]}"; fi
}

# ---------------------------------------------------------------- check 5
#
# Intervals between extender calls come from the extender's monotonic clock
# (mono_ms), never from wall clocks of different processes.

# filter_mono <pod> [jq-condition]: mono_ms of the pod's filter calls.
filter_mono() { ext_log | jq -r --arg p "$TNS/$1" "select(.msg==\"filter\" and .pod==\$p ${2:+and $2}) | .mono_ms"; }
# stats: "n=<count> min/p50/max=<a>/<b>/<c> ms" of a number column.
stats() { sort -n | awk '{v[NR]=$1} END {if (NR == 0) {print "n=0"; exit} printf "n=%d min/p50/max=%d/%d/%d ms", NR, v[1], v[int((NR+1)/2)], v[NR]}'; }
# pod_updates_seen <pod>: pod updates the scheduler received while it was unscheduled.
pod_updates_seen() { scheduler_logs | grep -c "Update event for unscheduled pod\" pod=\"$TNS/$1\""; }
create_ram_pod() { pod_json "$1" "{\"limits\":{\"$RAM\":\"1Gi\"}}" '{}' "$2" | kubectl create -f - >/dev/null; }

check5a() {
  section "check 5a: extender rejects only the first filter call; no intervention"
  local ts t1 t2 tb
  create_ram_pod r-once '{"spike.hvmc.fax.fyi/reject-first-filters":"1"}'
  if ! wait_until 120 pod_ready r-once; then
    record FAIL 5a "pod rejected once was not Ready within 120 s; filter calls (mono ms): $(filter_mono r-once | tr '\n' ' ')"
    return
  fi
  ts=$(filter_mono r-once)
  t1=$(sed -n 1p <<<"$ts")
  t2=$(sed -n 2p <<<"$ts")
  tb=$(ext_log | jq -r --arg p "$TNS/r-once" 'select(.msg=="bind" and .pod==$p) | .mono_ms' | tail -1)
  record PASS 5a "second filter call $((t2 - t1)) ms after the rejected first one without intervention; bound $((tb - t1)) ms after the first call; pod updates the scheduler saw meanwhile: $(pod_updates_seen r-once) (its own PodScheduled=False write)"
}

# check5_park <observe-id> <patch-id> <pod> <seconds>: the extender rejects
# with a constant reason until queue-generation >= 1. Observe without
# intervention, then patch the annotation as the coordinator's nudge would.
check5_park() {
  local oid=$1 pid=$2 name=$3 secs=$4 ts n tp0 tp1 upd nxt
  section "check $oid: constant rejection reason, no intervention for $secs s"
  create_ram_pod "$name" '{"spike.hvmc.fax.fyi/reject-until-generation":"1"}'
  sleep "$secs"
  ts=$(filter_mono "$name")
  n=$(grep -c . <<<"$ts")
  record PASS "$oid" "$n filter calls in $secs s without intervention; intervals ms: $(intervals <<<"$ts"); pod updates seen by the scheduler: $(pod_updates_seen "$name")"

  section "check $pid: queue-generation patch after $secs s parked"
  has_gen1_call() { [[ -n $(filter_mono "$name" '.queueGeneration=="1"') ]]; }
  tp0=$(now_ms)
  kubectl -n "$TNS" annotate pod "$name" hvmc.fax.fyi/queue-generation=1 >/dev/null
  if ! wait_until 60 has_gen1_call; then
    record FAIL "$pid" "no filter call within 60 s after the patch"
    return
  fi
  tp1=$(now_ms)
  upd=$(ext_log | jq -r --arg p "$TNS/$name" 'select(.msg=="pod-update" and .pod==$p and .queueGeneration=="1") | .mono_ms' | head -1)
  nxt=$(filter_mono "$name" '.queueGeneration=="1"' | head -1)
  wait_until 60 pod_ready "$name"
  local text
  text="next filter call $((nxt - ${upd:-0})) ms after the extender's informer saw the patched pod (upper bound from the script: patch command start to call visible in the log $((tp1 - tp0)) ms, incl. log polling); pod Ready: $(pod_ready "$name" && echo yes || echo no)"
  if [[ -n $upd ]] && ((nxt - upd < 1000)); then record PASS "$pid" "$text"; else record FAIL "$pid" "$text"; fi
}

check5_volatile() {
  section "check 5e: rejection reason that changes on every call, no intervention for 20 s"
  local ts n
  create_ram_pod r-volatile '{"spike.hvmc.fax.fyi/reject-first-filters":"1000000","spike.hvmc.fax.fyi/volatile-reason":"true"}'
  sleep 20
  ts=$(filter_mono r-volatile)
  local seen
  seen=$(pod_updates_seen r-volatile)
  kubectl -n "$TNS" delete pod r-volatile --wait=false >/dev/null
  kubectl -n "$TNS" wait --for=delete pod/r-volatile --timeout=60s >/dev/null
  n=$(grep -c . <<<"$ts")
  local text
  text="$n filter calls in 20 s ($((n / 20))/s); intervals $(intervals <<<"$ts" | tr ',' '\n' | stats); pod updates seen by the scheduler: $seen (each changed reason rewrites the PodScheduled condition message, and that pod update requeues the pod)"
  if ((n > 40)); then record FAIL 5e "$text"; else record PASS 5e "$text"; fi
}

# ---------------------------------------------------------------- check 6

slice_ips() {
  kubectl -n "$TNS" get endpointslices -l kubernetes.io/service-name=leader-spike -o json |
    jq -r '[.items[].endpoints[]? | select(.conditions.ready == true) | .addresses[]] | sort | join(",")'
}

check6() {
  section "check 6: leader-label routing, EndpointSlice follows a label move"
  envsubst '${PAUSE_IMAGE}' <"$HERE/manifests/leader.yaml" | kubectl apply -f - >/dev/null
  if ! wait_until 90 pod_ready leader-a || ! wait_until 90 pod_ready leader-b; then
    record ERROR 6 "leader pods not Ready"
    return
  fi
  declare -A ip
  ip[a]=$(kubectl -n "$TNS" get pod leader-a -o jsonpath='{.status.podIP}')
  ip[b]=$(kubectl -n "$TNS" get pod leader-b -o jsonpath='{.status.podIP}')
  kubectl -n "$TNS" label pod leader-a hvmc.fax.fyi/leader=true >/dev/null
  slice_is() { [[ $(slice_ips) == "$1" ]]; }
  wait_until 30 slice_is "${ip[a]}" ||
    { record ERROR 6 "EndpointSlice never showed leader-a"; return; }
  local cur=a nxt t0 t1 t2 follow=() total=() i deadline
  for i in 1 2 3 4 5; do
    [[ $cur == a ]] && nxt=b || nxt=a
    t0=$(now_ms)
    kubectl -n "$TNS" label pod "leader-$cur" hvmc.fax.fyi/leader- >/dev/null
    kubectl -n "$TNS" label pod "leader-$nxt" hvmc.fax.fyi/leader=true >/dev/null
    t1=$(now_ms)
    deadline=$((t1 + 30000))
    until [[ $(slice_ips) == "${ip[$nxt]}" ]]; do
      (($(now_ms) > deadline)) && break
      sleep 0.05
    done
    t2=$(now_ms)
    follow+=($((t2 - t1)))
    total+=($((t2 - t0)))
    log "move $i: leader-$cur -> leader-$nxt: slice followed $((t2 - t1)) ms after the new label ($((t2 - t0)) ms after the old label was removed)"
    cur=$nxt
  done
  local fs ts max
  fs=$(printf '%s\n' "${follow[@]}" | sort -n | tr '\n' ' ')
  ts=$(printf '%s\n' "${total[@]}" | sort -n | tr '\n' ' ')
  max=$(printf '%s\n' "${total[@]}" | sort -n | tail -1)
  local text="5 label moves; EndpointSlice ready set == new leader after (sorted, ms, from new label applied; ~50-100 ms polling resolution): $fs; from old label removed: $ts"
  if ((max <= 5000)); then record PASS 6 "$text"; else record FAIL 6 "$text"; fi
}

# ---------------------------------------------------------------- finish

finish() {
  section "diagnostics"
  if kubectl -n "$NS" get deploy stub-extender >/dev/null 2>&1; then
    echo "--- extender calls"
    ext_log | jq -c 'del(.time, .level)'
    echo "--- webhook calls"
    wh_log | jq -c 'del(.time, .level, .containersIn)'
    echo "--- hvmc-scheduler queue moves of the check 5 pods (count, message, event, queue)"
    scheduler_logs | grep 'scheduling_queue' | grep -E 'pod="spike-test/r-' |
      sed -E 's/^[IWE][0-9]+ [0-9:.]+ +[0-9]+ [a-z_]+\.go:[0-9]+\] //; s/ schedulingCycle=[0-9]+//; s/ (hint|unschedulable plugins|attempts|backoff)=.*//' |
      sort | uniq -c | sort -rn | head -40
    echo "--- hvmc-scheduler log (filtered)"
    scheduler_logs | grep -iE 'r-once|r-always|r-patch|c3-ram|c4-noram|forbidden|acquired|xtender|backoff|unable to schedule' | cut -c1-400 | tail -400
    echo "--- hvmc-scheduler log tail"
    scheduler_logs | tail -40 | cut -c1-400
    echo "--- events in $TNS"
    kubectl -n "$TNS" get events --sort-by=.lastTimestamp | tail -80
  fi
  section "summary"
  local r errors=0
  for r in "${RESULTS[@]}"; do
    echo "$r"
    [[ $r == ERROR* ]] && errors=1
  done
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    {
      echo "## Spike S2 results"
      echo
      echo "Server ${SERVER_VERSION:-unknown}, scheduler image ${SCHEDULER_IMAGE:-unknown}"
      echo
      for r in "${RESULTS[@]}"; do echo "- \`${r%%:*}\`:${r#*:}"; done
    } >>"$GITHUB_STEP_SUMMARY"
  fi
  if [[ $KEEP_CLUSTER != 1 ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1; fi
  rm -rf "$WORK"
  exit "$errors"
}

setup
check1
check2
deploy_stubs
check_scheduler_rbac
check3
check4
check5a
check5_park 5b 5c r-park-60 60
check5_park 5d-pre 5d r-park-15 15
check5_volatile
check6
finish
