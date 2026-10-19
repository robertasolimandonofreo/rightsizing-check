// Package app wires the command line to the loader, the usage readers, the rules and the reports.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/cluster"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/prom"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/report"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/rightsize"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
	"k8s.io/client-go/dynamic"
)

// Exit codes: 0 success, 1 the --fail-on threshold was reached, 2 a usage or runtime error.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// Env carries what the command needs from its surroundings, so tests can replace it.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
	// Cluster replaces the client of the live-cluster mode. Tests use it.
	Cluster dynamic.Interface
	// HTTPClient replaces the one used for Prometheus. Tests use it; nil means the default.
	HTTPClient *http.Client
}

const usageText = `rightsizing-check finds Kubernetes CPU and memory requests that are far above what the containers use.
It reads manifests and usage files. It never connects to a cluster.

Usage:
  rightsizing-check scan [flags] [paths...]   compare the manifests with the usage files
  rightsizing-check scan --cluster [flags]    read the workloads and the usage from a live cluster
  rightsizing-check explain <rule>            describe a rule, for example RC001
  rightsizing-check version

Live cluster (read-only: it only lists, with paging and a low request rate):
      --cluster                read the workloads, HPAs, KEDA ScaledObjects, VPAs and Argo Rollouts
                               from the cluster, and the usage from metrics-server (an instant, like
                               kubectl top) unless another usage source is given
      --kubeconfig <file>      kubeconfig to use (default: the usual one)
      --context <name>         kubeconfig context to use (default: the current one)
  -n, --namespace <name>       with --cluster, read only this namespace; otherwise it is the namespace
                               of kubectl top output without -A (default "default")

Usage data (at least one source):
      --prometheus <url>       read it from Prometheus: GET /api/v1/query, nothing else
      --window <duration>      the window to read, for example 7d, 14d or 168h (default 7d)
      --prom-selector <m>      extra label matchers, for example namespace=~"shop|nps"
      --prom-token-env <VAR>   environment variable that holds a bearer token
      --prom-timeout <dur>     limit for each query (default 3m)
      --prom-allow-long-window allow a --window over 7d (heavy: it can overload a small Prometheus)
      --prom-cpu <file>        Prometheus instant-query JSON, CPU in cores
      --prom-mem <file>        Prometheus instant-query JSON, memory in bytes
      --top <file>             output of: kubectl top pods -A --containers
      --csv <file>             CSV: namespace,pod,container,cpu_millis,memory_bytes
  -n, --namespace <name>       namespace of kubectl top output without -A (default "default")

Scan flags:
  -f, --format <table|json|markdown>   output format (default table)
      --fail-on <undersized|oversized> exit 1 when a workload reaches this level
      --slack <n>                      a request is inflated above n times the recommendation (default 1.5)
      --cpu-headroom <share>           headroom over the observed CPU (default 0.15)
      --mem-headroom <share>           headroom over the observed memory (default 0.20)
      --cpu-price <n>                  price of a vCPU per month, to show a cost line
      --mem-price <n>                  price of a GiB per month, to show a cost line
  -v, --verbose                        print every finding
`

// Run executes one command line and returns the process exit code.
func Run(args []string, env Env) int {
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "scan":
		return scan(args[1:], env)
	case "explain":
		return explain(args[1:], env)
	case "version", "--version":
		fmt.Fprintln(env.Stdout, "rightsizing-check", env.Version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usageText)
		return exitOK
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", args[0], usageText)
	return exitUsage
}

// parseInterspersed parses flags that may come before or after the paths, which the standard
// flag package does not do by itself: it stops at the first non-flag argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
}

