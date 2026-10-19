package rightsize

// Kind groups what a finding is about.
type Kind string

const (
	// Waste is a request above what the container uses: capacity the scheduler reserves for nothing.
	Waste Kind = "waste"
	// Risk is a request or limit below what the container uses.
	Risk Kind = "risk"
	// Caveat is something to check before acting on a recommendation, without being a problem of
	// the workload itself.
	Caveat Kind = "caveat"
	// Data is about the input: a missing request, or no usage found.
	Data Kind = "data"
)

// Rule documents one check. The catalog is the single source for the reports and for explain.
type Rule struct {
	ID      string
	Kind    Kind
	Title   string
	Why     string
	Fix     string
	Example string
}

// Catalog lists every rule, in ID order.
var Catalog = []Rule{
	{
		ID: "RC001", Kind: Waste,
		Title: "CPU request is far above the observed usage",
		Why:   "The scheduler reserves the requested CPU on a node whether the container uses it or not. A request well above usage keeps nodes half empty, and Karpenter cannot consolidate what the pods say they need.",
		Fix:   "Lower the CPU request to the recommended value (observed usage plus headroom), one workload at a time, and watch throttling and latency.",
		Example: `resources:
  requests:
    cpu: 1000m   # observed p95 is 120m
# fix
    cpu: 140m`,
	},
	{
		ID: "RC002", Kind: Waste,
		Title: "Memory request is far above the observed usage",
		Why:   "Memory is reserved like CPU, and it is the resource that usually decides how many pods fit on a node.",
		Fix:   "Lower the memory request to the recommended value. Memory is not compressible: keep the headroom, and prefer the maximum over the window, not the average.",
		Example: `resources:
  requests:
    memory: 4Gi    # observed maximum is 600Mi
# fix
    memory: 720Mi`,
	},
	{
		ID: "RC003", Kind: Risk,
		Title: "CPU usage is above the request",
		Why:   "The pod uses more CPU than it reserved, so the scheduler packs nodes tighter than the real load and neighbours compete for CPU when the node is busy.",
		Fix:   "Raise the CPU request to the recommended value.",
		Example: `resources:
  requests:
    cpu: 100m    # observed p95 is 300m
# fix
    cpu: 350m`,
	},
	{
		ID: "RC004", Kind: Risk,
		Title: "Memory usage is above the request",
		Why:   "The pod uses more memory than it reserved. Under node memory pressure the kubelet evicts the pods that use the most memory above their request first.",
		Fix:   "Raise the memory request to the recommended value.",
		Example: `resources:
  requests:
    memory: 256Mi  # observed maximum is 700Mi
# fix
    memory: 840Mi`,
	},
	{
		ID: "RC005", Kind: Risk,
		Title: "Memory usage is close to the limit",
		Why:   "A container that reaches its memory limit is killed (OOMKilled). Usage within 10% of the limit means it is one spike away.",
		Fix:   "Raise the memory limit, or find what grows. Do not lower the request of this container before that.",
		Example: `resources:
  limits:
    memory: 512Mi  # observed maximum is 490Mi`,
	},
	{
		ID: "RC006", Kind: Data,
		Title: "No request is set",
		Why:   "Without a request the scheduler assumes zero, so the container can land on a full node and the tool cannot say how much is reclaimable.",
		Fix:   "Set a request from the recommended value.",
		Example: `resources:
  requests:
    cpu: 140m
    memory: 720Mi`,
	},
	{
		ID: "RC007", Kind: Data,
		Title:   "No usage found for the container",
		Why:     "The usage files have no sample that maps to this container, so nothing can be said about it. Pods are matched to workloads by name, in the same namespace.",
		Fix:     "Check that the query covers this namespace and that the container ran in the window. Pods of Jobs and CronJobs are not matched.",
		Example: `# the usage file needs the labels namespace, pod and container`,
	},
	{
		ID: "RC008", Kind: Data,
		Title:   "Usage is a snapshot, not a window",
		Why:     "kubectl top shows the usage at one instant. A workload that is idle now and busy at night looks oversized, and shrinking it from a snapshot can cause throttling or OOMKills.",
		Fix:     "Use the Prometheus queries in the README (a 7-day p95 for CPU and a maximum for memory) before changing production.",
		Example: `kubectl top pods -A --containers   # one instant`,
	},
	{
		ID: "RC009", Kind: Caveat,
		Title: "An HPA or KEDA scaler uses the request as its denominator",
		Why:   "A HorizontalPodAutoscaler (or a KEDA cpu or memory trigger) that scales on utilization compares the usage with the request. Lowering the request raises the utilization it sees, so a workload that is oversized today can start to scale out the moment the request comes down.",
		Fix:   "Either keep the request, or lower it together with the scaler's target so the same usage lands where it does today. Compare the utilization the tool prints with the target before changing anything.",
		Example: `# usage 38m, request 500m (8% utilization), target 75%
# recommended request 50m -> 76% utilization: above the target, the HPA scales out
triggers:
  - type: cpu
    metricType: Utilization
    metadata: {value: "75"}`,
	},
	{
		ID: "RC010", Kind: Caveat,
		Title: "A VPA sets the requests of this workload",
		Why:   "A VerticalPodAutoscaler that is not in Off mode writes the requests of the pods it controls. A request changed by hand in the manifest does not last: the VPA overwrites it the next time the pods are created. The recommendation here is only useful as a comparison with what the VPA does.",
		Fix:   "Change the request through the VPA instead: set its minAllowed and maxAllowed, or put it in Off mode (recommendations only) and keep the requests in the manifest. A container can be taken out of the VPA's control with a resource policy with mode Off.",
		Example: `apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
spec:
  updatePolicy:
    updateMode: Auto        # Off keeps the requests in the manifest
  resourcePolicy:
    containerPolicies:
      - containerName: app
        minAllowed: {cpu: 50m, memory: 128Mi}`,
	},
	{
		ID: "RC011", Kind: Data,
		Title:   "The VPA has its own recommendation",
		Why:     "When the VPA object carries a status (read from a cluster, or a manifest exported from one), its target is shown next to this tool's recommendation. Two different numbers are a reason to look at the window each one uses: the VPA learns from the history that it has, this tool from the window you gave it.",
		Fix:     "Compare the two before changing a request. A VPA in Off mode is a good source of a second opinion.",
		Example: `kubectl get vpa -A -o yaml > vpa.yaml   # then scan it together with the manifests`,
	},
}

// Lookup finds a rule by ID.
func Lookup(id string) (Rule, bool) {
	for _, r := range Catalog {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}
