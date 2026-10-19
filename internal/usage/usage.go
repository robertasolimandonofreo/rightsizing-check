// Package usage reads observed CPU and memory usage from files, so the tool never needs access to
// a cluster or a metrics server. Three formats are understood: the JSON that the Prometheus HTTP
// API returns for an instant query, the output of kubectl top, and a plain CSV.
package usage

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Source says how trustworthy the numbers are: a percentile over a window is a measurement of
// the workload, a snapshot is one instant.
type Source string

const (
	// Window means the values came from a query over a time window (Prometheus) or a CSV.
	Window Source = "window"
	// Snapshot means the values came from kubectl top: the usage at one instant.
	Snapshot Source = "snapshot"
)

// Sample is the usage of one container of one pod. A missing value is negative.
type Sample struct {
	Namespace string
	Pod       string
	Container string
	CPUMillis float64
	MemBytes  float64
	Source    Source
}

func missing() Sample { return Sample{CPUMillis: -1, MemBytes: -1} }

// Dataset is everything read from the usage files.
type Dataset struct {
	Samples []Sample
}

// Merge adds samples, combining a CPU sample and a memory sample of the same container (which
// the Prometheus format delivers in separate files).
func (d *Dataset) Merge(in []Sample) {
	index := map[[3]string]int{}
	for i, s := range d.Samples {
		index[[3]string{s.Namespace, s.Pod, s.Container}] = i
	}
	for _, s := range in {
		key := [3]string{s.Namespace, s.Pod, s.Container}
		i, ok := index[key]
		if !ok {
			index[key] = len(d.Samples)
			d.Samples = append(d.Samples, s)
			continue
		}
		cur := &d.Samples[i]
		if s.CPUMillis >= 0 {
			cur.CPUMillis = s.CPUMillis
		}
		if s.MemBytes >= 0 {
			cur.MemBytes = s.MemBytes
		}
		if s.Source == Snapshot {
			cur.Source = Snapshot
		}
	}
}

// Metric picks which value a Prometheus result holds.
type Metric int

const (
	// CPU values are in cores, as rate(container_cpu_usage_seconds_total) returns them.
	CPU Metric = iota
	// Memory values are in bytes, as container_memory_working_set_bytes returns them.
	Memory
)

// Prometheus parses the JSON of an instant query (GET /api/v1/query). Every series needs the
// labels namespace, pod and container; series for the pause container, or without a container
// (the pod total), are ignored.
func Prometheus(r io.Reader, m Metric) ([]Sample, error) {
	var doc struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("not a Prometheus query result: %w", err)
	}
	if doc.Status != "success" {
		return nil, fmt.Errorf("the Prometheus query did not succeed (status %q) %s", doc.Status, doc.Error)
	}
	if doc.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected an instant query (resultType vector), got %q: use /api/v1/query, not query_range", doc.Data.ResultType)
	}
	var out []Sample
	for _, series := range doc.Data.Result {
		l := series.Metric
		if l["container"] == "" || l["container"] == "POD" || l["pod"] == "" {
			continue
		}
		raw, ok := series.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		s := missing()
		s.Namespace, s.Pod, s.Container, s.Source = l["namespace"], l["pod"], l["container"], Window
		if m == CPU {
			s.CPUMillis = v * 1000
		} else {
			s.MemBytes = v
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("the Prometheus result has no series with the labels namespace, pod and container")
	}
	return out, nil
}

// Top parses the output of kubectl top pods --containers, with or without -A. Without a
// NAMESPACE column, defaultNamespace is used.
func Top(r io.Reader, defaultNamespace string) ([]Sample, error) {
	sc := bufio.NewScanner(r)
	var cols []string
	var out []Sample
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if cols == nil {
			cols = fields
			if len(fields) < 4 || fields[len(fields)-1] != "MEMORY(bytes)" || fields[len(fields)-2] != "CPU(cores)" {
				return nil, errors.New("not kubectl top output: expected the columns POD NAME CPU(cores) MEMORY(bytes), from kubectl top pods --containers")
			}
			continue
		}
		ns, rest := defaultNamespace, fields
		if cols[0] == "NAMESPACE" {
			ns, rest = fields[0], fields[1:]
		}
		if len(rest) != 4 {
			return nil, fmt.Errorf("unexpected line %q", sc.Text())
		}
		cpu, err := resource.ParseQuantity(rest[2])
		if err != nil {
			return nil, fmt.Errorf("bad CPU value %q", rest[2])
		}
		mem, err := resource.ParseQuantity(rest[3])
		if err != nil {
			return nil, fmt.Errorf("bad memory value %q", rest[3])
		}
		out = append(out, Sample{Namespace: ns, Pod: rest[0], Container: rest[1],
			CPUMillis: float64(cpu.MilliValue()), MemBytes: float64(mem.Value()), Source: Snapshot})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cols == nil {
		return nil, errors.New("empty kubectl top output")
	}
	return out, nil
}

// CSV parses a file with the header namespace,pod,container,cpu_millis,memory_bytes. A blank
// value means that number is not known.
func CSV(r io.Reader) ([]Sample, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("empty CSV")
	}
	want := []string{"namespace", "pod", "container", "cpu_millis", "memory_bytes"}
	head := rows[0]
	if len(head) != len(want) {
		return nil, fmt.Errorf("the CSV header must be %s", strings.Join(want, ","))
	}
	for i := range want {
		if strings.TrimSpace(head[i]) != want[i] {
			return nil, fmt.Errorf("the CSV header must be %s", strings.Join(want, ","))
		}
	}
	var out []Sample
	for n, row := range rows[1:] {
		s := missing()
		s.Namespace, s.Pod, s.Container, s.Source = row[0], row[1], row[2], Window
		for i, dst := range []*float64{&s.CPUMillis, &s.MemBytes} {
			text := strings.TrimSpace(row[3+i])
			if text == "" {
				continue
			}
			v, err := strconv.ParseFloat(text, 64)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("line %d: bad number %q", n+2, text)
			}
			*dst = v
		}
		out = append(out, s)
	}
	return out, nil
}
