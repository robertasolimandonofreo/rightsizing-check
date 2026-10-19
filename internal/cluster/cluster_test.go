package cluster_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/cluster"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/cluster/clustertest"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

func deployment(ns, name string) *unstructured.Unstructured {
	return clustertest.Object("apps/v1", "Deployment", ns, name, map[string]any{"spec": map[string]any{
		"replicas": int64(2),
		"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "app", "image": "a:1", "resources": map[string]any{"requests": map[string]any{"cpu": "500m", "memory": "1Gi"}}},
		}}},
	}})
}

func podMetrics(ns, name string, cpu, mem string) *unstructured.Unstructured {
	return clustertest.Object("metrics.k8s.io/v1beta1", "PodMetrics", ns, name, map[string]any{"containers": []any{
		map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": mem}},
	}})
}

func objects() []runtime.Object {
	return []runtime.Object{
		deployment("shop", "api"),
		deployment("other", "web"),
		clustertest.Object("autoscaling/v2", "HorizontalPodAutoscaler", "shop", "api", map[string]any{"spec": map[string]any{
			"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "api"}, "maxReplicas": int64(5),
			"metrics": []any{map[string]any{"type": "Resource", "resource": map[string]any{"name": "cpu", "target": map[string]any{"type": "Utilization", "averageUtilization": int64(70)}}}},
		}}),
		clustertest.Object("autoscaling.k8s.io/v1", "VerticalPodAutoscaler", "shop", "api", map[string]any{"spec": map[string]any{
			"targetRef": map[string]any{"kind": "Deployment", "name": "api"}, "updatePolicy": map[string]any{"updateMode": "Off"},
		}}),
		podMetrics("shop", "api-5d8f7c9b6-x2k4j", "40m", "100Mi"),
		podMetrics("other", "web-5d8f7c9b6-x2k4j", "5m", "10Mi"),
	}
}

func TestReadListsEverythingAndNothingElse(t *testing.T) {
	client := clustertest.New(objects()...)
	inv := inventory.New()
	var notes bytes.Buffer
	res, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client, Notes: &notes})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 2 || len(inv.Autoscalers) != 1 || len(inv.VPAs) != 1 {
		t.Fatalf("workloads %d, autoscalers %d, vpas %d", len(inv.Workloads), len(inv.Autoscalers), len(inv.VPAs))
	}
	if res.Counts["deployments"] != 2 || res.Counts["pod metrics"] != 2 {
		t.Errorf("counts = %v", res.Counts)
	}
	byPod := map[string]usage.Sample{}
	for _, s := range res.Samples {
		byPod[s.Pod] = s
	}
	got := byPod["api-5d8f7c9b6-x2k4j"]
	if got.Namespace != "shop" || got.Container != "app" || got.CPUMillis != 40 || got.MemBytes != 100<<20 || got.Source != usage.Snapshot {
		t.Errorf("sample = %+v", got)
	}
	// Read-only: the only verb that was ever sent is list.
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("verb %q on %s: the reader must only list", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if len(client.Actions()) == 0 {
		t.Error("nothing was listed")
	}
	// The live Deployment keeps its real replica count.
	for _, w := range inv.Workloads {
		if w.Name == "api" && w.Replicas != 2 {
			t.Errorf("replicas = %d", w.Replicas)
		}
	}
}

func TestReadOneNamespace(t *testing.T) {
	inv := inventory.New()
	res, err := cluster.Read(context.Background(), inv, cluster.Options{Client: clustertest.New(objects()...), Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 1 || inv.Workloads[0].Namespace != "shop" || len(res.Samples) != 1 {
		t.Errorf("workloads %+v samples %d", inv.Workloads, len(res.Samples))
	}
}

func TestSkipMetricsNeverTouchesTheMetricsAPI(t *testing.T) {
	client := clustertest.New(objects()...)
	res, err := cluster.Read(context.Background(), inventory.New(), cluster.Options{Client: client, SkipMetrics: true})
	if err != nil || len(res.Samples) != 0 {
		t.Fatalf("samples %d, err %v", len(res.Samples), err)
	}
	for _, a := range client.Actions() {
		if a.GetResource().Group == "metrics.k8s.io" {
			t.Error("the metrics API was read although the usage comes from elsewhere")
		}
	}
}

func fail(client interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
}, resource string, err error) {
	client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
}

func TestMissingCustomResourcesAreNotes(t *testing.T) {
	client := clustertest.New(objects()...)
	fail(client, "rollouts", apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"}, ""))
	fail(client, "horizontalpodautoscalers", apierrors.NewForbidden(schema.GroupResource{Resource: "horizontalpodautoscalers"}, "", nil))
	var notes bytes.Buffer
	inv := inventory.New()
	if _, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client, Notes: &notes}); err != nil {
		t.Fatalf("optional kinds must not fail the read: %v", err)
	}
	for _, want := range []string{"rollouts is not installed", "could not list horizontalpodautoscalers"} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("notes lack %q:\n%s", want, notes.String())
		}
	}
	if len(inv.Workloads) != 2 {
		t.Errorf("the rest must still be read: %d workloads", len(inv.Workloads))
	}
}

func TestRequiredAndMetricsErrors(t *testing.T) {
	client := clustertest.New(objects()...)
	fail(client, "deployments", apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil))
	if _, err := cluster.Read(context.Background(), inventory.New(), cluster.Options{Client: client}); err == nil || !strings.Contains(err.Error(), "listing deployments") {
		t.Errorf("a Deployments list that is forbidden must fail the read: %v", err)
	}
	client = clustertest.New(objects()...)
	fail(client, "pods", apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, ""))
	if _, err := cluster.Read(context.Background(), inventory.New(), cluster.Options{Client: client}); err == nil || !strings.Contains(err.Error(), "metrics-server") {
		t.Errorf("a missing metrics API must point at metrics-server and Prometheus: %v", err)
	}
	// With another usage source the missing metrics API does not matter.
	if _, err := cluster.Read(context.Background(), inventory.New(), cluster.Options{Client: client, SkipMetrics: true}); err != nil {
		t.Errorf("skipping the metrics API: %v", err)
	}
}

func TestPaging(t *testing.T) {
	client := clustertest.New()
	calls := 0
	client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"})
		if calls == 1 {
			list.Items = []unstructured.Unstructured{*deployment("shop", "first")}
			list.SetContinue("next")
		} else {
			list.Items = []unstructured.Unstructured{*deployment("shop", "second")}
		}
		return true, list, nil
	})
	inv := inventory.New()
	if _, err := cluster.Read(context.Background(), inv, cluster.Options{Client: client, SkipMetrics: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(inv.Workloads) != 2 {
		t.Errorf("calls %d, workloads %d: the second page must be read", calls, len(inv.Workloads))
	}
}
