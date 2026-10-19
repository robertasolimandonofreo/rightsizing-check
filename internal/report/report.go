// Package report turns assessments into a table, JSON or Markdown.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/rightsize"
)

// SchemaVersion changes when the JSON layout changes in a way that breaks readers.
const SchemaVersion = 1

// Options tune the output.
type Options struct {
	Verbose bool
	Version string
	// Unmatched is the number of pods in the usage files that no workload claimed.
	Unmatched int
	// CPUPrice and MemPrice are per vCPU-month and per GiB-month; zero hides the cost line.
	CPUPrice float64
	MemPrice float64
	Config   rightsize.Config
}

// Summary counts the statuses and adds up what the recommendations would free. The sums cover
// long-running workloads (Deployments, StatefulSets, Pods), multiplied by their replicas:
// DaemonSets and Jobs have no replica count, so they are listed per pod and left out.
type Summary struct {
	Total              int                      `json:"total"`
	Counts             map[rightsize.Status]int `json:"counts"`
	RequestedCPUMillis int64                    `json:"requestedCPUMillis"`
	RequestedMemory    int64                    `json:"requestedMemoryBytes"`
	ReclaimCPUMillis   int64                    `json:"reclaimableCPUMillis"`
	// ReclaimCPUUnderHPA is the part of the reclaimable CPU on workloads whose HPA would scale
	// out if the request came down to the recommendation (RC009).
	ReclaimCPUUnderHPA int64 `json:"reclaimableCPUMillisWithScaleOut"`
	// ReclaimCPUUnderVPA is the part on workloads whose requests a VPA sets (RC010).
	ReclaimCPUUnderVPA int64 `json:"reclaimableCPUMillisUnderVPA"`
	ReclaimMemory      int64 `json:"reclaimableMemoryBytes"`
	ReclaimCPUPercent  int   `json:"reclaimableCPUPercent"`
	ReclaimMemPercent  int   `json:"reclaimableMemoryPercent"`
	UnmatchedPods      int   `json:"unmatchedPods,omitempty"`
}

// Summarize computes the counts and the sums. Only workloads with usage data enter the requested
// totals, so the percentages say how much of what was measured is reclaimable.
func Summarize(as []rightsize.Assessment, unmatched int) Summary {
	s := Summary{Total: len(as), Counts: map[rightsize.Status]int{}, UnmatchedPods: unmatched}
	for _, a := range as {
		s.Counts[a.Status]++
		if !a.Workload.LongRunning || a.Status == rightsize.NoData {
			continue
		}
		for _, c := range a.Containers {
			if !c.HasUsageCPU && !c.HasUsageMem {
				continue
			}
			s.RequestedCPUMillis += c.CPURequest * int64(a.Workload.Replicas)
			s.RequestedMemory += c.MemRequest * int64(a.Workload.Replicas)
		}
		s.ReclaimCPUMillis += a.ReclaimableCPU()
		if a.ScalesOut {
			s.ReclaimCPUUnderHPA += a.ReclaimableCPU()
		}
		if a.VPAControlled {
			s.ReclaimCPUUnderVPA += a.ReclaimableCPU()
		}
		s.ReclaimMemory += a.ReclaimableMemory()
	}
	if s.RequestedCPUMillis > 0 {
		s.ReclaimCPUPercent = int(100 * s.ReclaimCPUMillis / s.RequestedCPUMillis)
	}
	if s.RequestedMemory > 0 {
		s.ReclaimMemPercent = int(100 * s.ReclaimMemory / s.RequestedMemory)
	}
	return s
}

// Order sorts the assessments for reading: what can be reclaimed first (the most CPU on top),
// then the workloads at risk, then the ones that are fine, then the ones without data.
func Order(as []rightsize.Assessment) []rightsize.Assessment {
	rank := map[rightsize.Status]int{rightsize.Oversized: 0, rightsize.Undersized: 1, rightsize.OK: 2, rightsize.NoData: 3}
	out := append([]rightsize.Assessment{}, as...)
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		if ci, cj := out[i].ReclaimableCPU(), out[j].ReclaimableCPU(); ci != cj {
			return ci > cj
		}
		return out[i].Workload.ID() < out[j].Workload.ID()
	})
	return out
}

func cores(m int64) string { return fmt.Sprintf("%.1f", float64(m)/1000) }