func scan(args []string, env Env) int {
	var (
		format, failOn, promCPU, promMem, top, csvFile, namespace string
		promURL, window, selector, tokenEnv, promTimeout          string
		kubeconfig, kubeContext                                   string
		useCluster                                                bool
		verbose, allowLong                                        bool
		cfg                                                       = rightsize.DefaultConfig()
		cpuPrice, memPrice                                        float64
	)
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.StringVar(&format, "format", "table", "")
	fs.StringVar(&format, "f", "table", "")
	fs.StringVar(&failOn, "fail-on", "", "")
	fs.StringVar(&promCPU, "prom-cpu", "", "")
	fs.StringVar(&promMem, "prom-mem", "", "")
	fs.StringVar(&top, "top", "", "")
	fs.StringVar(&csvFile, "csv", "", "")
	fs.StringVar(&namespace, "namespace", "default", "")
	fs.StringVar(&namespace, "n", "default", "")
	fs.BoolVar(&useCluster, "cluster", false, "")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "")
	fs.StringVar(&kubeContext, "context", "", "")
	fs.StringVar(&promURL, "prometheus", "", "")
	fs.StringVar(&window, "window", "7d", "")
	fs.StringVar(&selector, "prom-selector", "", "")
	fs.StringVar(&tokenEnv, "prom-token-env", "", "")
	fs.StringVar(&promTimeout, "prom-timeout", "3m", "")
	fs.BoolVar(&allowLong, "prom-allow-long-window", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	fs.BoolVar(&verbose, "v", false, "")
	fs.Float64Var(&cfg.Slack, "slack", cfg.Slack, "")
	fs.Float64Var(&cfg.CPUHeadroom, "cpu-headroom", cfg.CPUHeadroom, "")
	fs.Float64Var(&cfg.MemHeadroom, "mem-headroom", cfg.MemHeadroom, "")
	fs.Float64Var(&cpuPrice, "cpu-price", 0, "")
	fs.Float64Var(&memPrice, "mem-price", 0, "")
	paths, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(env.Stderr, format+"\n", a...)
		return exitUsage
	}
	if len(paths) == 0 && !useCluster {
		return fail("scan needs at least one path (a file, a directory, or - for stdin), or --cluster")
	}
	usageGiven := promURL != "" || promCPU != "" || promMem != "" || top != "" || csvFile != ""
	if !usageGiven && !useCluster {
		return fail("scan needs usage data: pass --prometheus, --prom-cpu, --prom-mem, --top or --csv (see the README)")
	}
	switch format {
	case "table", "json", "markdown", "md":
	default:
		return fail("unknown format %q (use table, json or markdown)", format)
	}
	switch failOn {
	case "", "undersized", "oversized":
	default:
		return fail("unknown --fail-on %q (use undersized or oversized)", failOn)
	}
	if cfg.Slack < 1 || cfg.CPUHeadroom < 0 || cfg.MemHeadroom < 0 || cpuPrice < 0 || memPrice < 0 {
		return fail("--slack must be at least 1, and headrooms and prices cannot be negative")
	}

	var clusterSamples []usage.Sample
	inv, err := inventory.Paths(paths, env.Stdin)
	if err != nil {
		return fail("error: %v", err)
	}
	if n := len(inv.Unreadable); n > 0 {
		shown := inv.Unreadable
		if len(shown) > 3 {
			shown = shown[:3]
		}
		fmt.Fprintf(env.Stderr, "warning: skipped %d files that are not valid Kubernetes YAML (Helm templates, for example); first: ", n)
		for i, u := range shown {
			if i > 0 {
				fmt.Fprint(env.Stderr, ", ")
			}
			fmt.Fprint(env.Stderr, u.Path)
		}
		fmt.Fprintln(env.Stderr)
	}
	if useCluster {
		fmt.Fprintln(env.Stderr, "reading the cluster (read-only: list calls only)...")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		res, err := cluster.Read(ctx, inv, cluster.Options{Kubeconfig: kubeconfig, Context: kubeContext, Namespace: clusterNamespace(namespace, fs),
			SkipMetrics: usageGiven, Client: env.Cluster, Notes: env.Stderr})
		stop()
		if err != nil {
			return fail("error: %v", err)
		}
		fmt.Fprintf(env.Stderr, "read cluster %s: %d Deployments, %d StatefulSets, %d DaemonSets, %d Rollouts, %d HPAs, %d ScaledObjects, %d VPAs, usage of %d containers\n",
			res.Context, res.Counts["deployments"], res.Counts["statefulsets"], res.Counts["daemonsets"], res.Counts["rollouts"],
			res.Counts["horizontalpodautoscalers"], res.Counts["scaledobjects"], res.Counts["verticalpodautoscalers"], len(res.Samples))
		clusterSamples = res.Samples
	}
	inv.Sort()
	if len(inv.Workloads) == 0 {
		// Not an error, but a typo in the path would otherwise pass a CI check silently.
		where := "the cluster"
		if len(paths) > 0 {
			where = strings.Join(paths, ", ")
		}
		fmt.Fprintf(env.Stderr, "warning: no workloads found in %s (Deployments, StatefulSets, DaemonSets, Rollouts, Jobs, CronJobs and Pods are read)\n", where)
	}

	var data usage.Dataset
	data.Merge(clusterSamples)
	load := func(path string, parse func(io.Reader) ([]usage.Sample, error)) error {
		if path == "" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		samples, err := parse(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		data.Merge(samples)
		return nil
	}
	for _, step := range []struct {
		path  string
		parse func(io.Reader) ([]usage.Sample, error)
	}{
		{promCPU, func(r io.Reader) ([]usage.Sample, error) { return usage.Prometheus(r, usage.CPU) }},
		{promMem, func(r io.Reader) ([]usage.Sample, error) { return usage.Prometheus(r, usage.Memory) }},
		{top, func(r io.Reader) ([]usage.Sample, error) { return usage.Top(r, namespace) }},
		{csvFile, usage.CSV},
	} {
		if err := load(step.path, step.parse); err != nil {
			return fail("error: %v", err)
		}
	}

	if promURL != "" {
		timeout, err := time.ParseDuration(promTimeout)
		if err != nil || timeout <= 0 {
			return fail("invalid --prom-timeout %q (use a duration such as 3m)", promTimeout)
		}
		token := ""
		if tokenEnv != "" {
			if token = os.Getenv(tokenEnv); token == "" {
				return fail("--prom-token-env %s is empty or not set", tokenEnv)
			}
		}
		fmt.Fprintf(env.Stderr, "reading usage from %s over %s (read-only)...\n", prom.Host(promURL), window)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		started := time.Now()
		cpu, mem, err := prom.Fetch(ctx, prom.Options{BaseURL: promURL, Window: window, Selector: selector, Token: token, Timeout: timeout, AllowLongWindow: allowLong, Client: env.HTTPClient})
		stop()
		if err != nil {
			return fail("error: %v", err)
		}
		fmt.Fprintf(env.Stderr, "read %d CPU and %d memory series in %s\n", len(cpu), len(mem), time.Since(started).Round(100*time.Millisecond))
		data.Merge(cpu)
		data.Merge(mem)
	}

	matches := rightsize.Match(inv.Workloads, data.Samples)
	assessments := make([]rightsize.Assessment, 0, len(inv.Workloads))
	for i, w := range inv.Workloads {
		a := rightsize.Evaluate(w, matches.ByWorkload[i], cfg, rightsize.ScalersFor(w, inv.Autoscalers))
		rightsize.AddVPAFindings(&a, rightsize.VPAsFor(w, inv.VPAs))
		assessments = append(assessments, a)
	}

	ro := report.Options{Verbose: verbose, Version: env.Version, Unmatched: matches.Unmatched,
		CPUPrice: cpuPrice, MemPrice: memPrice, Config: cfg}
	var werr error
	switch format {
	case "json":
		werr = report.JSON(env.Stdout, assessments, ro)
	case "markdown", "md":
		werr = report.Markdown(env.Stdout, assessments, ro)
	default:
		werr = report.Table(env.Stdout, assessments, ro)
	}
	if werr != nil {
		return fail("error: %v", werr)
	}
	if reached(assessments, failOn) {
		return exitFail
	}
	return exitOK
}

