package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSnapshot(version string, routes ...Route) Snapshot {
	return Snapshot{Version: version, Routes: routes}
}

func testRoute(id, path, mode, target string) Route {
	return Route{ID: id, Path: path, MatchMode: mode, Methods: []string{"GET", "POST"}, TimeoutMs: 1000, Enabled: true, ServiceID: id, Targets: []Target{{URL: target, Weight: 1}}}
}

func TestSnapshotApplyIsAtomicAndLastKnownGoodSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway", "config.json")
	manager, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	first := testSnapshot("1", testRoute("users", "/api/users", "PREFIX", "http://127.0.0.1:4001"))
	if err := manager.Apply(first); err != nil {
		t.Fatal(err)
	}
	invalid := testSnapshot("2", testRoute("broken", "/broken", "PREFIX", "file:///etc/passwd"))
	if err := manager.Apply(invalid); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if got := manager.Current().Version; got != "1" {
		t.Fatalf("version after rejection=%q", got)
	}
	reloaded, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Current().Version; got != "1" {
		t.Fatalf("reloaded version=%q", got)
	}
	if err := reloaded.Apply(first); err != nil {
		t.Fatalf("idempotent apply failed: %v", err)
	}
	mutated := first
	mutated.Routes[0].Path = "/changed"
	if err := reloaded.Apply(mutated); err == nil {
		t.Fatal("version reused with changed content accepted")
	}
	if err := reloaded.Apply(testSnapshot("0", first.Routes...)); err == nil {
		t.Fatal("older version accepted")
	}
	second := testSnapshot("2", testRoute("users", "/api/users", "PREFIX", "http://127.0.0.1:4001"))
	if err := reloaded.Apply(second); err != nil {
		t.Fatalf("persist replacement snapshot: %v", err)
	}
	reloadedAgain, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloadedAgain.Current().Version; got != "2" {
		t.Fatalf("reloaded replacement version=%q", got)
	}
}

func TestConcurrentLookupsAndAtomicReplacements(t *testing.T) {
	manager, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(testSnapshot("1", testRoute("one", "/api", "PREFIX", "http://localhost:4001"))); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				snapshot := manager.Current()
				route, _ := lookup(&snapshot, "/api/users", http.MethodGet)
				if route == nil || route.ID != "one" {
					t.Errorf("unexpected concurrent lookup result: %#v", route)
				}
			}
		}()
	}
	for version := 2; version < 20; version++ {
		if err := manager.Apply(testSnapshot(jsonNumber(version), testRoute("one", "/api", "PREFIX", "http://localhost:4001"))); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestExactPrefixMethodsAndRewrites(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) }))
	defer backend.Close()
	manager, _ := NewManager("")
	routes := []Route{
		testRoute("broad", "/api", "PREFIX", backend.URL),
		testRoute("exact", "/api/users", "EXACT", backend.URL),
		testRoute("long", "/api/users", "PREFIX", backend.URL),
	}
	routes[2].StripPrefix = true
	if err := manager.Apply(testSnapshot("1", routes...)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/users/42", nil))
	if get.Code != http.StatusOK || get.Body.String() != "/42" {
		t.Fatalf("prefix strip got %d %q", get.Code, get.Body.String())
	}
	postRoute, _ := lookup(manager.current.Load(), "/api/users", http.MethodPost)
	if postRoute == nil || postRoute.ID != "exact" {
		t.Fatalf("exact route did not win tie: %#v", postRoute)
	}
	methodWriter := httptest.NewRecorder()
	handler.ServeHTTP(methodWriter, httptest.NewRequest(http.MethodDelete, "/api/users", nil))
	if methodWriter.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d", methodWriter.Code)
	}
}

func TestDisabledRouteAndRewrite(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) }))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("rewrite", "/external", "PREFIX", backend.URL)
	route.RewritePrefix = "/internal"
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	manager.Handler(time.Second, testLogger()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/external/x", nil))
	if recorder.Body.String() != "/internal/x" {
		t.Fatalf("rewrite got %q", recorder.Body.String())
	}
	route.Enabled = false
	if err := manager.Apply(testSnapshot("2", route)); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	manager.Handler(time.Second, testLogger()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/external/x", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled route status=%d", recorder.Code)
	}
}

