package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const cpuBody = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"0.12"]}]}}`
const memBody = `{"status":"success","data":{"resultType":"vector","result":[
 {"metric":{"namespace":"shop","pod":"api-5d8f7c9b6-x2k4j","container":"app"},"value":[1,"629145600"]}]}}`

// server records the queries it receives and answers like the Prometheus API.
type server struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
	auth    string
	path    string
}

func newServer(t *testing.T, status int, cpu, mem string) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.auth, s.path = r.Header.Get("Authorization"), r.URL.Path
		s.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, only GET is allowed", r.Method)
		}
		w.WriteHeader(status)
		if strings.Contains(q, "container_cpu_usage_seconds_total") {
			_, _ = w.Write([]byte(cpu))
			return
		}
		_, _ = w.Write([]byte(mem))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestFetchBuildsTheQueriesWithTheWindowAndSelector(t *testing.T) {
	s := newServer(t, 200, cpuBody, memBody)
	cpu, mem, err := Fetch(context.Background(), Options{BaseURL: s.URL, Window: "14d", AllowLongWindow: true, Selector: `namespace=~"shop|nps"`})
	if err != nil {
		t.Fatal(err)
	}
	if len(cpu) != 1 || cpu[0].CPUMillis != 120 || len(mem) != 1 || mem[0].MemBytes != 629145600 {
		t.Errorf("cpu = %+v, mem = %+v", cpu, mem)
	}
	if len(s.queries) != 2 {
		t.Fatalf("queries = %v", s.queries)
	}
	for _, q := range s.queries {
		if !strings.Contains(q, `[14d`) && !strings.Contains(q, `[14d]`) {
			t.Errorf("the window is missing from %q", q)
		}
		if !strings.Contains(q, `namespace=~"shop|nps"`) || !strings.Contains(q, `container!="POD"`) {
			t.Errorf("selector or default matchers missing from %q", q)
		}
	}
	var sawCPU bool
	for _, q := range s.queries {
		if strings.Contains(q, "quantile_over_time(0.95") && strings.Contains(q, "[14d:5m]") {
			sawCPU = true
		}
	}
	if !sawCPU {
		t.Errorf("the CPU query is a p95 subquery over the window: %v", s.queries)
	}
}

func TestEndpointNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"https://prom.example.com":                 "https://prom.example.com/api/v1/query",
		"https://prom.example.com/":                "https://prom.example.com/api/v1/query",
		"https://prom.example.com/query":           "https://prom.example.com/api/v1/query",
		"https://prom.example.com/graph":           "https://prom.example.com/api/v1/query",
		"https://prom.example.com/api/v1/query":    "https://prom.example.com/api/v1/query",
		"https://x.io/prometheus/query?g0.expr=up": "https://x.io/prometheus/api/v1/query",
		"http://localhost:9090":                    "http://localhost:9090/api/v1/query",
	} {
		u, err := endpoint(in)
		if err != nil || u.String() != want {
			t.Errorf("endpoint(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "prom.example.com", "ftp://x", "https://"} {
		if _, err := endpoint(bad); err == nil {
			t.Errorf("endpoint(%q) should be rejected", bad)
		}
	}
	if h := Host("https://user:secret@prom.example.com/query?x=1"); h != "prom.example.com" {
		t.Errorf("Host must not leak credentials: %q", h)
	}
}

func TestValidateWindowAndSelector(t *testing.T) {
	for _, ok := range []string{"7d", "168h", "2w", "30m", "1y"} {
		if err := ValidateWindow(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "7", "d", "7 days", "7d]", "7d) or vector(1", "-7d", "1.5d"} {
		if err := ValidateWindow(bad); err == nil {
			t.Errorf("%q must be rejected: it would go into the query", bad)
		}
	}
	if _, err := CPUQuery("7d", `namespace="a"}) or vector(1) #{`); err == nil {
		t.Error("braces in the selector must be rejected")
	}
}

func TestTokenIsSentAsBearer(t *testing.T) {
	s := newServer(t, 200, cpuBody, memBody)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: s.URL, Window: "7d", Token: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer abc123" {
		t.Errorf("Authorization = %q", s.auth)
	}
	s2 := newServer(t, 200, cpuBody, memBody)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: s2.URL, Window: "7d"}); err != nil || s2.auth != "" {
		t.Errorf("no token must send no credentials: %q, %v", s2.auth, err)
	}
	if s.path != "/api/v1/query" {
		t.Errorf("path = %q", s.path)
	}
}

