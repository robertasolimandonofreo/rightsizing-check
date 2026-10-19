// Package clustertest builds fake clusters for the tests of the packages that read one.
package clustertest

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/cluster"
)

var listKinds = map[string]string{
	"deployments": "DeploymentList", "statefulsets": "StatefulSetList", "daemonsets": "DaemonSetList",
	"rollouts": "RolloutList", "horizontalpodautoscalers": "HorizontalPodAutoscalerList",
	"scaledobjects": "ScaledObjectList", "verticalpodautoscalers": "VerticalPodAutoscalerList",
	"pods": "PodMetricsList",
}

// New returns a dynamic client that knows every resource that the reader lists, holding objs.
func New(objs ...runtime.Object) *fake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{cluster.MetricsPods: "PodMetricsList"}
	for _, k := range cluster.Kinds {
		kinds[k.GVR()] = listKinds[k.GVR().Resource]
	}
	// The tracker guesses a resource from the kind, and PodMetrics would land in "podmetrics",
	// while the metrics API serves them as "pods". Those are added under the right resource.
	var others []runtime.Object
	var metrics []*unstructured.Unstructured
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "PodMetrics" {
			metrics = append(metrics, u)
			continue
		}
		others = append(others, o)
	}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, others...)
	for _, m := range metrics {
		if err := c.Tracker().Create(cluster.MetricsPods, m, m.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return c
}

// Object builds an unstructured object from its parts.
func Object(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	obj := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}
