// Package rightsize compares what each container requests with what it uses, and recommends a
// request that leaves headroom.
package rightsize

import (
	"fmt"
	"math"
	"sort"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

// Status is the overall answer for a workload.
type Status string

const (
	// Oversized means at least one request is far above the usage, and nothing is at risk.
	Oversized Status = "oversized"
	// Undersized means a request or limit is below the usage. It outranks oversized: a workload
	// with a risk is not a candidate to shrink.
	Undersized Status = "undersized"
	// OK means the requests are close to the usage.
	OK Status = "ok"
	// NoData means no container of the workload has a usage sample.
	NoData Status = "no-data"
)

// Config tunes the recommendation and the thresholds.
type Config struct {
	// CPUHeadroom and MemHeadroom are added to the observed usage (0.15 is 15%).
	CPUHeadroom float64
	MemHeadroom float64
	// Slack is how many times the recommendation a request may be before it is called inflated.
	Slack float64
}

// DefaultConfig returns the thresholds the README documents.
func DefaultConfig() Config { return Config{CPUHeadroom: 0.15, MemHeadroom: 0.20, Slack: 1.5} }

// Finding is one rule that matched.
type Finding struct {
	Rule      string `json:"rule"`
	Kind      Kind   `json:"kind"`
	Container string `json:"container,omitempty"`
	Message   string `json:"message"`
}

// Container is the comparison for one container. Missing numbers are zero for requests and
// limits (not set) and negative for usage (not observed).
type Container struct {
	Name        string       `json:"name"`
	Pods        int          `json:"podsObserved"`
	Source      usage.Source `json:"source,omitempty"`
	CPURequest  int64        `json:"cpuRequestMillis"`
	CPULimit    int64        `json:"cpuLimitMillis"`
	MemRequest  int64        `json:"memoryRequestBytes"`
	MemLimit    int64        `json:"memoryLimitBytes"`
	CPUUsage    float64      `json:"cpuUsageMillis"`
	MemUsage    float64      `json:"memoryUsageBytes"`
	CPURec      int64        `json:"cpuRecommendedMillis"`
	MemRec      int64        `json:"memoryRecommendedBytes"`
	CPUReclaim  int64        `json:"cpuReclaimableMillis"`
	MemReclaim  int64        `json:"memoryReclaimableBytes"`
	HasUsageCPU bool         `json:"-"`
	HasUsageMem bool         `json:"-"`
}

// Assessment is the result for one workload.
type Assessment struct {
	Workload   inventory.Workload `json:"-"`
	Status     Status             `json:"status"`
	Containers []Container        `json:"containers"`
	Findings   []Finding          `json:"findings"`
	// AutoscaledBy names the HPAs and KEDA ScaledObjects that target the workload.
	AutoscaledBy []string `json:"autoscaledBy,omitempty"`
	// ScalesOut is true when lowering the CPU request to the recommendation would push an HPA
	// above its target (RC009 on CPU).
	ScalesOut bool `json:"-"`
}

// ReclaimableCPU is the CPU that the recommendation frees across all replicas, in millicores.
// DaemonSets, Jobs and CronJobs have no replica count and count once per pod.
func (a Assessment) ReclaimableCPU() int64 {
	return a.reclaim(func(c Container) int64 { return c.CPUReclaim })
}

// ReclaimableMemory is the memory the recommendation frees across all replicas, in bytes.
func (a Assessment) ReclaimableMemory() int64 {
	return a.reclaim(func(c Container) int64 { return c.MemReclaim })
}

func (a Assessment) reclaim(f func(Container) int64) int64 {
	var total int64
	for _, c := range a.Containers {
		total += f(c)
	}
	if a.Workload.LongRunning {
		total *= int64(a.Workload.Replicas)
	}
	return total
}

const (
	mi         = 1 << 20
	minCPU     = 10      // millicores: the smallest request worth recommending
	minMem     = 16 * mi // bytes
	memStep    = 4 * mi
	limitRatio = 0.9 // usage at or above this share of the limit is close to an OOMKill
)

func ceilTo(v float64, step int64) int64 {
	return int64(math.Ceil(v/float64(step))) * step
}

// Evaluate compares one workload with the samples that Match assigned to it.
func Evaluate(w inventory.Workload, samples map[string][]usage.Sample, cfg Config, scalers []inventory.Autoscaler) Assessment {
	a := Assessment{Workload: w, Findings: []Finding{}}
	add := func(rule string, container, format string, args ...any) {
		r, _ := Lookup(rule)
		a.Findings = append(a.Findings, Finding{Rule: rule, Kind: r.Kind, Container: container, Message: fmt.Sprintf(format, args...)})
	}

	observed, snapshot := 0, false
	for _, pc := range w.Pod.Containers {
		c := Container{
			Name:       pc.Name,
			CPURequest: pc.Resources.Requests.Cpu().MilliValue(),
			CPULimit:   pc.Resources.Limits.Cpu().MilliValue(),
			MemRequest: pc.Resources.Requests.Memory().Value(),
			MemLimit:   pc.Resources.Limits.Memory().Value(),
			CPUUsage:   -1,
			MemUsage:   -1,
		}
		// The worst replica decides: one busy pod needs the request, even if its siblings idle.
		pods := map[string]bool{}
		for _, s := range samples[pc.Name] {
			pods[s.Pod] = true
			if s.CPUMillis >= 0 {
				c.CPUUsage, c.HasUsageCPU = math.Max(c.CPUUsage, s.CPUMillis), true
			}
			if s.MemBytes >= 0 {
				c.MemUsage, c.HasUsageMem = math.Max(c.MemUsage, s.MemBytes), true
			}
			c.Source = s.Source
			if s.Source == usage.Snapshot {
				snapshot = true
			}
		}
		c.Pods = len(pods)
		if !c.HasUsageCPU && !c.HasUsageMem {
			add("RC007", c.Name, "container %q: no usage found", c.Name)
			a.Containers = append(a.Containers, c)
			continue
		}
		observed++
		cpuFindings(&c, cfg, add)
		memFindings(&c, cfg, add)
		a.Containers = append(a.Containers, c)
	}
	if snapshot {
		add("RC008", "", "usage comes from kubectl top, one instant: treat the recommendation as a hint")
	}
	scalerFindings(&a, scalers, add)

	a.Status = status(a, observed)
	sortFindings(a.Findings)
	return a
}

type adder func(rule string, container, format string, args ...any)

func cpuFindings(c *Container, cfg Config, add adder) {
	if !c.HasUsageCPU {
		return
	}
	c.CPURec = ceilTo(math.Max(c.CPUUsage*(1+cfg.CPUHeadroom), minCPU), minCPU)
	switch {
	case c.CPURequest == 0:
		add("RC006", c.Name, "container %q: no CPU request; recommended %dm", c.Name, c.CPURec)
	case c.CPUUsage > float64(c.CPURequest):
		add("RC003", c.Name, "container %q: CPU usage %.0fm is above the request %dm; recommended %dm", c.Name, c.CPUUsage, c.CPURequest, c.CPURec)
	case float64(c.CPURequest) > cfg.Slack*float64(c.CPURec):
		c.CPUReclaim = c.CPURequest - c.CPURec
		add("RC001", c.Name, "container %q: CPU request %dm, usage %.0fm; recommended %dm (frees %dm per pod)", c.Name, c.CPURequest, c.CPUUsage, c.CPURec, c.CPUReclaim)
	}
}

func memFindings(c *Container, cfg Config, add adder) {
	if !c.HasUsageMem {
		return
	}
	c.MemRec = ceilTo(math.Max(c.MemUsage*(1+cfg.MemHeadroom), minMem), memStep)
	if c.MemLimit > 0 && c.MemUsage >= limitRatio*float64(c.MemLimit) {
		add("RC005", c.Name, "container %q: memory usage %s is within 10%% of the limit %s", c.Name, Bytes(int64(c.MemUsage)), Bytes(c.MemLimit))
	}
	switch {
	case c.MemRequest == 0:
		add("RC006", c.Name, "container %q: no memory request; recommended %s", c.Name, Bytes(c.MemRec))
	case c.MemUsage > float64(c.MemRequest):
		add("RC004", c.Name, "container %q: memory usage %s is above the request %s; recommended %s", c.Name, Bytes(int64(c.MemUsage)), Bytes(c.MemRequest), Bytes(c.MemRec))
	case float64(c.MemRequest) > cfg.Slack*float64(c.MemRec):
		c.MemReclaim = c.MemRequest - c.MemRec
		add("RC002", c.Name, "container %q: memory request %s, usage %s; recommended %s (frees %s per pod)", c.Name, Bytes(c.MemRequest), Bytes(int64(c.MemUsage)), Bytes(c.MemRec), Bytes(c.MemReclaim))
	}
}

func status(a Assessment, observed int) Status {
	if observed == 0 {
		return NoData
	}
	waste := false
	for _, f := range a.Findings {
		switch f.Kind {
		case Risk:
			return Undersized
		case Waste:
			waste = true
		}
	}
	// A missing request is not waste, but the workload is not "ok" either: it is unbounded.
	for _, f := range a.Findings {
		if f.Rule == "RC006" {
			return Undersized
		}
	}
	if waste {
		return Oversized
	}
	return OK
}

func sortFindings(f []Finding) {
	rank := map[Kind]int{Risk: 0, Waste: 1, Caveat: 2, Data: 3}
	sort.SliceStable(f, func(i, j int) bool {
		if rank[f[i].Kind] != rank[f[j].Kind] {
			return rank[f[i].Kind] < rank[f[j].Kind]
		}
		return f[i].Rule < f[j].Rule
	})
}

// Bytes renders a size the way a manifest would write it (Mi or Gi).
func Bytes(n int64) string {
	const gi = 1 << 30
	switch {
	case n >= gi && n%(gi/4) == 0:
		return trimZero(float64(n)/gi) + "Gi"
	case n >= mi:
		return fmt.Sprintf("%dMi", int64(math.Round(float64(n)/mi)))
	}
	return fmt.Sprintf("%dKi", n/1024)
}

func trimZero(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	for s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

// ScalersFor returns the autoscalers that target the workload, in its namespace.
func ScalersFor(w inventory.Workload, all []inventory.Autoscaler) []inventory.Autoscaler {
	var out []inventory.Autoscaler
	seen := map[string]bool{}
	for _, s := range all {
		if s.Namespace != w.Namespace || s.TargetName != w.Name || s.TargetKind != w.Kind {
			continue
		}
		// The same object can be scanned twice (a manifest and a copy exported from the cluster).
		if key := s.Kind + "/" + s.Name; !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

// scalerFindings checks what lowering the requests would do to an autoscaler that scales on
// utilization. The utilization it sees is the usage of the pod over the sum of its requests, so
// the question is whether that number, at the recommended requests, passes the target.
func scalerFindings(a *Assessment, scalers []inventory.Autoscaler, add adder) {
	for _, sc := range scalers {
		a.AutoscaledBy = append(a.AutoscaledBy, sc.Kind+"/"+sc.Name)
		for _, m := range sc.Metrics {
			var usageSum, now, next float64
			for _, c := range a.Containers {
				if m.Container != "" && m.Container != c.Name {
					continue
				}
				switch m.Resource {
				case "cpu":
					if !c.HasUsageCPU || c.CPURequest == 0 {
						continue
					}
					usageSum += c.CPUUsage
					now += float64(c.CPURequest)
					next += float64(c.CPURequest - c.CPUReclaim)
				case "memory":
					if !c.HasUsageMem || c.MemRequest == 0 {
						continue
					}
					usageSum += c.MemUsage
					now += float64(c.MemRequest)
					next += float64(c.MemRequest - c.MemReclaim)
				}
			}
			if now == 0 || next >= now {
				continue // no request would be lowered, so the scaler sees the same utilization
			}
			after := 100 * usageSum / next
			if after <= float64(m.Target) {
				continue
			}
			upTo := ""
			if sc.MaxReplicas > 0 {
				upTo = fmt.Sprintf(", up to %d replicas", sc.MaxReplicas)
			}
			add("RC009", "", "%s %s scales on %s utilization (target %d%%): it sees %.0f%% of the request today and would see %.0f%% at the recommended request, so it would scale out%s",
				sc.Kind, sc.Name, m.Resource, m.Target, 100*usageSum/now, after, upTo)
			if m.Resource == "cpu" {
				a.ScalesOut = true
			}
		}
	}
}
