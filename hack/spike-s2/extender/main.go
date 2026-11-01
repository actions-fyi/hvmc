// Command extender is the logging scheduler-extender stub of spike S2
// (docs/spikes/s2-scheduling.md). It serves filter, prioritize and bind for
// a kube-scheduler extender configuration with nodeCacheCapable: true, logs
// every call as one JSON line with a Unix-millisecond timestamp, and binds
// through the pods/binding subresource.
//
// Filter rejection is controlled per pod so one extender serves every check:
//
//   - flag -reject-first N, or pod annotation
//     spike.hvmc.fax.fyi/reject-first-filters: "N", rejects the first N
//     filter calls for that pod UID;
//   - pod annotation spike.hvmc.fax.fyi/reject-until-generation: "G" rejects
//     every filter call while hvmc.fax.fyi/queue-generation is below G, which
//     simulates "not grantable until the coordinator nudges the pod";
//   - pod annotation spike.hvmc.fax.fyi/volatile-reason: "true" puts the call
//     number into the FailedNodes text, so it changes on every call (like a
//     reason that carries a queue position or current headroom).
//
// With -watch-namespace it also watches pods there and logs every pod add
// and every change of the hvmc.fax.fyi/queue-generation annotation, so the
// latency from "pod event visible to watchers" to "next filter call" is
// measured on one monotonic clock (mono_ms, milliseconds since start).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

const (
	annRejectFirst      = "spike.hvmc.fax.fyi/reject-first-filters"
	annRejectUntilGen   = "spike.hvmc.fax.fyi/reject-until-generation"
	annVolatileReason   = "spike.hvmc.fax.fyi/volatile-reason"
	annQueueGeneration  = "hvmc.fax.fyi/queue-generation"
	maxRequestBodyBytes = 8 << 20
)

// start anchors mono_ms; time.Since uses the monotonic clock reading, so
// intervals survive wall-clock steps on the host.
var start = time.Now()

type server struct {
	client      kubernetes.Interface
	rejectFirst int

	mu      sync.Mutex
	filters map[types.UID]int // filter calls seen per pod UID
}

func main() {
	listen := flag.String("listen", ":8888", "listen address")
	rejectFirst := flag.Int("reject-first", 0, "reject the first N filter calls of every pod (annotation overrides)")
	watchNamespace := flag.String("watch-namespace", "", "log queue-generation changes of pods in this namespace")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("in-cluster config", "err", err)
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("client", "err", err)
		os.Exit(1)
	}

	s := &server{client: client, rejectFirst: *rejectFirst, filters: map[types.UID]int{}}
	if *watchNamespace != "" {
		if err := watchQueueGeneration(client, *watchNamespace); err != nil {
			slog.Error("watch", "err", err)
			os.Exit(1)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /scheduler/filter", s.filter)
	mux.HandleFunc("POST /scheduler/prioritize", s.prioritize)
	mux.HandleFunc("POST /scheduler/bind", s.bind)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	logCall("start", "listen", *listen, "rejectFirst", *rejectFirst)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

// logCall writes one JSON line with a millisecond timestamp that run.sh
// compares against its own clock (same kernel clock inside kind).
func logCall(msg string, args ...any) {
	slog.Info(msg, append([]any{"unix_ms", time.Now().UnixMilli(), "mono_ms", time.Since(start).Milliseconds()}, args...)...)
}

// watchQueueGeneration logs pod adds and every change of the
// queue-generation annotation as the extender's informer sees them, which approximates when the
// scheduler's own pod informer sees the same update.
func watchQueueGeneration(client kubernetes.Interface, namespace string) error {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace))
	inf := factory.Core().V1().Pods().Informer()
	_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok {
				logCall("pod-add", "pod", podKey(p))
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(*corev1.Pod)
			n, ok2 := newObj.(*corev1.Pod)
			if !ok1 || !ok2 || o.Annotations[annQueueGeneration] == n.Annotations[annQueueGeneration] {
				return
			}
			logCall("pod-update", "pod", podKey(n), "queueGeneration", n.Annotations[annQueueGeneration],
				"resourceVersion", n.ResourceVersion)
		},
	})
	if err != nil {
		return err
	}
	factory.Start(context.Background().Done())
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		logCall("decode-error", "path", r.URL.Path, "err", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func encode(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logCall("encode-error", "err", err.Error())
	}
}

