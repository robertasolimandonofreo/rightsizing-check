// Package cluster reads the workloads, their autoscalers and the usage from a live cluster. It
// only lists: no other verb is ever sent, and the requests are paged and rate limited, because
// reading a production cluster is not free.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

// pageSize is how many objects one list call returns.
const pageSize = 500

// kind is a resource to list. required kinds fail the read when they cannot be listed; the others
// (autoscalers, custom resources that may not be installed) only produce a note.
type kind struct {
	gvr      schema.GroupVersionResource
	required bool
}

// GVR is the resource that is listed.
func (k kind) GVR() schema.GroupVersionResource { return k.gvr }

// Kinds are the resources that are read, in this order, one at a time.
var Kinds = []kind{
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, false},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, false},
	{schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}, false},
	{schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, false},
	{schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}, false},
	{schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}, false},
}

// MetricsPods is the metrics API that kubectl top reads.
var MetricsPods = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// Options says which cluster to read.
type Options struct {
	Kubeconfig string
	Context    string
	// Namespace limits the read to one namespace. Empty reads every namespace.
	Namespace string
	// SkipMetrics does not read the metrics API: the usage comes from somewhere else.
	SkipMetrics bool
	// Client replaces the one built from the kubeconfig. Tests use it.
	Client dynamic.Interface
	// Notes receives what was skipped or could not be read.
	Notes io.Writer
}

// Result is what was read.
type Result struct {
	// Context is the kubeconfig context that was used, to be printed.
	Context string
	Counts  map[string]int
	Samples []usage.Sample
}

// Read lists the workloads and autoscalers into inv and, unless SkipMetrics is set, returns the
// usage of every container from the metrics API (an instant, like kubectl top).
func Read(ctx context.Context, inv *inventory.Inventory, o Options) (*Result, error) {
	res := &Result{Counts: map[string]int{}}
	client := o.Client
	if client == nil {
		c, name, err := newClient(o)
		if err != nil {
			return nil, err
		}
		client, res.Context = c, name
	}
	note := func(format string, args ...any) {
		if o.Notes != nil {
			fmt.Fprintf(o.Notes, format+"\n", args...)
		}
	}

	for _, k := range Kinds {
		n, err := list(ctx, client, k.gvr, o.Namespace, func(item *unstructured.Unstructured) error {
			raw, err := json.Marshal(item.Object)
			if err != nil {
				return err
			}
			return inv.AddJSON("cluster", raw)
		})
		switch {
		case err == nil:
			res.Counts[k.gvr.Resource] = n
		case isAbsent(err):
			note("note: %s is not installed in this cluster, skipped", k.gvr.Resource)
		case k.required:
			return nil, fmt.Errorf("listing %s: %w", k.gvr.Resource, err)
		default:
			note("note: could not list %s (%s), skipped", k.gvr.Resource, short(err))
		}
	}

	if !o.SkipMetrics {
		pods, err := list(ctx, client, MetricsPods, o.Namespace, func(item *unstructured.Unstructured) error {
			res.Samples = append(res.Samples, podSamples(item)...)
			return nil
		})
		if err != nil {
			if isAbsent(err) {
				return nil, errors.New("the metrics API (metrics.k8s.io) is not available: install metrics-server, or take the usage from Prometheus with --prometheus")
			}
			return nil, fmt.Errorf("reading pod metrics: %w", err)
		}
		res.Counts["pod metrics"] = pods
	}
	return res, nil
}

func newClient(o Options) (dynamic.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		rules.ExplicitPath = o.Kubeconfig
	}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: o.Context})
	raw, err := cfg.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("reading the kubeconfig: %w", err)
	}
	name := raw.CurrentContext
	if o.Context != "" {
		name = o.Context
	}
	rest, err := cfg.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("the kubeconfig has no usable cluster: %w", err)
	}
	// Gentle on the API server: a few requests a second, each one bounded.
	rest.QPS, rest.Burst, rest.Timeout = 5, 10, 60*time.Second
	client, err := dynamic.NewForConfig(rest)
	if err != nil {
		return nil, "", err
	}
	return client, name, nil
}

// list pages through one resource and calls fn for every object. It returns how many there were.
func list(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace string, fn func(*unstructured.Unstructured) error) (int, error) {
	var res dynamic.ResourceInterface = client.Resource(gvr)
	if namespace != "" {
		res = client.Resource(gvr).Namespace(namespace)
	}
	total, cont := 0, ""
	for {
		page, err := res.List(ctx, metav1.ListOptions{Limit: pageSize, Continue: cont})
		if err != nil {
			return total, err
		}
		for i := range page.Items {
			if err := fn(&page.Items[i]); err != nil {
				return total, err
			}
			total++
		}
		if cont = page.GetContinue(); cont == "" {
			return total, nil
		}
	}
}

// isAbsent is true when the resource does not exist in the cluster (a custom resource that is not
// installed), which is not an error.
func isAbsent(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err) ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}

func short(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if len(msg) > 140 {
		msg = msg[:140] + "..."
	}
	return msg
}

// podSamples turns one PodMetrics object into one sample per container.
func podSamples(item *unstructured.Unstructured) []usage.Sample {
	containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
	var out []usage.Sample
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		u, _ := m["usage"].(map[string]any)
		s := usage.Sample{Namespace: item.GetNamespace(), Pod: item.GetName(), Container: name,
			CPUMillis: -1, MemBytes: -1, Source: usage.Snapshot}
		if q, err := resource.ParseQuantity(str(u["cpu"])); err == nil {
			s.CPUMillis = float64(q.MilliValue())
		}
		if q, err := resource.ParseQuantity(str(u["memory"])); err == nil {
			s.MemBytes = float64(q.Value())
		}
		if name != "" && (s.CPUMillis >= 0 || s.MemBytes >= 0) {
			out = append(out, s)
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
