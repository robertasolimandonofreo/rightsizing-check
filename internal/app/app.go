// Package app wires the command line to the loader, the usage readers, the rules and the reports.
package app

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/inventory"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/report"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/rightsize"
	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
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
}

const usageText = `rightsizing-check finds Kubernetes CPU and memory requests that are far above what the containers use.
It reads manifests and usage files. It never connects to a cluster.

Usage:
  rightsizing-check scan [flags] [paths...]   compare the manifests with the usage files
  rightsizing-check explain <rule>            describe a rule, for example RC001
  rightsizing-check version

Usage files (at least one):
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
		verbose                                                   bool
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
	if len(paths) == 0 {
		return fail("scan needs at least one path (a file, a directory, or - for stdin)")
	}
	if promCPU == "" && promMem == "" && top == "" && csvFile == "" {
		return fail("scan needs usage data: pass --prom-cpu, --prom-mem, --top or --csv (see the README)")
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

	inv, err := inventory.Paths(paths, env.Stdin)
	if err != nil {
		return fail("error: %v", err)
	}
	if len(inv.Workloads) == 0 {
		// Not an error, but a typo in the path would otherwise pass a CI check silently.
		fmt.Fprintf(env.Stderr, "warning: no workloads found in %s (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs and Pods are read)\n", strings.Join(paths, ", "))
	}
	var data usage.Dataset
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

	matches := rightsize.Match(inv.Workloads, data.Samples)
	assessments := make([]rightsize.Assessment, 0, len(inv.Workloads))
	for i, w := range inv.Workloads {
		assessments = append(assessments, rightsize.Evaluate(w, matches.ByWorkload[i], cfg))
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
