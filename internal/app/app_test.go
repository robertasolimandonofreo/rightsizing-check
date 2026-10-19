package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/cluster/clustertest"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Version: "test"})
	return code, out.String(), errb.String()
}

const (
	sample = "../../testdata/sample.yaml"
	csv    = "../../testdata/usage.csv"
)

func TestScanTable(t *testing.T) {
	code, out, _ := run(t, "", "scan", sample, "--csv", csv)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"STATUS", "shop/deployment/api", "oversized", "undersized", "6 workloads: 2 oversized, 2 undersized, 1 ok, 1 without data", "Reclaimable requests"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Oversized workloads come first, with the most reclaimable CPU on top.
	if strings.Index(out, "shop/deployment/api") > strings.Index(out, "kube-system/daemonset/agent") {
		t.Errorf("api frees more CPU than the agent and should come first:\n%s", out)
	}
	if strings.Contains(out, "per month") {
		t.Error("the cost line needs prices")
	}
}

func TestCostLineAndFlagsAfterPaths(t *testing.T) {
	code, out, _ := run(t, "", "scan", sample, "--csv", csv, "--cpu-price", "25", "--mem-price", "4")
	if code != exitOK || !strings.Contains(out, "per month") {
		t.Errorf("exit=%d out=%s", code, out)
	}
}

func TestJSON(t *testing.T) {
	code, out, _ := run(t, "", "scan", "-f", "json", "--csv", csv, sample)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Summary       struct {
			Reclaim int64 `json:"reclaimableCPUMillis"`
			Percent int   `json:"reclaimableCPUPercent"`
			Memory  int64 `json:"reclaimableMemoryBytes"`
		} `json:"summary"`
		Workloads []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d", doc.SchemaVersion)
	}
	got := map[string]string{}
	for _, w := range doc.Workloads {
		got[w.ID] = w.Status
	}
	for id, want := range map[string]string{
		"shop/deployment/api":         "oversized",
		"shop/deployment/web":         "ok",
		"shop/deployment/worker":      "undersized",
		"shop/statefulset/db":         "undersized", // no requests
		"kube-system/daemonset/agent": "oversized",
		"shop/cronjob/report":         "no-data",
	} {
		if got[id] != want {
			t.Errorf("%s = %q, want %q", id, got[id], want)
		}
	}
	// api frees 860m x 3 replicas; the DaemonSet is left out of the sums. Requested (long-running
	// with data): api 3000m + web 300m + worker 200m + db 0 = 3500m.
	if doc.Summary.Reclaim != 2580 || doc.Summary.Percent != 73 {
		t.Errorf("reclaimable = %dm (%d%%), want 2580m (73%%)", doc.Summary.Reclaim, doc.Summary.Percent)
	}
}

func TestTopAndPrometheusInputs(t *testing.T) {
	dir := t.TempDir()
	top := filepath.Join(dir, "top.txt")
	os.WriteFile(top, []byte("NAMESPACE POD NAME CPU(cores) MEMORY(bytes)\nshop api-5d8f7c9b6-x2k4j app 120m 600Mi\n"), 0o600)
	code, out, _ := run(t, "", "scan", sample, "--top", top)
	if code != exitOK || !strings.Contains(out, "oversized") {
		t.Errorf("top: exit=%d out=%s", code, out)
	}
	if code, out, _ := run(t, "", "scan", sample, "--top", top, "-v"); code != exitOK || !strings.Contains(out, "RC008") {
		t.Errorf("a snapshot should be flagged (RC008):\n%s", out)
	}
	cpu := filepath.Join(dir, "cpu.json")
	mem := filepath.Join(dir, "mem.json")
	os.WriteFile(cpu, []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"0.12"]}]}}`), 0o600)
	os.WriteFile(mem, []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"629145600"]}]}}`), 0o600)
	code, out, _ = run(t, "", "scan", sample, "--prom-cpu", cpu, "--prom-mem", mem, "-f", "markdown")
	if code != exitOK || !strings.HasPrefix(out, "<!-- rightsizing-check -->") || !strings.Contains(out, "`shop/deployment/api`") {
		t.Errorf("prometheus: exit=%d out=%s", code, out)
	}
}