func TestFetchErrors(t *testing.T) {
	denied := newServer(t, 401, "unauthorized", "unauthorized")
	if _, _, err := Fetch(context.Background(), Options{BaseURL: denied.URL, Window: "7d"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a 401 must be reported with its status: %v", err)
	}
	failed := newServer(t, 200, `{"status":"error","error":"query timed out"}`, memBody)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: failed.URL, Window: "7d"}); err == nil || !strings.Contains(err.Error(), "CPU query") {
		t.Errorf("a failed CPU query must say which one: %v", err)
	}
	empty := newServer(t, 200, `{"status":"success","data":{"resultType":"vector","result":[]}}`, memBody)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: empty.URL, Window: "7d"}); err == nil {
		t.Error("no series at all is an error: the window or the selector is wrong")
	}
	if _, _, err := Fetch(context.Background(), Options{BaseURL: "https://x.example.com", Window: "seven"}); err == nil {
		t.Error("a bad window is rejected before any request")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	t.Cleanup(slow.Close)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: slow.URL, Window: "7d", Timeout: 50 * time.Millisecond}); err == nil {
		t.Error("a slow server must hit the timeout")
	}
}

func TestStatusErrorsSayWhatToDo(t *testing.T) {
	// Like a real proxy: the cheap memory query answers, only the CPU subquery is cut off.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("query"), "container_cpu_usage_seconds_total") {
			w.WriteHeader(504)
			_, _ = w.Write([]byte("<html><head><title>504</title></head><body></body></html>"))
			return
		}
		_, _ = w.Write([]byte(memBody))
	}))
	t.Cleanup(gateway.Close)
	_, _, err := Fetch(context.Background(), Options{BaseURL: gateway.URL, Window: "14d", AllowLongWindow: true})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"CPU query", "504", "shorter", "--prom-selector"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "<html>") {
		t.Errorf("the HTML page from the proxy must not be printed: %v", err)
	}
	auth := newServer(t, 403, "forbidden", memBody)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: auth.URL, Window: "7d"}); err == nil || !strings.Contains(err.Error(), "--prom-token-env") {
		t.Errorf("a 403 should point at the token option: %v", err)
	}
}

func TestLongWindowNeedsConfirmation(t *testing.T) {
	s := newServer(t, 200, cpuBody, memBody)
	for _, w := range []string{"8d", "14d", "2w", "169h", "1y", "99999999999d"} {
		_, _, err := Fetch(context.Background(), Options{BaseURL: s.URL, Window: w})
		if err == nil || !strings.Contains(err.Error(), "--prom-allow-long-window") {
			t.Errorf("%s must be refused without the flag: %v", w, err)
		}
	}
	if len(s.queries) != 0 {
		t.Errorf("a refused window must not send any query, sent %v", s.queries)
	}
	for _, w := range []string{"1d", "7d", "168h", "1w", "30m"} {
		if _, _, err := Fetch(context.Background(), Options{BaseURL: s.URL, Window: w}); err != nil {
			t.Errorf("%s is within 7d: %v", w, err)
		}
	}
	if _, _, err := Fetch(context.Background(), Options{BaseURL: s.URL, Window: "30d", AllowLongWindow: true}); err != nil {
		t.Errorf("an explicit confirmation allows it: %v", err)
	}
}

func TestQueriesRunOneAtATimeMemoryFirst(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "memory") {
			order = append(order, "memory")
		} else {
			order = append(order, "cpu")
		}
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		if strings.Contains(q, "container_cpu_usage_seconds_total") {
			_, _ = w.Write([]byte(cpuBody))
			return
		}
		_, _ = w.Write([]byte(memBody))
	}))
	t.Cleanup(srv.Close)
	if _, _, err := Fetch(context.Background(), Options{BaseURL: srv.URL, Window: "7d"}); err != nil {
		t.Fatal(err)
	}
	if peak != 1 {
		t.Errorf("at most one query at a time, saw %d at once", peak)
	}
	if strings.Join(order, ",") != "memory,cpu" {
		t.Errorf("the lighter memory query goes first, got %v", order)
	}
}

func TestWindowDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "168h": 168 * time.Hour, "2w": 14 * 24 * time.Hour, "30m": 30 * time.Minute, "1y": 365 * 24 * time.Hour, "500ms": 500 * time.Millisecond} {
		if got := windowDuration(in); got != want {
			t.Errorf("windowDuration(%q) = %v, want %v", in, got, want)
		}
	}
}
