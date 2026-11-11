package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCanaryGroupSelectionIsDeterministic(t *testing.T) {
	manager, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	route := testRoute("split", "/split", "PREFIX", "http://stable.example")
	route.UpstreamGroups = []TargetGroup{
		{ID: "stable", Name: "Stable", Percentage: 90, Targets: []Target{{URL: "http://stable.example", Weight: 1}}},
		{ID: "canary", Name: "Canary", Percentage: 10, Targets: []Target{{URL: "http://canary.example", Weight: 1}}},
	}
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[manager.requestGroups(route)[0].Targets[0].URL]++
	}
	if counts["http://stable.example"] != 90 || counts["http://canary.example"] != 10 {
		t.Fatalf("unexpected split distribution: %#v", counts)
	}
}

func TestCanaryGroupsRequireValidPercentages(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("split", "/split", "PREFIX", "http://stable.example")
	route.UpstreamGroups = []TargetGroup{
		{ID: "stable", Name: "Stable", Percentage: 80, Targets: []Target{{URL: "http://stable.example", Weight: 1}}},
		{ID: "canary", Name: "Canary", Percentage: 10, Targets: []Target{{URL: "http://canary.example", Weight: 1}}},
	}
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("accepted percentages not totaling 100")
	}
}

func TestCanaryFallsBackWhenSelectedGroupIsUnhealthy(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("split-health", "/split-health", "PREFIX", "http://stable.example")
	route.UpstreamGroups = []TargetGroup{
		{ID: "stable", Name: "Stable", Percentage: 90, Targets: []Target{{URL: "http://stable.example", Weight: 1}}},
		{ID: "canary", Name: "Canary", Percentage: 10, Targets: []Target{{URL: "http://canary.example", Weight: 1}}},
	}
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	sequence, _ := manager.groupSequence.LoadOrStore(route.ID, &atomic.Uint64{})
	sequence.(*atomic.Uint64).Store(89) // Next selection is within the 10% canary range.
	groups := manager.requestGroups(route)
	if len(groups) != 2 || groups[0].Targets[0].GroupID != "canary" {
		t.Fatalf("unexpected group ordering: %#v", groups)
	}
	manager.state.mu.Lock()
	manager.state.probes[upstreamKey(route.ID, "http://canary.example")] = false
	manager.state.mu.Unlock()
	if _, ok := manager.availableTarget(groups[0], time.Now()); ok {
		t.Fatal("unhealthy canary target was selected")
	}
	selected, ok := manager.availableTarget(groups[1], time.Now())
	if !ok || selected.URL != "http://stable.example" {
		t.Fatalf("healthy stable fallback=%#v, available=%v", selected, ok)
	}
}

func TestRequestHistoryIsBoundedAndFiltered(t *testing.T) {
	manager, _ := NewManager("")
	for i := 0; i < maxRequestRecords+2; i++ {
		manager.state.mu.Lock()
		manager.state.requestsHistory = append(manager.state.requestsHistory, RequestRecord{RequestID: fmt.Sprintf("request-%d", i), RouteID: "users", Status: 200})
		manager.state.mu.Unlock()
	}
	manager.state.mu.Lock()
	if len(manager.state.requestsHistory) > maxRequestRecords {
		manager.state.requestsHistory = append([]RequestRecord(nil), manager.state.requestsHistory[len(manager.state.requestsHistory)-maxRequestRecords:]...)
	}
	manager.state.mu.Unlock()
	if got := len(manager.RequestHistory(nil, 200)); got != 200 {
		t.Fatalf("history limit=%d, want 200", got)
	}
	filtered := manager.RequestHistory(map[string]string{"requestId": "request-1001"}, 10)
	if len(filtered) != 1 || filtered[0].RequestID != "request-1001" {
		t.Fatalf("filter result=%#v", filtered)
	}
}