func TestRateLimitAndCache(t *testing.T) {
	var hits atomic.Int32
	var ids atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("limited", "/api", "PREFIX", backend.URL)
	route.RateLimit = 2
	route.CacheTtlMs = 30000
	otherRoute := testRoute("other", "/other", "PREFIX", backend.URL)
	otherRoute.RateLimit = 2
	if err := manager.Apply(testSnapshot("1", route, otherRoute)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	req := func(path, ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(requestIDHeader, fmt.Sprintf("request-%d", ids.Add(1)))
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	firstResponse := req("/api/items", "127.0.0.1:1000")
	if got := firstResponse; got.Code != 200 || got.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first response: %d cache=%q", got.Code, got.Header().Get("X-Cache"))
	}
	secondResponse := req("/api/items", "127.0.0.1:1000")
	if got := secondResponse; got.Code != 200 || got.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("cached response: %d cache=%q", got.Code, got.Header().Get("X-Cache"))
	}
	if firstResponse.Header().Get(requestIDHeader) != "request-1" || secondResponse.Header().Get(requestIDHeader) != "request-2" {
		t.Fatalf("request IDs changed or were reused across cache: first=%q second=%q", firstResponse.Header().Get(requestIDHeader), secondResponse.Header().Get(requestIDHeader))
	}
	blocked := req("/api/items", "127.0.0.1:1000")
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" || !strings.Contains(blocked.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("rate-limit response = %d headers=%v body=%s", blocked.Code, blocked.Header(), blocked.Body.String())
	}
	var rateError map[string]any
	if err := json.Unmarshal(blocked.Body.Bytes(), &rateError); err != nil || rateError["error"] != "rate_limit_exceeded" {
		t.Fatalf("rate-limit JSON error=%v, %v", rateError, err)
	}
	if got := req("/api/items", "127.0.0.2:1000"); got.Code != http.StatusOK {
		t.Fatalf("separate IP status=%d", got.Code)
	}
	if got := req("/other/items", "127.0.0.1:1000"); got.Code != http.StatusOK {
		t.Fatalf("separate route status=%d", got.Code)
	}
	if hits.Load() != 2 {
		t.Fatalf("backend called %d times", hits.Load())
	}
}

func TestMetricsSeparateClientErrorsAndCalculateRates(t *testing.T) {
	manager, _ := NewManager("")
	base := time.Now().Truncate(time.Second)
	manager.state.started = base.Add(-20 * time.Second)
	for _, sample := range []struct {
		status  int
		latency time.Duration
		cache   int
	}{
		{http.StatusOK, time.Millisecond, 1},
		{http.StatusBadRequest, 2 * time.Millisecond, 2},
		{http.StatusNotFound, 3 * time.Millisecond, 0},
		{http.StatusMethodNotAllowed, 4 * time.Millisecond, 0},
		{http.StatusServiceUnavailable, 5 * time.Millisecond, 0},
	} {
		manager.recordRequestStart(base)
		manager.recordRequestComplete(base, sample.status, sample.latency, sample.cache)
	}
	metrics := manager.Metrics()
	if metrics["requestsPerSecond"] != 0.5 {
		t.Fatalf("requests/sec = %v, want .5", metrics["requestsPerSecond"])
	}
	if metrics["averageResponseMs"] != 3.0 {
		t.Fatalf("average response = %v ms, want 3", metrics["averageResponseMs"])
	}
	if metrics["errors"] != uint64(1) || metrics["errorRate"] != 0.2 {
		t.Fatalf("server errors/rate = %v/%v", metrics["errors"], metrics["errorRate"])
	}
	if metrics["clientErrors"] != uint64(3) || metrics["expectedClientErrors"] != uint64(3) {
		t.Fatalf("client errors = %v/%v", metrics["clientErrors"], metrics["expectedClientErrors"])
	}
	if metrics["cacheHits"] != uint64(1) || metrics["cacheMisses"] != uint64(1) || metrics["cacheHitRate"] != 0.5 {
		t.Fatalf("cache metrics = %v/%v/%v", metrics["cacheHits"], metrics["cacheMisses"], metrics["cacheHitRate"])
	}
	statuses := metrics["statusCodes"].(map[string]uint64)
	for _, status := range []int{200, 400, 404, 405, 503} {
		if statuses[strconv.Itoa(status)] != 1 {
			t.Fatalf("status distribution = %#v", statuses)
		}
	}
	points := metrics["timeseries"].([]metricPoint)
	found := false
	for _, point := range points {
		if point.Timestamp == base.Unix() {
			found = point.Requests == 5 && point.AverageResponseMs == 3
		}
	}
	if !found {
		t.Fatalf("time-series does not contain the expected request bucket: %#v", points)
	}
}

