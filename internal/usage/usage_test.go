package usage

import (
	"strings"
	"testing"
)

const promCPU = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1760000000,"0.125"]},
 {"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"POD"},"value":[1760000000,"0.001"]},
 {"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j"},"value":[1760000000,"0.2"]},
 {"metric":{"namespace":"shop","pod":"bad","container":"app"},"value":[1760000000,"NaN"]}]}}`

func TestPrometheusAndMerge(t *testing.T) {
	cpu, err := Prometheus(strings.NewReader(promCPU), CPU)
	if err != nil {
		t.Fatal(err)
	}
	if len(cpu) != 1 || cpu[0].CPUMillis != 125 || cpu[0].MemBytes != -1 {
		t.Fatalf("cpu = %+v", cpu)
	}
	mem, err := Prometheus(strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"629145600"]}]}}`), Memory)
	if err != nil {
		t.Fatal(err)
	}
	var d Dataset
	d.Merge(cpu)
	d.Merge(mem)
	if len(d.Samples) != 1 || d.Samples[0].CPUMillis != 125 || d.Samples[0].MemBytes != 629145600 {
		t.Errorf("merged = %+v", d.Samples)
	}
}

func TestPrometheusErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":    "hello",
		"failed":      `{"status":"error","error":"bad query"}`,
		"range query": `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"no labels":   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"x"},"value":[1,"1"]}]}}`,
	} {
		if _, err := Prometheus(strings.NewReader(in), CPU); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTop(t *testing.T) {
	all := "NAMESPACE   POD                   NAME   CPU(cores)   MEMORY(bytes)\nshop        api-5d8f7c9b6-x2k4j   app    12m          130Mi\nshop        api-5d8f7c9b6-x2k4j   proxy  1            1Gi\n"
	got, err := Top(strings.NewReader(all), "default")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].Namespace != "shop" || got[0].CPUMillis != 12 || got[0].MemBytes != 130*(1<<20) || got[0].Source != Snapshot {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].CPUMillis != 1000 || got[1].MemBytes != 1<<30 {
		t.Errorf("second = %+v", got[1])
	}
	single := "POD   NAME   CPU(cores)   MEMORY(bytes)\napi-1   app   5m   10Mi\n"
	got, err = Top(strings.NewReader(single), "web")
	if err != nil || got[0].Namespace != "web" {
		t.Errorf("default namespace: %+v, %v", got, err)
	}
	if _, err := Top(strings.NewReader("NAME CPU\nx 1\n"), "d"); err == nil {
		t.Error("expected an error for a header that is not kubectl top")
	}
}

func TestCSV(t *testing.T) {
	got, err := CSV(strings.NewReader("namespace,pod,container,cpu_millis,memory_bytes\nshop,api-1,app,125,\nshop,api-1,proxy,,1048576\n"))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].CPUMillis != 125 || got[0].MemBytes != -1 || got[1].CPUMillis != -1 || got[1].MemBytes != 1048576 {
		t.Errorf("rows = %+v", got)
	}
	for _, bad := range []string{"", "a,b,c\n1,2,3\n", "namespace,pod,container,cpu_millis,memory_bytes\nshop,p,c,abc,1\n"} {
		if _, err := CSV(strings.NewReader(bad)); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
}