func TestCircuitOpensHalfOpensAndClosesAfterRecovery(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("circuit", "/circuit", "PREFIX", "http://127.0.0.1:1")
	route.CircuitBreakerEnabled = true
	route.CircuitFailureThreshold = 2
	route.CircuitRecoveryTimeoutMs = 100
	route.CircuitHalfOpenProbes = 1
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	manager.finishUpstream(route, route.Targets[0], time.Now(), 503, nil)
	manager.finishUpstream(route, route.Targets[0], time.Now(), 502, nil)
	if got := manager.state.upstreams[upstreamKey(route.ID, route.Targets[0].URL)].state; got != circuitOpen {
		t.Fatalf("state after failures=%s", got)
	}
	if _, ok := manager.availableTarget(route, time.Now()); ok {
		t.Fatal("open circuit admitted request before recovery timeout")
	}
	time.Sleep(105 * time.Millisecond)
	target, ok := manager.availableTarget(route, time.Now())
	if !ok || manager.state.upstreams[upstreamKey(route.ID, target.URL)].state != circuitHalfOpen {
		t.Fatal("circuit did not admit recovery probe")
	}
	manager.finishUpstream(route, target, time.Now(), 200, nil)
	if got := manager.state.upstreams[upstreamKey(route.ID, target.URL)].state; got != circuitClosed {
		t.Fatalf("recovered state=%s", got)
	}
	events := manager.state.circuitEvents
	if len(events) != 3 || events[0].To != circuitOpen || events[1].To != circuitHalfOpen || events[2].To != circuitClosed {
		t.Fatalf("transitions=%+v", events)
	}
}

func TestHalfOpenFailureReopensAndRoutesHaveIndependentCircuits(t *testing.T) {
	manager, _ := NewManager("")
	first := testRoute("first", "/first", "PREFIX", "http://127.0.0.1:1")
	second := testRoute("second", "/second", "PREFIX", "http://127.0.0.1:2")
	for _, route := range []*Route{&first, &second} {
		route.CircuitBreakerEnabled = true
		route.CircuitFailureThreshold = 1
		route.CircuitRecoveryTimeoutMs = 100
		route.CircuitHalfOpenProbes = 1
	}
	if err := manager.Apply(testSnapshot("1", first, second)); err != nil {
		t.Fatal(err)
	}
	manager.finishUpstream(first, first.Targets[0], time.Now(), 500, nil)
	if _, ok := manager.availableTarget(first, time.Now()); ok {
		t.Fatal("first route circuit should be open")
	}
	if _, ok := manager.availableTarget(second, time.Now()); !ok {
		t.Fatal("another route was blocked by the first route's circuit")
	}
	time.Sleep(105 * time.Millisecond)
	target, ok := manager.availableTarget(first, time.Now())
	if !ok {
		t.Fatal("half-open probe not allowed")
	}
	manager.finishUpstream(first, target, time.Now(), 503, nil)
	if manager.state.upstreams[upstreamKey(first.ID, target.URL)].state != circuitOpen {
		t.Fatal("failed half-open probe did not reopen circuit")
	}
}

func TestCircuitIsScopedToAnUpstreamInstance(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("two-targets", "/two-targets", "PREFIX", "http://bad")
	route.Targets = append(route.Targets, Target{URL: "http://good", Weight: 1})
	route.CircuitBreakerEnabled = true
	route.CircuitFailureThreshold = 1
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	manager.finishUpstream(route, route.Targets[0], time.Now(), 502, nil)
	target, ok := manager.availableTarget(route, time.Now())
	if !ok || target.URL != "http://good" {
		t.Fatalf("open instance was not excluded independently: %#v, %v", target, ok)
	}
	manager.finishUpstream(route, target, time.Now(), 200, nil)
}

func TestClientErrorsAndCancelledRequestsDoNotCountAsCircuitFailures(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("classification", "/classification", "PREFIX", "http://127.0.0.1:1")
	route.CircuitBreakerEnabled = true
	route.CircuitFailureThreshold = 2
	manager.finishUpstream(route, route.Targets[0], time.Now(), http.StatusNotFound, nil)
	manager.finishUpstream(route, route.Targets[0], time.Now(), 0, context.Canceled)
	state := manager.state.upstreams[upstreamKey(route.ID, route.Targets[0].URL)]
	if state.failureCount != 0 || state.failures != 0 || state.state != circuitClosed {
		t.Fatalf("client/cancel failures affected breaker: %+v", state)
	}
	manager.finishUpstream(route, route.Targets[0], time.Now(), 502, nil)
	if state.failures != 1 {
		t.Fatalf("server failure count=%d, want 1", state.failures)
	}
	manager.finishUpstream(route, route.Targets[0], time.Now(), 200, nil)
	if state.failures != 0 {
		t.Fatalf("success did not reset consecutive failures: %d", state.failures)
	}
}