func gib(b int64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

func cpuCell(m int64) string {
	if m == 0 {
		return "-"
	}
	return fmt.Sprintf("%dm", m)
}

func cpuUse(v float64) string {
	if v < 0 {
		return "?"
	}
	return fmt.Sprintf("%.0fm", v)
}

func memCell(b int64) string {
	if b == 0 {
		return "-"
	}
	return rightsize.Bytes(b)
}

func memUse(v float64) string {
	if v < 0 {
		return "?"
	}
	return rightsize.Bytes(int64(v))
}

func recCPU(c rightsize.Container) string {
	if !c.HasUsageCPU {
		return "?"
	}
	return cpuCell(c.CPURec)
}

func recMem(c rightsize.Container) string {
	if !c.HasUsageMem {
		return "?"
	}
	return memCell(c.MemRec)
}

func ruleIDs(a rightsize.Assessment, container string) string {
	seen := map[string]bool{}
	out := ""
	for _, f := range a.Findings {
		if f.Container != container && f.Container != "" {
			continue
		}
		if f.Container == "" && container != "" {
			continue
		}
		if !seen[f.Rule] {
			seen[f.Rule] = true
			if out != "" {
				out += ","
			}
			out += f.Rule
		}
	}
	if out == "" {
		return "-"
	}
	return out
}

// Table writes the default report, one row per container.
func Table(out io.Writer, as []rightsize.Assessment, o Options) error {
	as = Order(as)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tWORKLOAD\tCONTAINER\tREPLICAS\tCPU REQ\tCPU USE\tCPU REC\tMEM REQ\tMEM USE\tMEM REC\tFINDINGS")
	for _, a := range as {
		replicas := "-"
		if a.Workload.LongRunning {
			replicas = fmt.Sprint(a.Workload.Replicas)
		}
		for _, c := range a.Containers {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Status, a.Workload.ID(), c.Name, replicas,
				cpuCell(c.CPURequest), cpuUse(c.CPUUsage), recCPU(c), memCell(c.MemRequest), memUse(c.MemUsage), recMem(c), ruleIDs(a, c.Name))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if o.Verbose {
		for _, a := range as {
			if len(a.Findings) == 0 {
				continue
			}
			fmt.Fprintf(out, "\n%s\n", a.Workload.ID())
			for _, f := range a.Findings {
				fmt.Fprintf(out, "  %s [%s] %s\n", f.Rule, f.Kind, f.Message)
			}
		}
	}
	return summaryText(out, Summarize(as, o.Unmatched), o)
}

func summaryText(out io.Writer, s Summary, o Options) error {
	c := s.Counts
	fmt.Fprintf(out, "\n%d workloads: %d oversized, %d undersized, %d ok, %d without data\n",
		s.Total, c[rightsize.Oversized], c[rightsize.Undersized], c[rightsize.OK], c[rightsize.NoData])
	if s.RequestedCPUMillis > 0 || s.RequestedMemory > 0 {
		fmt.Fprintf(out, "Reclaimable requests: %s vCPU (%d%% of the %s requested) and %s GiB (%d%% of %s).\n",
			cores(s.ReclaimCPUMillis), s.ReclaimCPUPercent, cores(s.RequestedCPUMillis),
			gib(s.ReclaimMemory), s.ReclaimMemPercent, gib(s.RequestedMemory))
		if s.ReclaimCPUUnderHPA > 0 {
			fmt.Fprintf(out, "Of the CPU, %s vCPU is on workloads whose HPA would scale out if the request came down to the recommendation (RC009): lower the target with it, or leave it.\n", cores(s.ReclaimCPUUnderHPA))
		}
		if s.ReclaimCPUUnderVPA > 0 {
			fmt.Fprintf(out, "Of the CPU, %s vCPU is on workloads whose requests a VPA sets (RC010): change them through the VPA, because an edit in the manifest will be overwritten.\n", cores(s.ReclaimCPUUnderVPA))
		}
		if o.CPUPrice > 0 || o.MemPrice > 0 {
			cost := float64(s.ReclaimCPUMillis)/1000*o.CPUPrice + float64(s.ReclaimMemory)/(1<<30)*o.MemPrice
			fmt.Fprintf(out, "At your prices that is about %.0f per month in requests. It only becomes savings once the nodes consolidate.\n", cost)
		}
	}
	if s.UnmatchedPods > 0 {
		fmt.Fprintf(out, "%d pods in the usage files matched no workload (Jobs and CronJobs are not matched).\n", s.UnmatchedPods)
	}
	return nil
}

type jsonWorkload struct {
	ID         string                `json:"id"`
	Kind       string                `json:"kind"`
	Namespace  string                `json:"namespace"`
	Name       string                `json:"name"`
	File       string                `json:"file,omitempty"`
	Replicas   *int32                `json:"replicas,omitempty"`
	Status     rightsize.Status      `json:"status"`
	ReclaimCPU int64                 `json:"reclaimableCPUMillis"`
	ReclaimMem int64                 `json:"reclaimableMemoryBytes"`
	Containers []rightsize.Container `json:"containers"`
	Findings   []rightsize.Finding   `json:"findings"`
	// AutoscaledBy names the HPAs and ScaledObjects that target the workload.
	AutoscaledBy []string `json:"autoscaledBy,omitempty"`
	// ControlledBy names the VPAs that target the workload.
	ControlledBy []string `json:"controlledBy,omitempty"`
}

// JSON writes a machine-readable report that starts with schemaVersion and toolVersion.
func JSON(out io.Writer, as []rightsize.Assessment, o Options) error {
	doc := struct {
		SchemaVersion int              `json:"schemaVersion"`
		ToolVersion   string           `json:"toolVersion"`
		Config        rightsize.Config `json:"config"`
		Summary       Summary          `json:"summary"`
		Workloads     []jsonWorkload   `json:"workloads"`
	}{SchemaVersion: SchemaVersion, ToolVersion: o.Version, Config: o.Config,
		Summary: Summarize(as, o.Unmatched), Workloads: []jsonWorkload{}}
	for _, a := range Order(as) {
		w := jsonWorkload{
			ID: a.Workload.ID(), Kind: a.Workload.Kind, Namespace: a.Workload.Namespace,
			Name: a.Workload.Name, File: a.Workload.File, Status: a.Status,
			ReclaimCPU: a.ReclaimableCPU(), ReclaimMem: a.ReclaimableMemory(),
			Containers: a.Containers, Findings: a.Findings, AutoscaledBy: a.AutoscaledBy, ControlledBy: a.ControlledBy,
		}
		if a.Workload.LongRunning {
			r := a.Workload.Replicas
			w.Replicas = &r
		}
		doc.Workloads = append(doc.Workloads, w)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Markdown writes a report for a pull request comment: a summary and the workloads that need
// attention, the most reclaimable CPU first.
func Markdown(out io.Writer, as []rightsize.Assessment, o Options) error {
	s := Summarize(as, o.Unmatched)
	c := s.Counts
	fmt.Fprintln(out, "<!-- rightsizing-check -->")
	fmt.Fprintln(out, "## Rightsizing check")
	fmt.Fprintf(out, "\n%d workloads: **%d** oversized, **%d** undersized, **%d** ok, **%d** without data.\n",
		s.Total, c[rightsize.Oversized], c[rightsize.Undersized], c[rightsize.OK], c[rightsize.NoData])
	if s.RequestedCPUMillis > 0 {
		fmt.Fprintf(out, "\nReclaimable requests: **%s vCPU** (%d%%) and **%s GiB** (%d%%).\n",
			cores(s.ReclaimCPUMillis), s.ReclaimCPUPercent, gib(s.ReclaimMemory), s.ReclaimMemPercent)
	}
	var attention []rightsize.Assessment
	for _, a := range Order(as) {
		if a.Status == rightsize.Oversized || a.Status == rightsize.Undersized {
			attention = append(attention, a)
		}
	}
	if len(attention) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\n| Status | Workload | Reclaimable CPU | Reclaimable memory | Findings |\n| --- | --- | --- | --- | --- |")
	const limit = 50
	for i, a := range attention {
		if i == limit {
			fmt.Fprintf(out, "\n%d more workloads not shown.\n", len(attention)-limit)
			break
		}
		fmt.Fprintf(out, "| %s | `%s` | %s | %s | %s |\n", a.Status, a.Workload.ID(),
			cpuCell(a.ReclaimableCPU()), memCell(a.ReclaimableMemory()), ruleIDs2(a))
	}
	return nil
}

func ruleIDs2(a rightsize.Assessment) string {
	seen := map[string]bool{}
	out := ""
	for _, f := range a.Findings {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			if out != "" {
				out += ", "
			}
			out += f.Rule
		}
	}
	return out
}