func TestFailOn(t *testing.T) {
	tests := []struct {
		failOn string
		want   int
	}{{"", exitOK}, {"undersized", exitFail}, {"oversized", exitFail}}
	for _, tt := range tests {
		args := []string{"scan", sample, "--csv", csv}
		if tt.failOn != "" {
			args = append(args, "--fail-on", tt.failOn)
		}
		if code, _, _ := run(t, "", args...); code != tt.want {
			t.Errorf("--fail-on %q: exit = %d, want %d", tt.failOn, code, tt.want)
		}
	}
	// Only oversized workloads: undersized does not fail, oversized does.
	oversized := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api, namespace: shop}\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n        - {name: app, image: a:1, resources: {requests: {cpu: 2, memory: 4Gi}}}\n"
	if code, _, _ := run(t, oversized, "scan", "-", "--csv", csv, "--fail-on", "undersized"); code != exitOK {
		t.Errorf("an oversized workload must not fail --fail-on undersized, exit %d", code)
	}
	if code, _, _ := run(t, oversized, "scan", "-", "--csv", csv, "--fail-on", "oversized"); code != exitFail {
		t.Errorf("an oversized workload must fail --fail-on oversized, exit %d", code)
	}
}

func TestUsageErrors(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("not json"), 0o600)
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"scan"},
		{"scan", sample}, // no usage data
		{"scan", sample, "--csv", csv, "-f", "xml"},
		{"scan", sample, "--csv", csv, "--fail-on", "bad"},
		{"scan", sample, "--csv", csv, "--slack", "0.5"},
		{"scan", sample, "--csv", "missing.csv"},
		{"scan", sample, "--prom-cpu", bad},
		{"scan", "does-not-exist", "--csv", csv},
		{"explain"},
		{"explain", "RC999"},
	} {
		if code, _, _ := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestExplainAndVersion(t *testing.T) {
	code, out, _ := run(t, "", "explain", "rc001")
	if code != exitOK || !strings.Contains(out, "RC001 [waste]") || !strings.Contains(out, "consolidate") {
		t.Errorf("exit=%d out=%s", code, out)
	}
	if _, out, _ := run(t, "", "version"); !strings.Contains(out, "test") {
		t.Errorf("version output = %q", out)
	}
}

func TestEmptyScanWarns(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := run(t, "", "scan", dir, "--csv", csv)
	if code != exitOK || !strings.Contains(errOut, "no workloads found") {
		t.Errorf("exit=%d stderr=%q", code, errOut)
	}
}

func TestHPAWarningEndToEnd(t *testing.T) {
	dir := t.TempDir()
	manifests := `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: shop}
spec:
  replicas: 2
  template:
    spec:
      containers:
        - {name: app, image: a:1, resources: {requests: {cpu: 500m, memory: 512Mi}}}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: api, namespace: shop}
spec:
  scaleTargetRef: {name: api}
  maxReplicaCount: 6
  triggers:
    - {type: cpu, metricType: Utilization, metadata: {value: "75"}}
`
	if err := os.WriteFile(filepath.Join(dir, "m.yaml"), []byte(manifests), 0o600); err != nil {
		t.Fatal(err)
	}
	usageCSV := filepath.Join(dir, "u.csv")
	if err := os.WriteFile(usageCSV, []byte("namespace,pod,container,cpu_millis,memory_bytes\nshop,api-5d8f7c9b6-x2k4j,app,38,100000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := run(t, "", "scan", filepath.Join(dir, "m.yaml"), "--csv", usageCSV, "-f", "json")
	var doc struct {
		Summary struct {
			Reclaim int64 `json:"reclaimableCPUMillis"`
			Scale   int64 `json:"reclaimableCPUMillisWithScaleOut"`
		} `json:"summary"`
		Workloads []struct {
			AutoscaledBy []string `json:"autoscaledBy"`
			Findings     []struct {
				Rule string `json:"rule"`
			} `json:"findings"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Workloads) != 1 {
		t.Fatalf("%v: %s", err, out)
	}
	w := doc.Workloads[0]
	var rules []string
	for _, f := range w.Findings {
		rules = append(rules, f.Rule)
	}
	if !strings.Contains(strings.Join(rules, ","), "RC009") || len(w.AutoscaledBy) != 1 || w.AutoscaledBy[0] != "ScaledObject/api" {
		t.Errorf("rules %v, autoscaledBy %v", rules, w.AutoscaledBy)
	}
	if doc.Summary.Scale == 0 || doc.Summary.Scale != doc.Summary.Reclaim {
		t.Errorf("all the reclaimable CPU is under the HPA: %+v", doc.Summary)
	}
	_, table, _ := run(t, "", "scan", filepath.Join(dir, "m.yaml"), "--csv", usageCSV)
	if !strings.Contains(table, "would scale out if the request came down") {
		t.Errorf("the table summary should explain it:\n%s", table)
	}
}

func fakePrometheus(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("query"), "[30d") {
			t.Errorf("the --window must reach the query: %q", r.URL.Query().Get("query"))
		}
		series := func(v string) string {
			return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"` + v + `"]}]}}`
		}
		if strings.Contains(r.URL.Query().Get("query"), "container_cpu_usage_seconds_total") {
			_, _ = w.Write([]byte(series("0.04")))
			return
		}
		_, _ = w.Write([]byte(series("104857600")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScanReadsUsageFromPrometheus(t *testing.T) {
	srv := fakePrometheus(t)
	dir := t.TempDir()
	m := filepath.Join(dir, "m.yaml")
	os.WriteFile(m, []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api, namespace: shop}\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n        - {name: app, image: a:1, resources: {requests: {cpu: 500m, memory: 1Gi}}}\n"), 0o600)
	code, out, errOut := run(t, "", "scan", m, "--prometheus", srv.URL+"/query", "--window", "30d", "--prom-allow-long-window")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "oversized") || strings.Contains(out, "RC008") {
		t.Errorf("a Prometheus window is not a snapshot, and the workload is oversized:\n%s", out)
	}
	if !strings.Contains(errOut, "reading usage from") || !strings.Contains(errOut, "over 30d") || !strings.Contains(errOut, "read 1 CPU and 1 memory series") {
		t.Errorf("progress on stderr = %q", errOut)
	}
}

func TestPrometheusOptionErrors(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "m.yaml")
	os.WriteFile(m, []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {containers: [{name: c, image: c:1}]}\n"), 0o600)
	for _, args := range [][]string{
		{"scan", m, "--prometheus", "not a url"},
		{"scan", m, "--prometheus", "https://prom.example.invalid", "--window", "seven"},
		{"scan", m, "--prometheus", "https://prom.example.invalid", "--prom-timeout", "soon"},
		{"scan", m, "--prometheus", "https://prom.example.invalid", "--window", "14d"},
		{"scan", m, "--prometheus", "https://prom.example.invalid", "--prom-token-env", "RIGHTSIZING_TEST_TOKEN_UNSET"},
		{"scan", m, "--prometheus", "https://prom.example.invalid", "--prom-selector", `namespace="a"}`},
	} {
		if code, _, _ := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args[2:], code, exitUsage)
		}
	}
}

func TestVPAAndRolloutEndToEnd(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "m.yaml")
	os.WriteFile(m, []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata: {name: canary, namespace: shop}
spec:
  replicas: 2
  template:
    spec:
      containers:
        - {name: app, image: a:1, resources: {requests: {cpu: 500m, memory: 1Gi}}}
---
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata: {name: canary, namespace: shop}
spec:
  targetRef: {apiVersion: argoproj.io/v1alpha1, kind: Rollout, name: canary}
  updatePolicy: {updateMode: InPlaceOrRecreate}
`), 0o600)
	u := filepath.Join(dir, "u.csv")
	os.WriteFile(u, []byte("namespace,pod,container,cpu_millis,memory_bytes\nshop,canary-5d8f7c9b6-x2k4j,app,40,100000000\n"), 0o600)
	_, out, _ := run(t, "", "scan", m, "--csv", u, "-f", "json")
	var doc struct {
		Summary struct {
			Reclaim  int64 `json:"reclaimableCPUMillis"`
			UnderVPA int64 `json:"reclaimableCPUMillisUnderVPA"`
		} `json:"summary"`
		Workloads []struct {
			ID           string   `json:"id"`
			ControlledBy []string `json:"controlledBy"`
			Findings     []struct {
				Rule string `json:"rule"`
			} `json:"findings"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Workloads) != 1 {
		t.Fatalf("%v: %s", err, out)
	}
	w := doc.Workloads[0]
	var rules []string
	for _, f := range w.Findings {
		rules = append(rules, f.Rule)
	}
	if w.ID != "shop/rollout/canary" || !strings.Contains(strings.Join(rules, ","), "RC010") || len(w.ControlledBy) != 1 {
		t.Errorf("id %q rules %v controlledBy %v", w.ID, rules, w.ControlledBy)
	}
	if doc.Summary.UnderVPA == 0 || doc.Summary.UnderVPA != doc.Summary.Reclaim {
		t.Errorf("all the reclaimable CPU is under the VPA: %+v", doc.Summary)
	}
	_, table, _ := run(t, "", "scan", m, "--csv", u)
	if !strings.Contains(table, "whose requests a VPA sets (RC010)") {
		t.Errorf("the summary should explain it:\n%s", table)
	}
}

func clusterObjects() []runtime.Object {
	dep := clustertest.Object("apps/v1", "Deployment", "shop", "api", map[string]any{"spec": map[string]any{
		"replicas": int64(2),
		"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "app", "image": "a:1", "resources": map[string]any{"requests": map[string]any{"cpu": "500m", "memory": "1Gi"}}},
		}}},
	}})
	metrics := clustertest.Object("metrics.k8s.io/v1beta1", "PodMetrics", "shop", "api-5d8f7c9b6-x2k4j", map[string]any{"containers": []any{
		map[string]any{"name": "app", "usage": map[string]any{"cpu": "40m", "memory": "100Mi"}},
	}})
	return []runtime.Object{dep, metrics}
}

