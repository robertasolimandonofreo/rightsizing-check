package rightsize

import (
	"strings"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

// generatedAlphabet is what Kubernetes uses for the random parts of a name (the ReplicaSet hash
// and the pod suffix): no vowels, so a word like "gateway" cannot be mistaken for one.
const generatedAlphabet = "bcdfghjklmnpqrstvwxz2456789"

func isGenerated(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(generatedAlphabet, r) {
			return false
		}
	}
	return true
}

func isOrdinal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// podOf reports whether a pod name belongs to the workload, by the naming the controllers use:
// Deployment pods are name-<replicaset hash>-<suffix>, StatefulSet pods name-<ordinal>, DaemonSet
// pods name-<suffix>, and a bare Pod keeps its name. Jobs and CronJobs are not matched: their
// pods are short-lived and a sample of one says little.
func podOf(w inventory.Workload, pod string) bool {
	if w.Kind == "Pod" {
		return pod == w.Name
	}
	rest, ok := strings.CutPrefix(pod, w.Name+"-")
	if !ok {
		return false
	}
	switch w.Kind {
	case "StatefulSet":
		return isOrdinal(rest)
	case "Deployment":
		hash, suffix, found := strings.Cut(rest, "-")
		return found && isGenerated(hash, 6, 10) && isGenerated(suffix, 5, 5)
	case "DaemonSet":
		return isGenerated(rest, 5, 5)
	}
	return false
}

// Matches groups samples by workload index and container name. When two workloads claim a pod
// (api and api-v2 sharing a prefix cannot both match the strict patterns, but a StatefulSet and a
// Deployment can), the longest name wins, and a tie leaves the pod unmatched.
type Matches struct {
	ByWorkload map[int]map[string][]usage.Sample
	// Unmatched is the number of distinct pods that no workload claimed.
	Unmatched int
}

// Match assigns every sample to a workload of the same namespace.
func Match(ws []inventory.Workload, samples []usage.Sample) Matches {
	m := Matches{ByWorkload: map[int]map[string][]usage.Sample{}}
	unmatched := map[[2]string]bool{}
	for _, s := range samples {
		best, tie := -1, false
		for i, w := range ws {
			if w.Namespace != s.Namespace || !podOf(w, s.Pod) {
				continue
			}
			switch {
			case best < 0 || len(w.Name) > len(ws[best].Name):
				best, tie = i, false
			case len(w.Name) == len(ws[best].Name):
				tie = true
			}
		}
		if best < 0 || tie {
			unmatched[[2]string{s.Namespace, s.Pod}] = true
			continue
		}
		if m.ByWorkload[best] == nil {
			m.ByWorkload[best] = map[string][]usage.Sample{}
		}
		m.ByWorkload[best][s.Container] = append(m.ByWorkload[best][s.Container], s)
	}
	m.Unmatched = len(unmatched)
	return m
}
