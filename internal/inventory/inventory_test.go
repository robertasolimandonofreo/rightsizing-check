package inventory

import (
	"strings"
	"testing"
)

func load(t *testing.T, doc string) *Inventory {
	t.Helper()
	inv := &Inventory{Skipped: map[string]int{}}
	if err := inv.Add("f.yaml", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestKindsAndDefaults(t *testing.T) {
	inv := load(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - {name: a, image: a:1, resources: {requests: {cpu: 250m}}}
        - {name: b, image: b:1, resources: {requests: {cpu: "1"}}}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec:
  template: {spec: {containers: [{name: c, image: c:1}]}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  schedule: "0 1 * * *"
  jobTemplate: {spec: {template: {spec: {containers: [{name: d, image: d:1}]}}}}
---
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: owned}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: ignored}
`)
	if len(inv.Workloads) != 3 {
		t.Fatalf("workloads = %d", len(inv.Workloads))
	}
	byName := map[string]Workload{}
	for _, w := range inv.Workloads {
		byName[w.Name] = w
	}
	web := byName["web"]
	if web.ID() != "shop/deployment/web" || web.Replicas != 3 || web.CPUMillis != 1250 || web.RequestedCPU() != 3750 || !web.LongRunning {
		t.Errorf("web = %+v", web)
	}
	if db := byName["db"]; db.Namespace != "default" || db.Replicas != 1 {
		t.Errorf("db defaults = %+v", db)
	}
	if job := byName["nightly"]; job.LongRunning || job.Pod.Containers[0].Image != "d:1" {
		t.Errorf("cronjob = %+v", job)
	}
	if inv.Skipped["ReplicaSet"] != 1 {
		t.Errorf("skipped = %v", inv.Skipped)
	}
}

func TestListJSONAndInitContainers(t *testing.T) {
	inv := load(t, `{"apiVersion":"v1","kind":"List","items":[
	  {"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{
	    "initContainers":[{"name":"i","image":"i:1"}],
	    "containers":[{"name":"c","image":"c:1","resources":{"requests":{"cpu":"2"}}}]}}]}`)
	if len(inv.Workloads) != 1 {
		t.Fatalf("workloads = %d", len(inv.Workloads))
	}
	w := inv.Workloads[0]
	cs := w.Containers()
	if len(cs) != 2 || cs[0].Name != "i" || w.CPUMillis != 2000 {
		t.Errorf("containers = %v, cpu = %d", cs, w.CPUMillis)
	}
}

func TestInvalidDocumentIsAnError(t *testing.T) {
	inv := &Inventory{Skipped: map[string]int{}}
	if err := inv.Add("", []byte("kind: Deployment\nspec: [unterminated")); err == nil {
		t.Error("expected an error for a broken document")
	}
	if err := inv.Add("", []byte("# only a comment\n---\n")); err != nil {
		t.Errorf("empty documents should be fine: %v", err)
	}
}

func TestPathsWalksDirectoriesAndStdin(t *testing.T) {
	inv, err := Paths([]string{"../../testdata", "-"}, strings.NewReader("apiVersion: v1\nkind: Pod\nmetadata: {name: from-stdin}\nspec: {containers: [{name: c, image: c:1}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Workloads) != 7 {
		t.Errorf("workloads = %d, want 7 (6 in sample.yaml and 1 from stdin)", len(inv.Workloads))
	}
	for i := 1; i < len(inv.Workloads); i++ {
		if inv.Workloads[i-1].ID() > inv.Workloads[i].ID() {
			t.Error("workloads are not sorted by ID")
		}
	}
}