func TestHalfOpenProbeLimitIsEnforced(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("probe-limit", "/probe-limit", "PREFIX", "http://127.0.0.1:1")
	route.CircuitBreakerEnabled = true
	route.CircuitFailureThreshold = 1
	route.CircuitRecoveryTimeoutMs = 100
	route.CircuitHalfOpenProbes = 1
	manager.finishUpstream(route, route.Targets[0], time.Now(), 500, nil)
	key := upstreamKey(route.ID, route.Targets[0].URL)
	manager.state.upstreams[key].openedAt = time.Now().Add(-time.Second)
	target, ok := manager.availableTarget(route, time.Now())
	if !ok {
		t.Fatal("first half-open probe was not admitted")
	}
	if _, ok = manager.availableTarget(route, time.Now()); ok {
		t.Fatal("half-open probe limit admitted a second concurrent probe")
	}
	manager.finishUpstream(route, target, time.Now(), 200, nil)
	if _, ok = manager.availableTarget(route, time.Now()); !ok {
		t.Fatal("closed circuit did not admit normal traffic")
	}
}

func TestConcurrentCircuitAccountingReleasesConnections(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("parallel", "/parallel", "PREFIX", "http://127.0.0.1:1")
	route.CircuitBreakerEnabled = true
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				target, ok := manager.availableTarget(route, time.Now())
				if ok {
					manager.finishUpstream(route, target, time.Now(), 200, nil)
				}
			}
		}()
	}
	wg.Wait()
	if active := manager.state.upstreams[upstreamKey(route.ID, route.Targets[0].URL)].active; active != 0 {
		t.Fatalf("active count leaked: %d", active)
	}
}

func TestActiveConnectionCounterCoversResponseBodyAndReleasesOnClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	}))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("active", "/active", "PREFIX", backend.URL)
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager.Handler(time.Second, testLogger()))
	defer server.Close()
	responseCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		response, err := http.Get(server.URL + "/active")
		if err != nil {
			errCh <- err
			return
		}
		responseCh <- response
	}()
	<-started
	activeCount := func() int {
		manager.state.mu.Lock()
		defer manager.state.mu.Unlock()
		return manager.state.upstreams[upstreamKey(route.ID, route.Targets[0].URL)].active
	}
	if got := activeCount(); got != 1 {
		t.Fatalf("active requests before body close=%d", got)
	}
	close(release)
	select {
	case err := <-errCh:
		t.Fatal(err)
	case response := <-responseCh:
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	case <-time.After(time.Second):
		t.Fatal("request did not finish")
	}
	if got := activeCount(); got != 0 {
		t.Fatalf("active request counter leaked: %d", got)
	}
}

func TestRetryConnectionFailureAndRetryableStatus(t *testing.T) {
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { firstHits.Add(1); http.Error(w, "temporary", 503) }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	}))
	defer second.Close()
	manager, _ := NewManager("")
	route := testRoute("retry", "/retry", "PREFIX", first.URL)
	route.Targets = append(route.Targets, Target{URL: second.URL, Weight: 1})
	route.LoadBalancingAlgorithm = "ROUND_ROBIN"
	route.MaxRetries = 1
	route.RetryBackoffMs = 1
	route.RetryMaxBackoffMs = 10
	route.RetryStatuses = []int{503}
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	manager.sequence.Store(route.ID, &atomic.Uint64{})
	sequence, _ := manager.sequence.Load(route.ID)
	sequence.(*atomic.Uint64).Store(1)
	server := httptest.NewServer(manager.Handler(time.Second, testLogger()))
	defer server.Close()
	response, err := http.Get(server.URL + "/retry/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "ok" || firstHits.Load() != 1 || secondHits.Load() != 1 {
		t.Fatalf("status=%d body=%q hits=%d/%d", response.StatusCode, body, firstHits.Load(), secondHits.Load())
	}
	metrics := manager.Metrics()
	if metrics["retryAttempts"] != uint64(1) || metrics["retrySuccesses"] != uint64(1) {
		t.Fatalf("retry metrics=%v", metrics)
	}
}