func runCluster(t *testing.T, client dynamic.Interface, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Version: "test", Cluster: client})
	return code, out.String(), errb.String()
}

func TestScanCluster(t *testing.T) {
	client := clustertest.New(clusterObjects()...)
	code, out, errOut := runCluster(t, client, "scan", "--cluster")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "shop/deployment/api") || !strings.Contains(out, "oversized") || !strings.Contains(out, "RC008") && !strings.Contains(out, "RC001") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(errOut, "read-only") || !strings.Contains(errOut, "1 Deployments") || !strings.Contains(errOut, "usage of 1 containers") {
		t.Errorf("stderr = %q", errOut)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("verb %q: --cluster must only list", a.GetVerb())
		}
	}
	// The usage of the metrics API is an instant, and the tool says so.
	_, out, _ = runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "-v")
	if !strings.Contains(out, "RC008") {
		t.Errorf("metrics-server usage is a snapshot (RC008):\n%s", out)
	}
}

func TestScanClusterWithAnotherUsageSourceSkipsTheMetricsAPI(t *testing.T) {
	client := clustertest.New(clusterObjects()...)
	csv := filepath.Join(t.TempDir(), "u.csv")
	os.WriteFile(csv, []byte("namespace,pod,container,cpu_millis,memory_bytes\nshop,api-5d8f7c9b6-x2k4j,app,40,100000000\n"), 0o600)
	code, out, _ := runCluster(t, client, "scan", "--cluster", "--csv", csv, "-v")
	if code != exitOK || strings.Contains(out, "RC008") {
		t.Errorf("a CSV is a window, not a snapshot (exit %d):\n%s", code, out)
	}
	for _, a := range client.Actions() {
		if a.GetResource().Group == "metrics.k8s.io" {
			t.Error("the metrics API must not be read when the usage is given")
		}
	}
}