// reached tells whether any workload is at or beyond the --fail-on level. oversized also fails
// on undersized.
func reached(as []rightsize.Assessment, level string) bool {
	for _, a := range as {
		switch a.Status {
		case rightsize.Undersized:
			if level != "" {
				return true
			}
		case rightsize.Oversized:
			if level == "oversized" {
				return true
			}
		}
	}
	return false
}

func explain(args []string, env Env) int {
	if len(args) != 1 {
		fmt.Fprintln(env.Stderr, "usage: rightsizing-check explain <rule>, for example RC001")
		return exitUsage
	}
	id := strings.ToUpper(args[0])
	r, ok := rightsize.Lookup(id)
	if !ok {
		var ids []string
		for _, r := range rightsize.Catalog {
			ids = append(ids, r.ID)
		}
		fmt.Fprintf(env.Stderr, "unknown rule %q (rules: %s)\n", args[0], strings.Join(ids, ", "))
		return exitUsage
	}
	fmt.Fprintf(env.Stdout, "%s [%s] %s\n\nWhy it matters\n  %s\n\nHow to fix it\n  %s\n", r.ID, r.Kind, r.Title, r.Why, r.Fix)
	if r.Example != "" {
		lines := strings.Split(r.Example, "\n")
		for i, l := range lines {
			lines[i] = "  " + l
		}
		fmt.Fprintf(env.Stdout, "\nExample\n%s\n", strings.Join(lines, "\n"))
	}
	return exitOK
}

// clusterNamespace is the namespace to read in cluster mode: -n only counts when it was given, so
// its default for kubectl top output ("default") does not limit the read.
func clusterNamespace(namespace string, fs *flag.FlagSet) string {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "n" || f.Name == "namespace" {
			given = true
		}
	})
	if given {
		return namespace
	}
	return ""
}