func podKey(p *corev1.Pod) string {
	if p == nil {
		return ""
	}
	return p.Namespace + "/" + p.Name
}

// candidateNames returns the node names the scheduler offered, for either
// nodeCacheCapable setting.
func candidateNames(args *extenderv1.ExtenderArgs) []string {
	if args.NodeNames != nil {
		return slices.Clone(*args.NodeNames)
	}
	var names []string
	if args.Nodes != nil {
		for i := range args.Nodes.Items {
			names = append(names, args.Nodes.Items[i].Name)
		}
	}
	return names
}

func annotationInt(p *corev1.Pod, key string) (int, bool) {
	v, ok := p.Annotations[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// rejectReason decides whether filter call number n (1-based) for pod p is
// rejected, and why.
func (s *server) rejectReason(p *corev1.Pod, n int) string {
	limit := s.rejectFirst
	if v, ok := annotationInt(p, annRejectFirst); ok {
		limit = v
	}
	if n <= limit {
		if p.Annotations[annVolatileReason] == "true" {
			return fmt.Sprintf("spike: rejecting filter call %d of first %d", n, limit)
		}
		return fmt.Sprintf("spike: rejecting the first %d filter calls", limit)
	}
	if want, ok := annotationInt(p, annRejectUntilGen); ok {
		have, _ := annotationInt(p, annQueueGeneration)
		if have < want {
			return fmt.Sprintf("spike: queue-generation %d < %d", have, want)
		}
	}
	return ""
}

func (s *server) filter(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var args extenderv1.ExtenderArgs
	if !decode(w, r, &args) {
		return
	}
	if args.Pod == nil {
		http.Error(w, "no pod", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.filters[args.Pod.UID]++
	n := s.filters[args.Pod.UID]
	s.mu.Unlock()

	names := candidateNames(&args)
	reason := s.rejectReason(args.Pod, n)
	res := extenderv1.ExtenderFilterResult{}
	if reason != "" {
		res.FailedNodes = extenderv1.FailedNodesMap{}
		for _, name := range names {
			res.FailedNodes[name] = reason
		}
		res.NodeNames = &[]string{}
	} else {
		res.NodeNames = &names
		if args.Nodes != nil {
			res.Nodes = args.Nodes
		}
	}
	logCall("filter", "pod", podKey(args.Pod), "uid", string(args.Pod.UID), "call", n,
		"nodes", names, "rejected", reason != "", "reason", reason,
		"queueGeneration", args.Pod.Annotations[annQueueGeneration],
		"resourceVersion", args.Pod.ResourceVersion, "dur_ms", time.Since(start).Milliseconds())
	encode(w, &res)
}

func (s *server) prioritize(w http.ResponseWriter, r *http.Request) {
	var args extenderv1.ExtenderArgs
	if !decode(w, r, &args) {
		return
	}
	names := candidateNames(&args)
	slices.Sort(names)
	list := make(extenderv1.HostPriorityList, 0, len(names))
	for i, name := range names {
		// Deterministic, distinguishable scores: the last node by name wins.
		score := int64(0)
		if len(names) > 1 {
			score = int64(i) * extenderv1.MaxExtenderPriority / int64(len(names)-1)
		}
		list = append(list, extenderv1.HostPriority{Host: name, Score: score})
	}
	logCall("prioritize", "pod", podKey(args.Pod), "nodes", names, "scores", list)
	encode(w, &list)
}

func (s *server) bind(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var args extenderv1.ExtenderBindingArgs
	if !decode(w, r, &args) {
		return
	}
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: args.PodName, Namespace: args.PodNamespace, UID: args.PodUID},
		Target:     corev1.ObjectReference{Kind: "Node", Name: args.Node},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := s.client.CoreV1().Pods(args.PodNamespace).Bind(ctx, binding, metav1.CreateOptions{})
	res := extenderv1.ExtenderBindingResult{}
	errText := ""
	if err != nil {
		errText = err.Error()
		res.Error = errText
	}
	logCall("bind", "pod", args.PodNamespace+"/"+args.PodName, "uid", string(args.PodUID), "node", args.Node,
		"ok", err == nil, "err", errText, "dur_ms", time.Since(start).Milliseconds())
	encode(w, &res)
}
