package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func adaptiveTestRoute() Route {
	route := testRoute("adaptive", "/adaptive", "PREFIX", "http://localhost:4001")
	route.RateLimit, route.RateLimitMode, route.AdaptiveEnabled = 100, "ADAPTIVE", true
	route.MinimumRateLimit, route.MaximumRateLimit = 20, 100
	route.SpikeSensitivity, route.RecoveryCooldownMs = 2, 30000
	return route
}

func TestAdaptiveBaselineWarmupAndSpikeReduction(t *testing.T) {
	manager, _ := NewManager("")
	route := adaptiveTestRoute()
	start := time.Unix(1000, 0)
	for i := 0; i < 4; i++ {
		decision := manager.rateLimitDecision(route, "client", start.Add(time.Duration(i)*5*time.Second))
		if !decision.Allowed || decision.EffectiveLimit != 100 {
			t.Fatalf("warmup decision=%+v", decision)
		}
	}
	// Four steady five-second observations establish a usable low baseline.
	for i := 0; i < 15; i++ {
		manager.rateLimitDecision(route, "burst", start.Add(20*time.Second))
	}
	decision := manager.rateLimitDecision(route, "observer", start.Add(25*time.Second))
	if !decision.Allowed || decision.EffectiveLimit != 75 {
		t.Fatalf("spike did not lower effective limit: %+v", decision)
	}
	manager.state.mu.Lock()
	state := manager.state.adaptive[route.ID]
	if state.spikes != 1 || state.baselineRPS <= 0 || state.effective != 75 {
		t.Fatalf("adaptive state=%+v", state)
	}
	manager.state.mu.Unlock()
	history := manager.AdaptiveHistory()
	seen := map[string]bool{}
	for _, event := range history {
		seen[event.Type] = true
	}
	if !seen["spike_detected"] || !seen["effective_limit_reduced"] {
		t.Fatalf("missing explainability history: %+v", history)
	}
}

func TestAdaptiveRequiresReliableBaselineAndRespectsThreshold(t *testing.T) {
	manager, _ := NewManager("")
	route := adaptiveTestRoute()
	state := &adaptiveRouteState{effective: 100, baselineRPS: 1, evaluations: 2}
	manager.evaluateAdaptiveLocked(route, state, 100, time.Now())
	if state.effective != 100 || state.spikes != 0 {
		t.Fatalf("unreliable baseline made aggressive decision: %+v", state)
	}
	state = &adaptiveRouteState{effective: 100, baselineRPS: 1, evaluations: 3}
	manager.evaluateAdaptiveLocked(route, state, 2, time.Now())
	if state.effective != 100 || state.spikes != 0 {
		t.Fatalf("threshold equality was treated as a spike: %+v", state)
	}
	state.baselineRPS = 1
	manager.evaluateAdaptiveLocked(route, state, 2.01, time.Now())
	if state.effective != 75 {
		t.Fatalf("strict threshold did not reduce limit: %+v", state)
	}
}

func TestAdaptiveMinimumMaximumAndGradualRecoveryCooldown(t *testing.T) {
	manager, _ := NewManager("")
	route := adaptiveTestRoute()
	state := &adaptiveRouteState{effective: 100, baselineRPS: 1, evaluations: 3}
	route.MinimumRateLimit = 80
	manager.evaluateAdaptiveLocked(route, state, 3, time.Unix(1000, 0))
	if state.effective != 80 {
		t.Fatalf("minimum bound=%d, want 80", state.effective)
	}
	route.MinimumRateLimit = 20
	route.MaximumRateLimit = 60
	state = &adaptiveRouteState{effective: 60, baselineRPS: 1, evaluations: 3}
	manager.evaluateAdaptiveLocked(route, state, 3, time.Unix(1000, 0))
	if state.effective > 60 {
		t.Fatalf("maximum bound exceeded: %d", state.effective)
	}

	route.MaximumRateLimit = 100
	route.RecoveryCooldownMs = 1000
	state = &adaptiveRouteState{effective: 75, baselineRPS: 1, evaluations: 3, lastSpike: time.Unix(2000, 0)}
	manager.evaluateAdaptiveLocked(route, state, 1, time.Unix(2000, 0).Add(999*time.Millisecond))
	if state.recovering {
		t.Fatal("recovery started before cooldown")
	}
	manager.evaluateAdaptiveLocked(route, state, 1, time.Unix(2000, 0).Add(time.Second))
	if !state.recovering || state.effective != 75 {
		t.Fatalf("recovery should start without an immediate jump: %+v", state)
	}
	manager.evaluateAdaptiveLocked(route, state, 1, time.Unix(2000, 0).Add(2*time.Second))
	if state.effective != 80 {
		t.Fatalf("recovery was not gradual: %d", state.effective)
	}
}