func TestPOSTIsNotRetriedAndExplicitSafeBodyReplayWorks(t *testing.T) {
	for _, safe := range []bool{false, true} {
		var hits atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != "payload" {
				t.Errorf("upstream body=%q", body)
			}
			if hits.Load() == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(201)
		}))
		manager, _ := NewManager("")
		route := testRoute("post", "/post", "PREFIX", backend.URL)
		route.MaxRetries = 1
		route.RetryBackoffMs = 1
		route.RetryMaxBackoffMs = 10
		route.RetryStatuses = []int{503}
		route.RetryNonIdempotent = safe
		if err := manager.Apply(testSnapshot("1", route)); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(manager.Handler(time.Second, testLogger()))
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/post", strings.NewReader("payload"))
		if safe {
			request.Header.Set("Idempotency-Key", "test-123")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		expected := int32(1)
		expectedStatus := 503
		if safe {
			expected = 2
			expectedStatus = 201
		}
		if hits.Load() != expected || response.StatusCode != expectedStatus {
			t.Fatalf("safe=%v hits=%d status=%d", safe, hits.Load(), response.StatusCode)
		}
		server.Close()
		backend.Close()
	}
}

func TestRetryBackoffExponentialCapAndCancel(t *testing.T) {
	route := Route{RetryBackoffMs: 10, RetryMaxBackoffMs: 25}
	if retryDelay(route, 1) != 10*time.Millisecond || retryDelay(route, 2) != 20*time.Millisecond || retryDelay(route, 3) != 25*time.Millisecond {
		t.Fatal("retry delay is not exponential with a cap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	manager, _ := NewManager("")
	_, err := (retryRoundTripper{manager: manager, route: testRoute("cancel", "/", "PREFIX", "http://127.0.0.1:1"), maxRetries: 1}).RoundTrip(request)
	if err == nil {
		t.Fatal("cancelled request unexpectedly continued")
	}
}

func TestLoadBalancersRespectConfiguredAlgorithm(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("balance", "/balance", "PREFIX", "http://one")
	route.Targets = append(route.Targets, Target{URL: "http://two", Weight: 1})
	route.LoadBalancingAlgorithm = "ROUND_ROBIN"
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	a, _ := manager.availableTarget(route, time.Now())
	manager.finishUpstream(route, a, time.Now(), 200, nil)
	b, _ := manager.availableTarget(route, time.Now())
	manager.finishUpstream(route, b, time.Now(), 200, nil)
	if a.URL == b.URL {
		t.Fatalf("round robin selected %s twice", a.URL)
	}
	route.LoadBalancingAlgorithm = "LEAST_CONNECTIONS"
	manager.state.upstreams[upstreamKey(route.ID, a.URL)].active = 0
	manager.state.upstreams[upstreamKey(route.ID, b.URL)].active = 4
	selected := manager.chooseTargetLockedForTest(route)
	if selected.URL != a.URL {
		t.Fatalf("least-connections picked %s, expected %s", selected.URL, a.URL)
	}
	route.LoadBalancingAlgorithm = "LEAST_RESPONSE_TIME"
	manager.state.upstreams[upstreamKey(route.ID, a.URL)].requests = 4
	manager.state.upstreams[upstreamKey(route.ID, a.URL)].avgLatencyNs = uint64(25 * time.Millisecond)
	manager.state.upstreams[upstreamKey(route.ID, b.URL)].requests = 4
	manager.state.upstreams[upstreamKey(route.ID, b.URL)].avgLatencyNs = uint64(5 * time.Millisecond)
	selected = manager.chooseTargetLockedForTest(route)
	if selected.URL != b.URL {
		t.Fatalf("least-response-time picked %s, expected %s", selected.URL, b.URL)
	}
}

func TestRetryConnectionFailureFallsBackAndRetryExhaustion(t *testing.T) {
	var successes atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { successes.Add(1); _, _ = io.WriteString(w, "recovered") }))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("connect-retry", "/connect-retry", "PREFIX", "http://127.0.0.1:1")
	route.Targets = append(route.Targets, Target{URL: backend.URL, Weight: 1})
	route.LoadBalancingAlgorithm = "ROUND_ROBIN"
	route.MaxRetries, route.RetryBackoffMs, route.RetryMaxBackoffMs = 1, 1, 10
	route.RetryConnectionErrors = true
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	manager.sequence.Store(route.ID, &atomic.Uint64{})
	sequence, _ := manager.sequence.Load(route.ID)
	sequence.(*atomic.Uint64).Store(1)
	server := httptest.NewServer(manager.Handler(time.Second, testLogger()))
	defer server.Close()
	response, err := http.Get(server.URL + "/connect-retry")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "recovered" || successes.Load() != 1 {
		t.Fatalf("status=%d body=%q successes=%d", response.StatusCode, body, successes.Load())
	}
	if health := manager.Health(); health[route.ID+":0"] {
		t.Fatal("failed connection remained marked healthy")
	}

	var failedHits atomic.Int32
	exhaustedBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { failedHits.Add(1); w.WriteHeader(503) }))
	defer exhaustedBackend.Close()
	exhaustedManager, _ := NewManager("")
	exhausted := testRoute("exhausted", "/exhausted", "PREFIX", exhaustedBackend.URL)
	exhausted.MaxRetries, exhausted.RetryBackoffMs, exhausted.RetryMaxBackoffMs = 2, 1, 10
	exhausted.RetryStatuses = []int{503}
	if err := exhaustedManager.Apply(testSnapshot("1", exhausted)); err != nil {
		t.Fatal(err)
	}
	exhaustedServer := httptest.NewServer(exhaustedManager.Handler(time.Second, testLogger()))
	defer exhaustedServer.Close()
	response, err = http.Get(exhaustedServer.URL + "/exhausted")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 || failedHits.Load() != 3 {
		t.Fatalf("exhaustion status=%d hits=%d", response.StatusCode, failedHits.Load())
	}
	metrics := exhaustedManager.Metrics()
	if metrics["retryAttempts"] != uint64(2) || metrics["retryExhausted"] != uint64(1) {
		t.Fatalf("exhaustion metrics=%v", metrics)
	}
}