func TestScanClusterNamespaceFlag(t *testing.T) {
	// Without -n the whole cluster is read, even though -n defaults to "default" for kubectl top.
	_, out, _ := runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster")
	if !strings.Contains(out, "shop/deployment/api") {
		t.Errorf("no -n: every namespace is read:\n%s", out)
	}
	_, out, _ = runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "-n", "other")
	if strings.Contains(out, "shop/deployment/api") {
		t.Errorf("-n other must not read shop:\n%s", out)
	}
	_, out, _ = runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster", "--namespace", "shop")
	if !strings.Contains(out, "shop/deployment/api") {
		t.Errorf("--namespace shop must read shop:\n%s", out)
	}
}

func TestScanNeedsPathsOrCluster(t *testing.T) {
	if code, _, errOut := run(t, "", "scan"); code != exitUsage || !strings.Contains(errOut, "--cluster") {
		t.Errorf("exit %d, %q", code, errOut)
	}
}

func TestNoWorkloadsWarningComesAfterTheClusterRead(t *testing.T) {
	// The cluster has workloads: no warning, even though no path was given.
	_, _, errOut := runCluster(t, clustertest.New(clusterObjects()...), "scan", "--cluster")
	if strings.Contains(errOut, "no workloads found") {
		t.Errorf("the cluster has workloads, but got: %q", errOut)
	}
	// An empty cluster does warn, and says where it looked.
	_, _, errOut = runCluster(t, clustertest.New(), "scan", "--cluster")
	if !strings.Contains(errOut, "no workloads found in the cluster") {
		t.Errorf("an empty cluster should warn: %q", errOut)
	}
}

