package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
