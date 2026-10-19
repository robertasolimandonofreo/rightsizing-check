// Package prom reads CPU and memory usage straight from a Prometheus, with the same two queries
// that the README gives for exporting the data by hand. It only makes GET requests to
// /api/v1/query.
package prom

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/usage"
)

// windowRE is a Prometheus duration of one unit: 7d, 168h, 2w.
var windowRE = regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)

// Options says where to read and over what window.
type Options struct {
	// BaseURL is the Prometheus address. A path that the web UI uses (/query, /graph) or a
	// trailing slash is accepted and removed.
	BaseURL string
	// Window is a Prometheus duration, for example 7d. It must cover your busiest period.
	Window string
	// Selector adds label matchers to both queries, for example namespace=~"shop|nps". Empty
	// reads every container.
	Selector string
	// Token is sent as a bearer token. Empty sends no credentials.
	Token string
	// Timeout bounds each query. The CPU query is a subquery and can be slow on a large server.
	Timeout time.Duration
	// AllowLongWindow lets a window over MaxSafeWindow run. The CPU query is a subquery over the
	// whole window: on a small Prometheus a long one can use enough memory to get its pod evicted.
	AllowLongWindow bool
	// Client is used by tests. Nil means a client with Timeout.
	Client *http.Client
}

// MaxSafeWindow is the longest window that runs without AllowLongWindow.
const MaxSafeWindow = 7 * 24 * time.Hour

// windowDuration converts a validated window to a time.Duration. Prometheus counts a year as 365
// days and a week as 7.
func windowDuration(w string) time.Duration {
	i := len(w)
	for i > 0 && (w[i-1] < '0' || w[i-1] > '9') {
		i--
	}
	var n int64
	for _, c := range w[:i] {
		n = n*10 + int64(c-'0')
		if n > 1<<40 {
			return 1<<62 - 1
		}
	}
	unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour}[w[i:]]
	return time.Duration(n) * unit
}

// ValidateWindow rejects anything that is not a Prometheus duration, so it can be put in a query.
func ValidateWindow(w string) error {
	if !windowRE.MatchString(w) {
		return fmt.Errorf("invalid --window %q: use a Prometheus duration such as 7d, 168h or 2w", w)
	}
	return nil
}

func matchers(selector string) (string, error) {
	if strings.ContainsAny(selector, "{}") {
		return "", fmt.Errorf("invalid --prom-selector %q: give the matchers without braces, for example namespace=\"shop\"", selector)
	}
	m := `container!="",container!="POD"`
	if s := strings.TrimSpace(selector); s != "" {
		m += "," + s
	}
	return m, nil
}

// CPUQuery is the 95th percentile of the per-container CPU rate over the window, in cores.
func CPUQuery(window, selector string) (string, error) {
	m, err := matchers(selector)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`max by (namespace, pod, container) (quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{%s}[5m])[%s:5m]))`, m, window), nil
}

// MemoryQuery is the maximum working set of each container over the window, in bytes.
func MemoryQuery(window, selector string) (string, error) {
	m, err := matchers(selector)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`max by (namespace, pod, container) (max_over_time(container_memory_working_set_bytes{%s}[%s]))`, m, window), nil
}

// endpoint turns what a person pastes (the UI address, with or without /query) into the API URL.
func endpoint(base string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid --prometheus %q: use an address such as https://prometheus.example.com", base)
	}
	path := strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{"/api/v1/query", "/query", "/graph"} {
		path = strings.TrimSuffix(path, suffix)
	}
	u.Path = strings.TrimRight(path, "/") + "/api/v1/query"
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// Host is what is safe to print about the address: no credentials, no query.
func Host(base string) string {
	u, err := endpoint(base)
	if err != nil {
		return "?"
	}
	return u.Host
}

// Fetch runs the CPU and memory queries at the same time and returns the samples of both.
func Fetch(ctx context.Context, o Options) (cpu, mem []usage.Sample, err error) {
	if err := ValidateWindow(o.Window); err != nil {
		return nil, nil, err
	}
	if d := windowDuration(o.Window); d > MaxSafeWindow && !o.AllowLongWindow {
		return nil, nil, fmt.Errorf("--window %s is longer than 7d: the CPU query is a subquery over the whole window, and on a small Prometheus it can use enough memory to get the pod evicted (it happened while this tool was tested against a real server). Use 7d or less, or pass --prom-allow-long-window if you know your server can take it", o.Window)
	}
	u, err := endpoint(o.BaseURL)
	if err != nil {
		return nil, nil, err
	}
	cpuQ, err := CPUQuery(o.Window, o.Selector)
	if err != nil {
		return nil, nil, err
	}
	memQ, _ := MemoryQuery(o.Window, o.Selector)
	client := o.Client
	if client == nil {
		timeout := o.Timeout
		if timeout == 0 {
			timeout = 3 * time.Minute
		}
		client = &http.Client{Timeout: timeout}
	}

	// One query at a time, the lighter one first: running both together doubles the load that a
	// read of the usage puts on a server that is probably busy monitoring something else.
	mem, err = query(ctx, client, u, o.Token, memQ, usage.Memory)
	if err != nil {
		return nil, nil, fmt.Errorf("memory query: %w", err)
	}
	cpu, err = query(ctx, client, u, o.Token, cpuQ, usage.CPU)
	if err != nil {
		return nil, nil, fmt.Errorf("CPU query: %w", err)
	}
	return cpu, mem, nil
}

func query(ctx context.Context, client *http.Client, base *url.URL, token, q string, m usage.Metric) ([]usage.Sample, error) {
	u := *base
	u.RawQuery = url.Values{"query": {q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	return usage.Prometheus(resp.Body, m)
}

// statusError explains a failed request. A gateway or timeout status usually means the query was
// too heavy for the server or for a proxy in front of it, and the window is what makes it heavy.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	text := strings.TrimSpace(string(body))
	if strings.Contains(text, "<") {
		text = "" // an HTML error page from a proxy says nothing more than the status
	}
	msg := resp.Status
	if text != "" {
		msg += ": " + text
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		msg += " (the query was cut off: a long --window makes the CPU subquery heavy, so try a shorter one, or read a part of the cluster at a time with --prom-selector 'namespace=\"shop\"')"
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (the server wants credentials: pass a bearer token with --prom-token-env)"
	}
	return fmt.Errorf("%s", msg)
}