func TestFixedCompatibilityClientAndRouteIsolationAndOverrideExpiry(t *testing.T) {
	manager, _ := NewManager("")
	fixed := adaptiveTestRoute()
	fixed.RateLimitMode = "FIXED"
	fixed.AdaptiveEnabled = false
	fixed.RateLimit = 2
	fixed.MinimumRateLimit, fixed.MaximumRateLimit = 1, 2
	first := manager.rateLimitDecision(fixed, "one", time.Now())
	second := manager.rateLimitDecision(fixed, "one", time.Now().Add(time.Millisecond))
	third := manager.rateLimitDecision(fixed, "one", time.Now().Add(2*time.Millisecond))
	otherClient := manager.rateLimitDecision(fixed, "two", time.Now().Add(3*time.Millisecond))
	otherRoute := fixed
	otherRoute.ID = "other"
	isolated := manager.rateLimitDecision(otherRoute, "one", time.Now().Add(4*time.Millisecond))
	if !first.Allowed || !second.Allowed || third.Allowed || !otherClient.Allowed || !isolated.Allowed {
		t.Fatalf("fixed/isolation decisions: %+v %+v %+v %+v %+v", first, second, third, otherClient, isolated)
	}
	if third.Mode != "FIXED" || third.Reason == "" {
		t.Fatalf("fixed rejection missing mode/reason: %+v", third)
	}

	adaptive := adaptiveTestRoute()
	adaptive.ID = "adaptive-override"
	now := time.Now()
	adaptive.ManualOverrideLimit = 40
	adaptive.ManualOverrideExpiresAt = now.Add(time.Minute).UnixMilli()
	overridden := manager.rateLimitDecision(adaptive, "admin", now)
	if overridden.EffectiveLimit != 40 || overridden.Mode != "ADAPTIVE_OVERRIDE" {
		t.Fatalf("manual override=%+v", overridden)
	}
	adaptive.ManualOverrideExpiresAt = now.Add(-time.Second).UnixMilli()
	expired := manager.rateLimitDecision(adaptive, "admin", now.Add(time.Second))
	if expired.EffectiveLimit != 100 || expired.Mode != "ADAPTIVE" {
		t.Fatalf("expired override remained active: %+v", expired)
	}
}

func TestAdaptiveConcurrentDecisionsAndExpiredClientCleanup(t *testing.T) {
	manager, _ := NewManager("")
	route := adaptiveTestRoute()
	route.RateLimit = 5
	route.MinimumRateLimit = 1
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision := manager.rateLimitDecision(route, "shared", time.Now())
			if decision.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 5 {
		t.Fatalf("concurrent per-IP allowance=%d, want 5", allowed)
	}
	manager.state.mu.Lock()
	manager.state.clients["stale|ip"] = []time.Time{time.Now().Add(-2 * time.Minute)}
	manager.state.clientHitCount = 1
	manager.state.decisionCount = 127
	manager.state.mu.Unlock()
	manager.rateLimitDecision(route, "new", time.Now())
	manager.state.mu.Lock()
	defer manager.state.mu.Unlock()
	if _, exists := manager.state.clients["stale|ip"]; exists {
		t.Fatal("expired client state was retained")
	}
}

func TestAdaptiveInvalidConfigurationRejectedAndHeadersExplainDecision(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	manager, _ := NewManager("")
	route := testRoute("header", "/header", "PREFIX", backend.URL)
	route.RateLimit = 1
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	invalid := route
	invalid.RateLimitMode = "ADAPTIVE"
	invalid.AdaptiveEnabled = true
	invalid.MinimumRateLimit = 2
	if err := ValidateSnapshot(testSnapshot("2", invalid)); err == nil {
		t.Fatal("invalid minimum above base accepted")
	}
	invalid = adaptiveTestRoute()
	invalid.MaximumRateLimit = 60
	if err := ValidateSnapshot(testSnapshot("2", invalid)); err == nil {
		t.Fatal("maximum below base accepted")
	}
	handler := manager.Handler(time.Second, testLogger())
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/header", nil))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/header", nil))
	if first.Header().Get("X-RateLimit-Limit") != "1" || first.Header().Get("X-RateLimit-Remaining") != "0" || first.Header().Get("X-RateLimit-Mode") != "FIXED" {
		t.Fatalf("rate-limit headers=%v", first.Header())
	}
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
		t.Fatalf("429 headers/status=%d %v", second.Code, second.Header())
	}
}

func TestAdaptiveTelemetryAndAdministratorOverrideAudit(t *testing.T) {
	manager, _ := NewManager("")
	route := adaptiveTestRoute()
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	manager.rateLimitDecision(route, "observer", time.Now())
	telemetry := manager.AdaptiveTelemetry()
	routes := telemetry["routes"].([]map[string]any)
	if len(routes) != 1 || routes[0]["mode"] != "ADAPTIVE" || routes[0]["effectiveLimit"] != 100 {
		t.Fatalf("adaptive telemetry=%#v", telemetry)
	}
	route.ManualOverrideLimit, route.ManualOverrideExpiresAt = 40, time.Now().Add(time.Minute).UnixMilli()
	if err := manager.Apply(testSnapshot("2", route)); err != nil {
		t.Fatal(err)
	}
	route.ManualOverrideLimit, route.ManualOverrideExpiresAt = 0, 0
	if err := manager.Apply(testSnapshot("3", route)); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range manager.AdaptiveHistory() {
		seen[event.Type] = true
	}
	if !seen["administrator_override_applied"] || !seen["administrator_override_cleared"] {
		t.Fatalf("override history=%+v", manager.AdaptiveHistory())
	}
}