func TestJSONFileSitsNextToTheChosenFormat(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	code, md, _ := run(t, "", "scan", sample, "--csv", csv, "-f", "markdown", "--json-file", out)
	if code != exitOK || !strings.HasPrefix(md, "<!-- rightsizing-check -->") {
		t.Fatalf("exit %d, stdout %.60q", code, md)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Summary       struct {
			Total int `json:"total"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.SchemaVersion != 1 || doc.Summary.Total != 6 {
		t.Errorf("json file: %v %+v", err, doc)
	}
	if code, _, _ := run(t, "", "scan", sample, "--csv", csv, "--json-file", filepath.Join(t.TempDir(), "no", "such", "dir", "r.json")); code != exitUsage {
		t.Errorf("an unwritable file must be an error, exit %d", code)
	}
}

func TestJSONFileReadsPrometheusOnce(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		series := func(v string) string {
			return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"` + v + `"]}]}}`
		}
		if strings.Contains(r.URL.Query().Get("query"), "container_cpu_usage_seconds_total") {
			_, _ = w.Write([]byte(series("0.04")))
			return
		}
		_, _ = w.Write([]byte(series("104857600")))
	}))
	t.Cleanup(srv.Close)
	m := filepath.Join(t.TempDir(), "m.yaml")
	os.WriteFile(m, []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api, namespace: shop}\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n        - {name: app, image: a:1, resources: {requests: {cpu: 500m}}}\n"), 0o600)
	if code, _, _ := run(t, "", "scan", m, "--prometheus", srv.URL, "-f", "markdown", "--json-file", filepath.Join(t.TempDir(), "r.json")); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if hits != 2 {
		t.Errorf("both formats come from one read: %d requests, want 2 (one CPU, one memory)", hits)
	}
}
