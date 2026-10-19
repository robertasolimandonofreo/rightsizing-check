package rightsize

import (
	"strings"
	"testing"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

func workload(t *testing.T, kind, name, resources string, replicas int) inventory.Workload {
	t.Helper()
	doc := "apiVersion: apps/v1\nkind: " + kind + "\nmetadata: {name: " + name + ", namespace: shop}\nspec:\n"
	if kind != "DaemonSet" {
		doc += "  replicas: " + string(rune('0'+replicas)) + "\n"
	}
	doc += "  template:\n    spec:\n      containers:\n        - name: app\n          image: app:1\n          resources:\n" + resources
	inv := &inventory.Inventory{Skipped: map[string]int{}}
	if err := inv.Add("", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	return inv.Workloads[0]
}

const mib = 1 << 20

func sample(pod string, cpu, mem float64) usage.Sample {
	return usage.Sample{Namespace: "shop", Pod: pod, Container: "app", CPUMillis: cpu, MemBytes: mem, Source: usage.Window}
}

func ids(a Assessment) string {
	var out []string
	for _, f := range a.Findings {
		out = append(out, f.Rule)
	}
	return strings.Join(out, ",")
}

func TestEvaluate(t *testing.T) {
	const big = "            requests: {cpu: 1000m, memory: 2Gi}\n"
	tests := []struct {
		name      string
		resources string
		cpu, mem  float64
		want      string
		status    Status
	}{
		{"inflated CPU and memory", big, 120, 600 * mib, "RC001,RC002", Oversized},
		{"close to the request", "            requests: {cpu: 150m, memory: 700Mi}\n", 120, 600 * mib, "", OK},
		{"CPU above request", "            requests: {cpu: 100m, memory: 700Mi}\n", 300, 600 * mib, "RC003", Undersized},
		{"memory above request", "            requests: {cpu: 150m, memory: 256Mi}\n", 120, 600 * mib, "RC004", Undersized},
		{"close to the memory limit", "            requests: {cpu: 150m, memory: 700Mi}\n            limits: {memory: 640Mi}\n", 120, 600 * mib, "RC005", Undersized},
		{"no requests", "            limits: {memory: 1Gi}\n", 120, 600 * mib, "RC006,RC006", Undersized},
		{"risk outranks waste", "            requests: {cpu: 1000m, memory: 256Mi}\n", 120, 600 * mib, "RC004,RC001", Undersized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := workload(t, "Deployment", "api", tt.resources, 2)
			a := Evaluate(w, map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-x2k4j", tt.cpu, tt.mem)}}, DefaultConfig(), nil)
			if ids(a) != tt.want || a.Status != tt.status {
				t.Errorf("findings %q status %s, want %q %s (%+v)", ids(a), a.Status, tt.want, tt.status, a.Findings)
			}
		})
	}
}

func TestRecommendationAndReclaim(t *testing.T) {
	w := workload(t, "Deployment", "api", "            requests: {cpu: 1000m, memory: 2Gi}\n", 3)
	a := Evaluate(w, map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-x2k4j", 120, 600*mib)}}, DefaultConfig(), nil)
	c := a.Containers[0]
	// 120m + 15% = 138m, rounded up to 140m. 600Mi + 20% = 720Mi, a multiple of 4Mi already.
	if c.CPURec != 140 || c.MemRec != 720*mib {
		t.Errorf("recommended %dm %d, want 140m %d", c.CPURec, c.MemRec, 720*mib)
	}
	if c.CPUReclaim != 860 || a.ReclaimableCPU() != 860*3 {
		t.Errorf("reclaim %dm, total %dm", c.CPUReclaim, a.ReclaimableCPU())
	}
	if a.ReclaimableMemory() != (2048-720)*mib*3 {
		t.Errorf("memory reclaim = %d", a.ReclaimableMemory())
	}
}

func TestWorstReplicaDecides(t *testing.T) {
	w := workload(t, "Deployment", "api", "            requests: {cpu: 500m, memory: 1Gi}\n", 2)
	a := Evaluate(w, map[string][]usage.Sample{"app": {
		sample("api-5d8f7c9b6-aaaaa", 50, 100*mib),
		sample("api-5d8f7c9b6-bbbbb", 400, 100*mib),
	}}, DefaultConfig(), nil)
	if a.Containers[0].CPUUsage != 400 || a.Containers[0].Pods != 2 {
		t.Errorf("container = %+v", a.Containers[0])
	}
}

func TestMinimumsAndNoData(t *testing.T) {
	w := workload(t, "Deployment", "api", "            requests: {cpu: 500m, memory: 1Gi}\n", 1)
	a := Evaluate(w, map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-aaaaa", 0, 1*mib)}}, DefaultConfig(), nil)
	if a.Containers[0].CPURec != minCPU || a.Containers[0].MemRec != minMem {
		t.Errorf("minimums not applied: %+v", a.Containers[0])
	}
	none := Evaluate(w, nil, DefaultConfig(), nil)
	if none.Status != NoData || ids(none) != "RC007" {
		t.Errorf("no samples: %s %s", none.Status, ids(none))
	}
}

func TestSnapshotIsFlagged(t *testing.T) {
	w := workload(t, "Deployment", "api", "            requests: {cpu: 150m, memory: 700Mi}\n", 1)
	s := sample("api-5d8f7c9b6-aaaaa", 120, 600*mib)
	s.Source = usage.Snapshot
	a := Evaluate(w, map[string][]usage.Sample{"app": {s}}, DefaultConfig(), nil)
	if !strings.Contains(ids(a), "RC008") {
		t.Errorf("findings = %s", ids(a))
	}
}

func TestMatch(t *testing.T) {
	ws := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "api"},
		{Kind: "Deployment", Namespace: "shop", Name: "api-gateway"},
		{Kind: "StatefulSet", Namespace: "shop", Name: "db"},
		{Kind: "DaemonSet", Namespace: "shop", Name: "agent"},
		{Kind: "Pod", Namespace: "shop", Name: "debug"},
		{Kind: "Deployment", Namespace: "other", Name: "api"},
	}
	pods := map[string]int{ // pod name -> expected workload index, -1 for none
		"api-5d8f7c9b6-x2k4j":         0,
		"api-gateway-6c9d8f7b5-q9w8z": 1,
		"db-0":                        2,
		"db-12":                       2,
		"agent-x2k4j":                 3,
		"debug":                       4,
		"nightly-29183-abcde":         -1,
		"api-gateway-extra":           -1, // words are not generated names
		"api-6c9d8f-x2k4j":            0,  // a shorter ReplicaSet hash
		"api-5d8f7c9b3-x2k4j":         -1, // 3 is not in the alphabet
	}
	var samples []usage.Sample
	for pod := range pods {
		samples = append(samples, usage.Sample{Namespace: "shop", Pod: pod, Container: "c"})
	}
	m := Match(ws, samples)
	want := map[int]int{}
	unmatched := 0
	for _, idx := range pods {
		if idx < 0 {
			unmatched++
		} else {
			want[idx]++
		}
	}
	for idx, n := range want {
		if got := len(m.ByWorkload[idx]["c"]); got != n {
			t.Errorf("workload %d (%s): %d samples, want %d", idx, ws[idx].Name, got, n)
		}
	}
	if m.Unmatched != unmatched {
		t.Errorf("unmatched = %d, want %d", m.Unmatched, unmatched)
	}
	if len(m.ByWorkload[5]) != 0 {
		t.Error("a pod of another namespace must not match")
	}
}

func TestBytes(t *testing.T) {
	for in, want := range map[int64]string{720 * mib: "720Mi", 2 << 30: "2Gi", 1536 * mib: "1.5Gi", 16 * mib: "16Mi", 512 * 1024: "512Ki"} {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestCatalogHasEveryRule(t *testing.T) {
	for _, id := range []string{"RC001", "RC002", "RC003", "RC004", "RC005", "RC006", "RC007", "RC008", "RC009"} {
		if r, ok := Lookup(id); !ok || r.Title == "" || r.Why == "" || r.Fix == "" {
			t.Errorf("%s is missing or incomplete", id)
		}
	}
}

// generatedName builds a pod name the way the apiserver does: the base is cut at 58 characters
// and five random ones are added.
func generatedName(base, random string) string {
	if len(base) > 58 {
		base = base[:58]
	}
	return base + random
}

func TestMatchPodsWithTruncatedNames(t *testing.T) {
	// 49 characters: "<name>-<10-character hash>-" is 61, so the apiserver cuts it at 58 and the
	// pod name loses the last separator and the end of the hash.
	long := "reports-aggregation-daily-summary-service-prod-v2"
	if len(long) != 49 {
		t.Fatalf("test name has %d characters", len(long))
	}
	longer := strings.Repeat("x", 70) // the cut falls inside the name itself
	ws := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: long},
		{Kind: "DaemonSet", Namespace: "shop", Name: longer},
		{Kind: "Deployment", Namespace: "shop", Name: "reports"},
	}
	cases := []struct {
		name string
		pod  string
		want int // index of the workload that must claim the pod, -1 for none
	}{
		{"10-character hash, cut", generatedName(long+"-5d8f7c9b6c-", "x2k4j"), 0},
		{"9-character hash, cut", generatedName(long+"-6c9d8f7b5-", "q9w8z"), 0},
		{"cut inside the DaemonSet name", generatedName(longer+"-", "x2k4j"), 1},
		{"a vowel is not in the alphabet", generatedName(long+"-5d8f7c9b6c-", "x2k4e"), -1},
		{"a word is not a generated name", "reports-aggregation-daily-summary-service-prod-v2-extra-words-abcdefgh", -1},
		{"the short workload still matches", "reports-5d8f7c9b6-x2k4j", 2},
	}
	for _, c := range cases {
		if c.name != "a word is not a generated name" && c.name != "the short workload still matches" && len(c.pod) != 63 {
			t.Fatalf("%s: pod has %d characters, want 63", c.name, len(c.pod))
		}
		m := Match(ws, []usage.Sample{{Namespace: "shop", Pod: c.pod, Container: "c"}})
		got := -1
		for idx, byContainer := range m.ByWorkload {
			if len(byContainer["c"]) == 1 {
				got = idx
			}
		}
		if got != c.want {
			t.Errorf("%s: pod %q matched workload %d, want %d", c.name, c.pod, got, c.want)
		}
	}
}

func scaler(kind, target string, max int32, metrics ...inventory.ScaleMetric) inventory.Autoscaler {
	return inventory.Autoscaler{Kind: kind, Name: "api", Namespace: "shop", TargetKind: "Deployment", TargetName: target, Metrics: metrics, MaxReplicas: max}
}

func cpuTarget(percent int) inventory.ScaleMetric {
	return inventory.ScaleMetric{Resource: "cpu", Target: percent}
}

func TestScalerWarnsWhenTheRecommendationPassesTheTarget(t *testing.T) {
	// request 500m. Usage 38m: recommended 50m, so the HPA would see 38/50 = 76%.
	w := workload(t, "Deployment", "api", "            requests: {cpu: 500m, memory: 512Mi}\n", 2)
	eval := func(cpu float64, scalers ...inventory.Autoscaler) Assessment {
		return Evaluate(w, map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-x2k4j", cpu, 300*mib)}}, DefaultConfig(), scalers)
	}

	got := eval(38, scaler("ScaledObject", "api", 10, cpuTarget(75)))
	if !strings.Contains(ids(got), "RC009") || !got.ScalesOut || got.AutoscaledBy[0] != "ScaledObject/api" {
		t.Fatalf("findings %q, scalesOut %v, by %v", ids(got), got.ScalesOut, got.AutoscaledBy)
	}
	var msg string
	for _, f := range got.Findings {
		if f.Rule == "RC009" {
			msg = f.Message
		}
	}
	for _, want := range []string{"target 75%", "8% of the request today", "76% at the recommended request", "up to 10 replicas"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}

	// Usage 20m: recommended 30m, utilization would be 67%, under the 75% target.
	if got := eval(20, scaler("HorizontalPodAutoscaler", "api", 0, cpuTarget(75))); strings.Contains(ids(got), "RC009") || got.ScalesOut {
		t.Errorf("67%% is under the target, but got %q", ids(got))
	}
	// The same usage under a lower target does pass it.
	if got := eval(20, scaler("HorizontalPodAutoscaler", "api", 0, cpuTarget(60))); !strings.Contains(ids(got), "RC009") {
		t.Errorf("67%% is above a 60%% target, got %q", ids(got))
	}
	// No autoscaler, or one for another workload: nothing to say.
	if got := eval(38); strings.Contains(ids(got), "RC009") {
		t.Error("no autoscaler")
	}
}

func TestScalerOnlyMattersWhenARequestWouldFall(t *testing.T) {
	// Request 150m, usage 120m: close to the recommendation (140m), nothing is lowered.
	w := workload(t, "Deployment", "api", "            requests: {cpu: 150m, memory: 200Mi}\n", 1)
	got := Evaluate(w, map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-x2k4j", 120, 150*mib)}}, DefaultConfig(),
		[]inventory.Autoscaler{scaler("ScaledObject", "api", 5, cpuTarget(50))})
	if strings.Contains(ids(got), "RC009") {
		t.Errorf("no request is lowered, so the scaler sees the same utilization: %q", ids(got))
	}
}

func TestScalerOnMemoryAndOnOneContainer(t *testing.T) {
	// Memory request 2Gi, usage 600Mi: recommended 720Mi, utilization 83% against a 70% target.
	w := workload(t, "Deployment", "api", "            requests: {cpu: 150m, memory: 2Gi}\n", 1)
	samples := map[string][]usage.Sample{"app": {sample("api-5d8f7c9b6-x2k4j", 120, 600*mib)}}
	mem := inventory.ScaleMetric{Resource: "memory", Target: 70}
	if got := Evaluate(w, samples, DefaultConfig(), []inventory.Autoscaler{scaler("HorizontalPodAutoscaler", "api", 0, mem)}); !strings.Contains(ids(got), "RC009") || got.ScalesOut {
		t.Errorf("memory: findings %q, scalesOut %v (scalesOut is only for CPU)", ids(got), got.ScalesOut)
	}
	// A metric on another container does not see this one.
	other := inventory.ScaleMetric{Resource: "memory", Target: 70, Container: "sidecar"}
	if got := Evaluate(w, samples, DefaultConfig(), []inventory.Autoscaler{scaler("HorizontalPodAutoscaler", "api", 0, other)}); strings.Contains(ids(got), "RC009") {
		t.Errorf("another container: %q", ids(got))
	}
}

func TestScalersFor(t *testing.T) {
	w := inventory.Workload{Kind: "Deployment", Namespace: "shop", Name: "api"}
	all := []inventory.Autoscaler{
		{Namespace: "shop", TargetKind: "Deployment", TargetName: "api"},
		{Namespace: "shop", TargetKind: "Deployment", TargetName: "web"},
		{Namespace: "other", TargetKind: "Deployment", TargetName: "api"},
		{Namespace: "shop", TargetKind: "StatefulSet", TargetName: "api"},
	}
	if got := ScalersFor(w, all); len(got) != 1 || got[0].TargetName != "api" {
		t.Errorf("scalers = %+v", got)
	}
	// The same HPA scanned twice counts once.
	twice := append(all, inventory.Autoscaler{Kind: "HorizontalPodAutoscaler", Name: "h", Namespace: "shop", TargetKind: "Deployment", TargetName: "api"})
	twice = append(twice, twice[len(twice)-1])
	if got := ScalersFor(w, twice); len(got) != 2 {
		t.Errorf("a duplicate must be dropped, got %d", len(got))
	}
}