func TestCacheExpirationAndConfigurationInvalidation(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, calls.Add(1)) }))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("cache-expiry", "/items", "PREFIX", backend.URL)
	route.CacheTtlMs = 200
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	request := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/items/1", nil))
		return response
	}
	first := request()
	second := request()
	if first.Body.String() != "1" || second.Body.String() != "1" || second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("initial cache: %q / %q", first.Body.String(), second.Body.String())
	}
	time.Sleep(220 * time.Millisecond)
	expired := request()
	if expired.Body.String() != "2" || expired.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expired response: %q %q", expired.Body.String(), expired.Header().Get("X-Cache"))
	}
	if err := manager.Apply(testSnapshot("2", route)); err != nil {
		t.Fatal(err)
	}
	invalidated := request()
	if invalidated.Body.String() != "3" || invalidated.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("after config update: %q %q", invalidated.Body.String(), invalidated.Header().Get("X-Cache"))
	}
}

func TestCacheSkipsPostErrorsAndSetCookie(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/cookie":
			w.Header().Set("Set-Cookie", "session=secret; HttpOnly")
		case "/error":
			http.Error(w, "backend error", http.StatusInternalServerError)
		}
	}))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("no-cache", "/", "PREFIX", backend.URL)
	route.CacheTtlMs = 30000
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	for range 2 {
		if got := request(http.MethodGet, "/cookie"); got.Header().Get("X-Cache") != "" {
			t.Fatalf("Set-Cookie response was cacheable: %v", got.Header())
		}
		if got := request(http.MethodGet, "/error"); got.Code != http.StatusInternalServerError {
			t.Fatalf("error status=%d", got.Code)
		}
		if got := request(http.MethodPost, "/post"); got.Header().Get("X-Cache") != "" {
			t.Fatalf("POST response was cacheable: %v", got.Header())
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("backend calls=%d, want 6", calls.Load())
	}
	metrics := manager.Metrics()
	if metrics["cacheHits"] != uint64(0) || metrics["cacheMisses"] != uint64(4) {
		t.Fatalf("cache lookup metrics=%v/%v", metrics["cacheHits"], metrics["cacheMisses"])
	}
}

func TestConcurrentRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("concurrent", "/api", "PREFIX", backend.URL)
	route.RateLimit = 1000
	route.CacheTtlMs = 30000
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	var wg sync.WaitGroup
	errCh := make(chan string, 50)
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
			req.RemoteAddr = "127.0.0.1:" + strconv.Itoa(1000+i)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusOK || response.Body.String() != "ok" {
				errCh <- response.Result().Status
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent request failed: %s", err)
	}
	if got := manager.Metrics()["totalRequests"]; got != uint64(50) {
		t.Fatalf("request count=%v, want 50", got)
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	var a, b atomic.Int32
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.Add(1) }))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.Add(1) }))
	defer two.Close()
	manager, _ := NewManager("")
	route := testRoute("lb", "/lb", "PREFIX", one.URL)
	route.Targets[0].Weight = 3
	route.Targets = append(route.Targets, Target{URL: two.URL, Weight: 1})
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	first, second := int32(0), int32(0)
	handler := manager.Handler(time.Second, testLogger())
	for i := 0; i < 100; i++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/lb/item", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("request %d status=%d", i, response.Code)
		}
	}
	first, second = a.Load(), b.Load()
	if first != 75 || second != 25 {
		t.Fatalf("weighted round robin requests = %d/%d, want 75/25", first, second)
	}
	if first+second != 100 {
		t.Fatalf("weighted targets handled %d requests", first+second)
	}
}

func TestBackendHealthReportsTarget(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probe path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	manager, _ := NewManager("")
	if err := manager.Apply(testSnapshot("1", testRoute("healthy", "/x", "PREFIX", backend.URL))); err != nil {
		t.Fatal(err)
	}
	status := manager.Health()
	if status["healthy:0"] != true {
		t.Fatalf("health status=%v", status)
	}
}

