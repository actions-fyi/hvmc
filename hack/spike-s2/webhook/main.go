// Command webhook is the logging mutating-admission stub of spike S2
// (docs/spikes/s2-scheduling.md). It applies the pod mutations of DESIGN
// §7.1 to pods that carry a limit for hvmc.fax.fyi/ram and logs every
// AdmissionReview it receives as one JSON line, so run.sh can count calls and
// see what the API server had already defaulted before admission.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	resourceRAM          = corev1.ResourceName("hvmc.fax.fyi/ram")
	annManaged           = "hvmc.fax.fyi/managed"
	defaultSchedulerName = corev1.DefaultSchedulerName
	maxRequestBodyBytes  = 8 << 20
)

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type mutator struct {
	schedulerName string
	memoryRequest resource.Quantity
}

func main() {
	listen := flag.String("listen", ":9444", "listen address")
	certFile := flag.String("cert", "/etc/webhook/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("key", "/etc/webhook/tls/tls.key", "TLS key")
	schedulerName := flag.String("scheduler-name", "hvmc-scheduler", "schedulerName to set")
	memoryRequest := flag.String("memory-request", "64Mi", "requests.memory to set")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	q, err := resource.ParseQuantity(*memoryRequest)
	if err != nil {
		slog.Error("memory-request", "err", err)
		os.Exit(1)
	}
	m := &mutator{schedulerName: *schedulerName, memoryRequest: q}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /mutate", m.serve)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	slog.Info("start", "unix_ms", time.Now().UnixMilli(), "listen", *listen)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func (m *mutator) serve(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Request == nil {
		slog.Error("decode", "unix_ms", now.UnixMilli(), "err", err)
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	req := review.Request
	var pod corev1.Pod
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		resp.Allowed = false
		resp.Result = &metav1.Status{Message: "decode pod: " + err.Error()}
	} else if ops, denied := m.mutate(&pod); denied != "" {
		resp.Allowed = false
		resp.Result = &metav1.Status{Message: denied, Code: http.StatusForbidden}
	} else if len(ops) > 0 {
		patch, err := json.Marshal(ops)
		if err != nil {
			resp.Allowed = false
			resp.Result = &metav1.Status{Message: err.Error()}
		} else {
			pt := admissionv1.PatchTypeJSONPatch
			resp.Patch, resp.PatchType = patch, &pt
		}
	}

	in := map[string]map[string]string{}
	all := append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, c := range all {
		in[c.Name] = map[string]string{
			"requests.memory": qstr(c.Resources.Requests, corev1.ResourceMemory),
			"limits.memory":   qstr(c.Resources.Limits, corev1.ResourceMemory),
			"requests.ram":    qstr(c.Resources.Requests, resourceRAM),
			"limits.ram":      qstr(c.Resources.Limits, resourceRAM),
		}
	}
	msg := ""
	if resp.Result != nil {
		msg = resp.Result.Message
	}
	slog.Info("admission", "unix_ms", now.UnixMilli(), "uid", string(req.UID), "operation", string(req.Operation),
		"dryRun", req.DryRun != nil && *req.DryRun, "namespace", req.Namespace, "name", req.Name,
		"generateName", pod.GenerateName, "schedulerNameIn", pod.Spec.SchedulerName, "containersIn", in,
		"allowed", resp.Allowed, "message", msg, "patch", string(resp.Patch))

	review.Response = resp
	review.Request = nil
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(&review); err != nil {
		slog.Error("encode", "err", err)
	}
}

func qstr(l corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := l[name]; ok {
		return q.String()
	}
	return ""
}

// mutate returns the JSON patch of DESIGN §7.1 for pod, or a denial message.
// Pods without a hvmc.fax.fyi/ram limit get no patch (the CEL
// matchCondition should keep them away from the webhook entirely).
func (m *mutator) mutate(pod *corev1.Pod) ([]patchOp, string) {
	var ops []patchOp
	for _, set := range []struct {
		path       string
		containers []corev1.Container
	}{{"/spec/initContainers", pod.Spec.InitContainers}, {"/spec/containers", pod.Spec.Containers}} {
		for i := range set.containers {
			c := &set.containers[i]
			ram, ok := c.Resources.Limits[resourceRAM]
			if !ok {
				continue
			}
			res := c.Resources.DeepCopy()
			if res.Requests == nil {
				res.Requests = corev1.ResourceList{}
			}
			res.Requests[corev1.ResourceMemory] = m.memoryRequest.DeepCopy()
			if _, ok := res.Limits[corev1.ResourceMemory]; !ok {
				res.Limits[corev1.ResourceMemory] = ram.DeepCopy()
			}
			ops = append(ops, patchOp{Op: "add", Path: set.path + "/" + strconv.Itoa(i) + "/resources", Value: res})
		}
	}
	if len(ops) == 0 {
		return nil, ""
	}
	if sn := pod.Spec.SchedulerName; sn != "" && sn != defaultSchedulerName && sn != m.schedulerName {
		return nil, "hvmc: pod requests " + string(resourceRAM) + " but names scheduler " + sn
	}
	ops = append(ops, patchOp{Op: "add", Path: "/spec/schedulerName", Value: m.schedulerName})
	if len(pod.Annotations) == 0 {
		ops = append(ops, patchOp{Op: "add", Path: "/metadata/annotations", Value: map[string]string{annManaged: "true"}})
	} else {
		ops = append(ops, patchOp{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscaper.Replace(annManaged), Value: "true"})
	}
	return ops, ""
}

// jsonPointerEscaper applies RFC 6901 escaping to one path segment.
var jsonPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")
