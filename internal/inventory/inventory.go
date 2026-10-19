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
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// Unreadable lists the files found while walking a directory that could not be parsed as
	// Kubernetes manifests (a Helm template, for example). They are skipped, not fatal. A file
	// given on the command line that cannot be parsed is an error instead.
	Unreadable []Unreadable
	// Autoscalers are the HorizontalPodAutoscalers and KEDA ScaledObjects found next to the
	// workloads. One that scales on utilization uses the request as its denominator.
	Autoscalers []Autoscaler
}

// Autoscaler is what the request-based rules need from an HPA or a KEDA ScaledObject.
type Autoscaler struct {
	Kind       string // HorizontalPodAutoscaler or ScaledObject
	Name       string
	Namespace  string
	TargetKind string
	TargetName string
	Metrics    []ScaleMetric
	// MaxReplicas is zero when the manifest does not say.
	MaxReplicas int32
	File        string
}

// ScaleMetric is a CPU or memory metric that scales on utilization of the request.
type ScaleMetric struct {
	Resource string // cpu or memory
	Target   int    // percent of the request
	// Container limits the metric to one container (HPA ContainerResource, KEDA containerName).
	Container string
}

// Unreadable is a file that was skipped, and why.
type Unreadable struct {
	Path   string
	Reason string
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
			// A file named on the command line must parse. One found in a directory may be
			// a template or a values file, so it is skipped, and nothing it held is kept.
			before, scalers, skipped := len(inv.Workloads), len(inv.Autoscalers), maps.Clone(inv.Skipped)
			if err := inv.Add(path, data); err != nil {
				if path == p {
					return fmt.Errorf("%s: %w", path, err)
				}
				inv.Workloads, inv.Autoscalers, inv.Skipped = inv.Workloads[:before], inv.Autoscalers[:scalers], skipped
				inv.Unreadable = append(inv.Unreadable, Unreadable{Path: path, Reason: firstLine(err)})
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
		if w.Name == "" && om.GenerateName != "" {
			// Created with generateName, so the real name does not exist until it runs.
			w.Name = om.GenerateName + "*"
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
	case "HorizontalPodAutoscaler":
		var o struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				ScaleTargetRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"scaleTargetRef"`
				MaxReplicas                    int32 `json:"maxReplicas"`
				TargetCPUUtilizationPercentage *int  `json:"targetCPUUtilizationPercentage"`
				Metrics                        []struct {
					Type     string `json:"type"`
					Resource *struct {
						Name   string `json:"name"`
						Target struct {
							Type               string `json:"type"`
							AverageUtilization *int   `json:"averageUtilization"`
						} `json:"target"`
					} `json:"resource"`
					ContainerResource *struct {
						Name      string `json:"name"`
						Container string `json:"container"`
						Target    struct {
							Type               string `json:"type"`
							AverageUtilization *int   `json:"averageUtilization"`
						} `json:"target"`
					} `json:"containerResource"`
				} `json:"metrics"`
			} `json:"spec"`
		}
		if err := decode(&o); err != nil {
			return err
		}
		a := Autoscaler{Kind: "HorizontalPodAutoscaler", Name: o.Metadata.Name, Namespace: o.Metadata.Namespace,
			TargetKind: o.Spec.ScaleTargetRef.Kind, TargetName: o.Spec.ScaleTargetRef.Name,
			MaxReplicas: o.Spec.MaxReplicas, File: file}
		if a.Namespace == "" {
			a.Namespace = "default"
		}
		// autoscaling/v1 has only a CPU percentage.
		if v := o.Spec.TargetCPUUtilizationPercentage; v != nil {
			a.Metrics = append(a.Metrics, ScaleMetric{Resource: "cpu", Target: *v})
		}
		for _, m := range o.Spec.Metrics {
			if r := m.Resource; r != nil && r.Target.Type == "Utilization" && r.Target.AverageUtilization != nil {
				a.Metrics = append(a.Metrics, ScaleMetric{Resource: r.Name, Target: *r.Target.AverageUtilization})
			}
			if r := m.ContainerResource; r != nil && r.Target.Type == "Utilization" && r.Target.AverageUtilization != nil {
				a.Metrics = append(a.Metrics, ScaleMetric{Resource: r.Name, Target: *r.Target.AverageUtilization, Container: r.Container})
			}
		}
		inv.Autoscalers = append(inv.Autoscalers, a)
	case "ScaledObject":
		// Only KEDA's: it creates an HPA, and its cpu and memory triggers work the same way.
		if !strings.HasPrefix(meta.APIVersion, "keda.sh/") {
			return nil
		}
		var o struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				ScaleTargetRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"scaleTargetRef"`
				MaxReplicaCount int32 `json:"maxReplicaCount"`
				Triggers        []struct {
					Type       string         `json:"type"`
					MetricType string         `json:"metricType"`
					Metadata   map[string]any `json:"metadata"`
				} `json:"triggers"`
			} `json:"spec"`
		}
		if err := decode(&o); err != nil {
			return err
		}
		a := Autoscaler{Kind: "ScaledObject", Name: o.Metadata.Name, Namespace: o.Metadata.Namespace,
			TargetKind: o.Spec.ScaleTargetRef.Kind, TargetName: o.Spec.ScaleTargetRef.Name,
			MaxReplicas: o.Spec.MaxReplicaCount, File: file}
		if a.Namespace == "" {
			a.Namespace = "default"
		}
		if a.TargetKind == "" {
			a.TargetKind = "Deployment" // KEDA's default
		}
		for _, t := range o.Spec.Triggers {
			if t.Type != "cpu" && t.Type != "memory" {
				continue
			}
			// metricType is the current field; metadata.type is the deprecated one. AverageValue is
			// an absolute number and does not depend on the request.
			kind := t.MetricType
			if kind == "" {
				kind, _ = t.Metadata["type"].(string)
			}
			if kind != "" && kind != "Utilization" {
				continue
			}
			target := 0
			switch v := t.Metadata["value"].(type) {
			case string:
				target, _ = strconv.Atoi(strings.TrimSpace(v))
			case float64:
				target = int(v)
			}
			if target <= 0 {
				continue
			}
			container, _ := t.Metadata["containerName"].(string)
			a.Metrics = append(a.Metrics, ScaleMetric{Resource: t.Type, Target: target, Container: container})
		}
		inv.Autoscalers = append(inv.Autoscalers, a)
	case "ReplicaSet", "ReplicationController":
		// Owned by a Deployment in practice: counted so the report can say what was left out.
		inv.Skipped[meta.Kind]++
	}
	return nil
}

func firstLine(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
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