func TestFailedBackendIsSkippedForFailover(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("healthy")) }))
	defer healthy.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("dead")) }))
	deadURL := dead.URL
	dead.Close()
	manager, _ := NewManager("")
	route := testRoute("failover", "/x", "PREFIX", healthy.URL)
	route.Targets = append(route.Targets, Target{URL: deadURL, Weight: 1})
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler(time.Second, testLogger())
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/x", nil))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first status=%d", first.Code)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/x", nil))
	if second.Code != http.StatusOK || second.Body.String() != "healthy" {
		t.Fatalf("failover response=%d %q", second.Code, second.Body.String())
	}
}

func TestUnhealthyUpstreamExclusionRecoveryAndRouteIsolation(t *testing.T) {
	var unavailable atomic.Bool
	var backupRequests atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "primary") }))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if unavailable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		backupRequests.Add(1)
		_, _ = io.WriteString(w, "backup")
	}))
	defer backup.Close()
	manager, _ := NewManager("")
	route := testRoute("main", "/main", "PREFIX", primary.URL)
	route.Targets = append(route.Targets, Target{URL: backup.URL, Weight: 1})
	other := testRoute("other-route", "/other", "PREFIX", primary.URL)
	if err := manager.Apply(testSnapshot("1", route, other)); err != nil {
		t.Fatal(err)
	}
	if states := manager.Health(); !states["main:0"] || !states["main:1"] {
		t.Fatalf("initial health=%v", states)
	}
	unavailable.Store(true)
	states := manager.Health()
	if states["main:1"] {
		t.Fatalf("unhealthy target still marked healthy: %v", states)
	}
	handler := manager.Handler(time.Second, testLogger())
	for i := 0; i < 4; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/main/items", nil))
		if w.Code != http.StatusOK || w.Body.String() != "primary" {
			t.Fatalf("healthy route interrupted: %d %q", w.Code, w.Body.String())
		}
	}
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, httptest.NewRequest(http.MethodGet, "/other/items", nil))
	if otherResponse.Code != http.StatusOK || otherResponse.Body.String() != "primary" {
		t.Fatalf("unrelated route interrupted: %d %q", otherResponse.Code, otherResponse.Body.String())
	}
	if backupRequests.Load() != 0 {
		t.Fatalf("unhealthy backend received %d requests", backupRequests.Load())
	}
	unavailable.Store(false)
	states = manager.Health()
	if !states["main:1"] {
		t.Fatalf("recovered backend not healthy: %v", states)
	}
	for i := 0; i < 12; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/main/items", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("recovery request status=%d", w.Code)
		}
	}
	if backupRequests.Load() == 0 {
		t.Fatal("recovered backend never received traffic")
	}
}

func TestInternalConfigUpdateAuthentication(t *testing.T) {
	manager, _ := NewManager("")
	server, err := New(Config{Addr: ":8080", RequestTimeout: time.Second, UpstreamTimeout: time.Second, Manager: manager, AdminToken: "secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(testSnapshot("1", testRoute("one", "/x", "PREFIX", "http://localhost:4001")))
	for _, test := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"secret-token", http.StatusNoContent}} {
		request := httptest.NewRequest(http.MethodPut, "/internal/config", strings.NewReader(string(payload)))
		if test.token != "" {
			request.Header.Set("Authorization", "Bearer "+test.token)
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("status=%d want %d", response.Code, test.want)
		}
	}
}

func TestNewManagerFallsBackToEmptyWhenNoValidSnapshotExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"version":"bad","routes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(path)
	if err != nil {
		t.Fatalf("manager should start from empty fallback: %v", err)
	}
	if manager.Current().Version != "0" {
		t.Fatalf("fallback version=%s", manager.Current().Version)
	}
	if _, err := os.Stat(path + ".last-good"); err != nil {
		t.Fatalf("missing durable last-good file: %v", err)
	}
}

func TestCorruptPrimaryRestoresLastKnownGoodBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(testSnapshot("1", testRoute("users", "/users", "PREFIX", "http://localhost:4001"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"invalid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Current().Version != "1" {
		t.Fatalf("recovered version=%s", recovered.Current().Version)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil || restored.Version != "1" {
		t.Fatalf("primary was not restored from backup: %#v, %v", restored, err)
	}
}

func jsonNumber(value int) string { return strconv.Itoa(value) }

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
