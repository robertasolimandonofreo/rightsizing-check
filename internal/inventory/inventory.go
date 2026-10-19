// Package inventory reads Kubernetes manifests (files, directories or stdin) into a list of
// workloads, each one reduced to what the architecture rules need.
package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Workload is a pod-creating object, with its pod template.
type Workload struct {
	Kind      string
	Namespace string
	Name      string
	Replicas  int32
	// CPUMillis is the CPU requested by one replica, summed over its containers.
	CPUMillis int64
	// LongRunning is true for objects that keep running (Deployment, StatefulSet, Pod). Only
	// those count in the CPU figures: Jobs and DaemonSets do not have a replica count to weigh by.
	LongRunning bool
	Pod         corev1.PodSpec
	File        string
}

// ID is the display name, namespace/kind/name in lower case, like kubectl output.
func (w Workload) ID() string {
	return fmt.Sprintf("%s/%s/%s", w.Namespace, strings.ToLower(w.Kind), w.Name)
}

// RequestedCPU is the CPU the workload asks for in total, in millicores.
func (w Workload) RequestedCPU() int64 { return w.CPUMillis * int64(w.Replicas) }

// Containers returns every container of the pod: init containers first, then the app ones.
func (w Workload) Containers() []corev1.Container {
	out := append([]corev1.Container{}, w.Pod.InitContainers...)
	return append(out, w.Pod.Containers...)
}

// Inventory is what a scan found.
type Inventory struct {
	Workloads []Workload
	// Skipped counts objects that were read but are not analyzed, by kind (for example
	// ReplicaSets, which belong to a Deployment).
	Skipped map[string]int
}

// Paths loads every .yaml, .yml and .json file under the given paths. "-" reads stdin.
func Paths(paths []string, stdin io.Reader) (*Inventory, error) {
	inv := &Inventory{Skipped: map[string]int{}}
	for _, p := range paths {
		if p == "-" {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return nil, err
			}
			if err := inv.Add("", data); err != nil {
				return nil, fmt.Errorf("stdin: %w", err)
			}
			continue
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
			default:
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := inv.Add(path, data); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(inv.Workloads, func(i, j int) bool { return inv.Workloads[i].ID() < inv.Workloads[j].ID() })
	return inv, nil
}

// Add parses a multi-document YAML (or JSON) stream. file is only used to say where a workload
// came from.
func (inv *Inventory) Add(file string, data []byte) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
			continue
		}
		if err := inv.addObject(file, raw); err != nil {
			return err
		}
	}
}

func (inv *Inventory) addObject(file string, raw json.RawMessage) error {
	var meta struct {
		metav1.TypeMeta `json:",inline"`
		Items           []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return err
	}
	decode := func(v any) error { return json.Unmarshal(raw, v) }
	add := func(kind string, om metav1.ObjectMeta, replicas *int32, long bool, pod corev1.PodSpec) {
		w := Workload{
			Kind:        kind,
			Namespace:   om.Namespace,
			Name:        om.Name,
			Replicas:    1,
			LongRunning: long,
			Pod:         pod,
			File:        file,
		}
		if w.Namespace == "" {
			w.Namespace = "default"
		}
		if replicas != nil {
			w.Replicas = *replicas
		}
		w.CPUMillis = cpuRequest(pod)
		inv.Workloads = append(inv.Workloads, w)
	}

	switch meta.Kind {
	case "":
		// Not a Kubernetes object (for example a values file): ignore it.
	case "List":
		for _, item := range meta.Items {
			if err := inv.addObject(file, item); err != nil {
				return err
			}
		}
	case "Deployment":
		var o appsv1.Deployment
		if err := decode(&o); err != nil {
			return err
		}
		add("Deployment", o.ObjectMeta, o.Spec.Replicas, true, o.Spec.Template.Spec)
	case "StatefulSet":
		var o appsv1.StatefulSet
		if err := decode(&o); err != nil {
			return err
		}
		add("StatefulSet", o.ObjectMeta, o.Spec.Replicas, true, o.Spec.Template.Spec)
	case "DaemonSet":
		var o appsv1.DaemonSet
		if err := decode(&o); err != nil {
			return err
		}
		add("DaemonSet", o.ObjectMeta, nil, false, o.Spec.Template.Spec)
	case "Job":
		var o batchv1.Job
		if err := decode(&o); err != nil {
			return err
		}
		add("Job", o.ObjectMeta, nil, false, o.Spec.Template.Spec)
	case "CronJob":
		var o batchv1.CronJob
		if err := decode(&o); err != nil {
			return err
		}
		add("CronJob", o.ObjectMeta, nil, false, o.Spec.JobTemplate.Spec.Template.Spec)
	case "Pod":
		var o corev1.Pod
		if err := decode(&o); err != nil {
			return err
		}
		add("Pod", o.ObjectMeta, nil, true, o.Spec)
	case "ReplicaSet", "ReplicationController":
		// Owned by a Deployment in practice: counted so the report can say what was left out.
		inv.Skipped[meta.Kind]++
	}
	return nil
}

// cpuRequest sums the CPU requests of the app containers, in millicores. Init containers run
// before the app and do not add to the steady-state request.
func cpuRequest(pod corev1.PodSpec) int64 {
	var total int64
	for _, c := range pod.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			total += q.MilliValue()
		}
	}
	return total
}
