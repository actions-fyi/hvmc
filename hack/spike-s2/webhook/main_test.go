package main

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMutate(t *testing.T) {
	m := &mutator{schedulerName: "hvmc-scheduler", memoryRequest: resource.MustParse("64Mi")}
	ram := corev1.ResourceList{resourceRAM: resource.MustParse("1Gi")}

	tests := []struct {
		name      string
		pod       corev1.Pod
		wantPaths []string
		wantDeny  bool
	}{
		{
			name: "no hvmc limit",
			pod:  corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}},
		},
		{
			name: "container limit, no annotations",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				SchedulerName: corev1.DefaultSchedulerName,
				Containers:    []corev1.Container{{Name: "a"}, {Name: "b", Resources: corev1.ResourceRequirements{Limits: ram}}},
			}},
			wantPaths: []string{"/spec/containers/1/resources", "/spec/schedulerName", "/metadata/annotations"},
		},
		{
			name: "init container limit, existing annotations",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"x": "y"}},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "i", Resources: corev1.ResourceRequirements{Limits: ram}}},
					Containers:     []corev1.Container{{Name: "c"}},
				},
			},
			wantPaths: []string{"/spec/initContainers/0/resources", "/spec/schedulerName", "/metadata/annotations/hvmc.fax.fyi~1managed"},
		},
		{
			name: "other scheduler is denied",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				SchedulerName: "other",
				Containers:    []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Limits: ram}}},
			}},
			wantDeny: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, deny := m.mutate(&tt.pod)
			if (deny != "") != tt.wantDeny {
				t.Fatalf("deny = %q, want deny %v", deny, tt.wantDeny)
			}
			if diff := cmp.Diff(tt.wantPaths, opPaths(ops)); diff != "" {
				t.Fatalf("patch paths (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMutateResources(t *testing.T) {
	m := &mutator{schedulerName: "hvmc-scheduler", memoryRequest: resource.MustParse("64Mi")}
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "c",
		Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{resourceRAM: resource.MustParse("1Gi")},
			Requests: corev1.ResourceList{resourceRAM: resource.MustParse("1Gi")},
		},
	}}}}
	ops, _ := m.mutate(&pod)
	raw, err := json.Marshal(ops[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.ResourceRequirements
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if q := got.Requests[corev1.ResourceMemory]; q.String() != "64Mi" {
		t.Errorf("requests.memory = %s, want 64Mi", q.String())
	}
	if q := got.Limits[corev1.ResourceMemory]; q.String() != "1Gi" {
		t.Errorf("limits.memory = %s, want 1Gi", q.String())
	}
	if _, ok := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]; ok {
		t.Error("mutate modified the input pod")
	}
}

func opPaths(ops []patchOp) []string {
	var paths []string
	for _, op := range ops {
		paths = append(paths, op.Path)
	}
	return paths
}