func TestNonRetryableClientErrorDoesNotRetryAndTimeoutHonorsDeadline(t *testing.T) {
	var clientHits atomic.Int32
	clientBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { clientHits.Add(1); w.WriteHeader(404) }))
	defer clientBackend.Close()
	manager, _ := NewManager("")
	route := testRoute("client-error", "/client-error", "PREFIX", clientBackend.URL)
	route.MaxRetries, route.RetryBackoffMs, route.RetryMaxBackoffMs = 3, 1, 10
	route.RetryStatuses = []int{503}
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager.Handler(time.Second, testLogger()))
	defer server.Close()
	response, err := http.Get(server.URL + "/client-error")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 || clientHits.Load() != 1 {
		t.Fatalf("404 retries=%d status=%d", clientHits.Load(), response.StatusCode)
	}

	var timeoutHits atomic.Int32
	timeoutBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		timeoutHits.Add(1)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer timeoutBackend.Close()
	timeoutManager, _ := NewManager("")
	timeoutRoute := testRoute("deadline", "/deadline", "PREFIX", timeoutBackend.URL)
	timeoutRoute.TimeoutMs = 50
	timeoutRoute.MaxRetries, timeoutRoute.RetryBackoffMs, timeoutRoute.RetryMaxBackoffMs = 2, 1, 10
	timeoutRoute.RetryTimeouts = true
	if err := timeoutManager.Apply(testSnapshot("1", timeoutRoute)); err != nil {
		t.Fatal(err)
	}
	timeoutServer := httptest.NewServer(timeoutManager.Handler(time.Second, testLogger()))
	defer timeoutServer.Close()
	started := time.Now()
	response, err = http.Get(timeoutServer.URL + "/deadline")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusGatewayTimeout || time.Since(started) > 150*time.Millisecond || timeoutHits.Load() != 1 {
		t.Fatalf("deadline status=%d elapsed=%s hits=%d", response.StatusCode, time.Since(started), timeoutHits.Load())
	}
}

func (m *Manager) chooseTargetLockedForTest(route Route) Target {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	return m.chooseTargetLocked(route, route.Targets)
}

func TestValidateReliabilityConfiguration(t *testing.T) {
	manager, _ := NewManager("")
	route := testRoute("invalid", "/invalid", "PREFIX", "http://127.0.0.1:1")
	route.LoadBalancingAlgorithm = "MAGIC"
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("unsupported balancing algorithm accepted")
	}
	route.LoadBalancingAlgorithm = "ROUND_ROBIN"
	route.MaxRetries = 6
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("excessive retries accepted")
	}
	route.MaxRetries = 1
	route.RetryBackoffMs = 0
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("retry configuration without backoff accepted")
	}
	route.RetryBackoffMs = 10
	route.RetryStatuses = []int{404}
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("client error accepted as retry status")
	}
	route.RetryStatuses = nil
	route.Targets = make([]Target, 101)
	for i := range route.Targets {
		route.Targets[i] = Target{URL: fmt.Sprintf("http://backend-%d.example", i), Weight: 1}
	}
	if err := manager.Apply(testSnapshot("1", route)); err == nil {
		t.Fatal("unbounded upstream target count accepted")
	}
}
