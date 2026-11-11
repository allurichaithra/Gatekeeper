package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	circuitClosed    = "CLOSED"
	circuitOpen      = "OPEN"
	circuitHalfOpen  = "HALF-OPEN"
	maxCircuitEvents = 500
)

// CircuitEvent is a bounded, non-sensitive state transition record.
type CircuitEvent struct {
	At       time.Time `json:"at"`
	RouteID  string    `json:"routeId"`
	Upstream string    `json:"upstream"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Failures int       `json:"failures"`
	Reason   string    `json:"reason"`
}

type upstreamRuntime struct {
	state          string
	failures       int
	openedAt       time.Time
	probesInFlight int
	active         int
	avgLatencyNs   uint64
	requests       uint64
	failureCount   uint64
	lastTransition time.Time
}

func upstreamKey(routeID, target string) string { return routeID + "|" + target }

// requestGroups returns the configured canary group first, then deterministic
// fallback groups. A failed group therefore never forces traffic to an
// unhealthy/open-circuit target, while a healthy peer group can serve it.
func (m *Manager) requestGroups(route Route) []Route {
	groups := route.UpstreamGroups
	if len(groups) == 0 {
		stable := append([]Target(nil), route.Targets...)
		for i := range stable {
			if stable[i].GroupID == "" {
				stable[i].GroupID = "stable"
			}
		}
		copyRoute := route
		copyRoute.Targets = stable
		return []Route{copyRoute}
	}
	active := make([]TargetGroup, 0, len(groups))
	for _, group := range groups {
		if group.Percentage > 0 {
			active = append(active, group)
		}
	}
	if len(active) == 0 {
		return nil
	}
	value, _ := m.groupSequence.LoadOrStore(route.ID, &atomic.Uint64{})
	point := value.(*atomic.Uint64).Add(1) % 100
	selected := 0
	for index, group := range active {
		if point < uint64(group.Percentage) {
			selected = index
			break
		}
		point -= uint64(group.Percentage)
	}
	ordered := make([]Route, 0, len(active))
	for offset := 0; offset < len(active); offset++ {
		group := active[(selected+offset)%len(active)]
		copyRoute := route
		copyRoute.Targets = append([]Target(nil), group.Targets...)
		for index := range copyRoute.Targets {
			copyRoute.Targets[index].GroupID = group.ID
		}
		ordered = append(ordered, copyRoute)
	}
	return ordered
}

func (m *Manager) upstreamLocked(routeID, target string) *upstreamRuntime {
	key := upstreamKey(routeID, target)
	upstream := m.state.upstreams[key]
	if upstream == nil {
		upstream = &upstreamRuntime{state: circuitClosed}
		m.state.upstreams[key] = upstream
	}
	return upstream
}

func (m *Manager) transitionLocked(route Route, target, next, reason string, current *upstreamRuntime) {
	if current.state == next {
		return
	}
	previous := current.state
	current.state = next
	current.lastTransition = time.Now().UTC()
	if next == circuitOpen {
		current.openedAt = time.Now()
	}
	if next == circuitClosed {
		current.failures = 0
		current.probesInFlight = 0
	}
	m.state.circuitEvents = append(m.state.circuitEvents, CircuitEvent{At: current.lastTransition, RouteID: route.ID, Upstream: target, From: previous, To: next, Failures: current.failures, Reason: reason})
	if len(m.state.circuitEvents) > maxCircuitEvents {
		m.state.circuitEvents = append([]CircuitEvent(nil), m.state.circuitEvents[len(m.state.circuitEvents)-maxCircuitEvents:]...)
	}
}

// availableTarget reserves a target connection and a limited half-open probe.
func (m *Manager) availableTarget(route Route, now time.Time) (Target, bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	targets := make([]Target, 0, len(route.Targets))
	for _, target := range route.Targets {
		key := upstreamKey(route.ID, target.URL)
		healthy, known := m.state.probes[key]
		if known && !healthy {
			continue
		}
		runtime := m.upstreamLocked(route.ID, target.URL)
		if route.CircuitBreakerEnabled {
			if runtime.state == circuitOpen {
				if now.Sub(runtime.openedAt) < time.Duration(route.CircuitRecoveryTimeoutMs)*time.Millisecond {
					continue
				}
				m.transitionLocked(route, target.URL, circuitHalfOpen, "recovery timeout elapsed", runtime)
			}
			if runtime.state == circuitHalfOpen {
				if runtime.probesInFlight >= route.CircuitHalfOpenProbes {
					continue
				}
			}
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		m.state.circuitOpenRejects.Add(1)
		return Target{}, false
	}
	selected := m.chooseTargetLocked(route, targets)
	runtime := m.upstreamLocked(route.ID, selected.URL)
	runtime.active++
	if route.CircuitBreakerEnabled && runtime.state == circuitHalfOpen {
		runtime.probesInFlight++
	}
	return selected, true
}

func (m *Manager) chooseTargetLocked(route Route, targets []Target) Target {
	algorithm := strings.ToUpper(route.LoadBalancingAlgorithm)
	if algorithm == "" || algorithm == "WEIGHTED_ROUND_ROBIN" {
		var total uint64
		for _, target := range targets {
			total += uint64(target.Weight)
		}
		value, _ := m.sequence.LoadOrStore(route.ID, &atomic.Uint64{})
		selected := value.(*atomic.Uint64).Add(1) % total
		for _, target := range targets {
			if selected < uint64(target.Weight) {
				return target
			}
			selected -= uint64(target.Weight)
		}
		return targets[0]
	}
	if algorithm == "ROUND_ROBIN" {
		value, _ := m.sequence.LoadOrStore(route.ID, &atomic.Uint64{})
		return targets[value.(*atomic.Uint64).Add(1)%uint64(len(targets))]
	}
	if algorithm == "LEAST_CONNECTIONS" {
		best := targets[0]
		for _, candidate := range targets[1:] {
			if m.upstreamLocked(route.ID, candidate.URL).active < m.upstreamLocked(route.ID, best.URL).active {
				best = candidate
			}
		}
		return best
	}
	// LEAST_RESPONSE_TIME uses an EWMA of completed upstream response times.
	ordered := append([]Target(nil), targets...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := m.upstreamLocked(route.ID, ordered[i].URL), m.upstreamLocked(route.ID, ordered[j].URL)
		if a.requests == 0 || b.requests == 0 {
			return a.requests == 0 && b.requests != 0
		}
		if a.avgLatencyNs == b.avgLatencyNs {
			return i < j
		}
		return a.avgLatencyNs < b.avgLatencyNs
	})
	return ordered[0]
}

func (m *Manager) finishUpstream(route Route, target Target, started time.Time, status int, requestErr error) {
	m.finishUpstreamResult(route, target, started, status, requestErr, true)
}

func (m *Manager) finishUpstreamResponse(route Route, target Target, started time.Time, status int, body io.ReadCloser) io.ReadCloser {
	m.finishUpstreamResult(route, target, started, status, nil, false)
	if body == nil {
		m.releaseUpstream(route.ID, target.URL)
		return http.NoBody
	}
	return &trackedUpstreamBody{ReadCloser: body, release: func() { m.releaseUpstream(route.ID, target.URL) }}
}

func (m *Manager) releaseUpstream(routeID, target string) {
	m.state.mu.Lock()
	if runtime := m.state.upstreams[upstreamKey(routeID, target)]; runtime != nil && runtime.active > 0 {
		runtime.active--
	}
	m.state.mu.Unlock()
}

type trackedUpstreamBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *trackedUpstreamBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}
func (b *trackedUpstreamBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func (m *Manager) finishUpstreamResult(route Route, target Target, started time.Time, status int, requestErr error, releaseActive bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	runtime := m.upstreamLocked(route.ID, target.URL)
	if releaseActive && runtime.active > 0 {
		runtime.active--
	}
	if runtime.state == circuitHalfOpen && runtime.probesInFlight > 0 {
		runtime.probesInFlight--
	}
	runtime.requests++
	latency := uint64(time.Since(started).Nanoseconds())
	if runtime.avgLatencyNs == 0 {
		runtime.avgLatencyNs = latency
	} else {
		runtime.avgLatencyNs = (runtime.avgLatencyNs*7 + latency) / 8
	}
	if errors.Is(requestErr, context.Canceled) {
		return
	}
	failure := requestErr != nil || status >= 500
	if !failure {
		runtime.failures = 0
		if route.CircuitBreakerEnabled && runtime.state == circuitHalfOpen {
			m.transitionLocked(route, target.URL, circuitClosed, "recovery probe succeeded", runtime)
		}
		return
	}
	runtime.failureCount++
	if requestErr == nil && status < 500 {
		return
	}
	runtime.failures++
	if !route.CircuitBreakerEnabled {
		return
	}
	if runtime.state == circuitHalfOpen || runtime.failures >= route.CircuitFailureThreshold {
		reason := "upstream failure threshold reached"
		if runtime.state == circuitHalfOpen {
			reason = "half-open recovery probe failed"
		}
		m.transitionLocked(route, target.URL, circuitOpen, reason, runtime)
	}
}

func retryableUpstreamStatus(route Route, status int) bool {
	for _, candidate := range route.RetryStatuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func shouldRetry(route Route, request *http.Request, status int, err error) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions && request.Method != http.MethodPut && request.Method != http.MethodDelete && request.Method != http.MethodTrace {
		if !route.RetryNonIdempotent || request.Header.Get("Idempotency-Key") == "" {
			return false
		}
	}
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
			return route.RetryTimeouts
		}
		return route.RetryConnectionErrors
	}
	return retryableUpstreamStatus(route, status)
}

func retryDelay(route Route, attempt int) time.Duration {
	base := time.Duration(route.RetryBackoffMs) * time.Millisecond
	if base <= 0 {
		return 0
	}
	shift := attempt - 1
	if shift > 20 {
		shift = 20
	}
	delay := base * time.Duration(1<<shift)
	maximum := time.Duration(route.RetryMaxBackoffMs) * time.Millisecond
	if maximum > 0 && delay > maximum {
		delay = maximum
	}
	if route.RetryJitter && delay > 1 {
		delay = time.Duration(rand.Int63n(int64(delay)))
	}
	return delay
}

type retryRoundTripper struct {
	manager        *Manager
	route          Route
	fallbackRoutes []Route
	maxRetries     int
	logger         *slog.Logger
	trace          *requestTrace
}

type requestTrace struct {
	retries            atomic.Int32
	upstream           string
	groupID            string
	circuitDecision    string
	upstreamDurationMs float64
}

func (t retryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	var lastErr error
	var pendingResponse *http.Response
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if err := request.Context().Err(); err != nil {
			if pendingResponse != nil {
				_ = pendingResponse.Body.Close()
			}
			return nil, err
		}
		if attempt > 0 {
			if t.trace != nil {
				t.trace.retries.Add(1)
			}
			wait := retryDelay(t.route, attempt)
			timer := time.NewTimer(wait)
			select {
			case <-request.Context().Done():
				timer.Stop()
				if pendingResponse != nil {
					_ = pendingResponse.Body.Close()
				}
				return nil, request.Context().Err()
			case <-timer.C:
			}
		}
		selectedRoute := t.route
		target, ok := t.manager.availableTarget(selectedRoute, time.Now())
		if !ok {
			for _, fallback := range t.fallbackRoutes {
				target, ok = t.manager.availableTarget(fallback, time.Now())
				if ok {
					selectedRoute = fallback
					break
				}
			}
		}
		if !ok {
			if pendingResponse != nil {
				t.manager.state.retryExhausted.Add(1)
				return pendingResponse, nil
			}
			if t.trace != nil {
				t.trace.circuitDecision = "NO_HEALTHY_UPSTREAM"
			}
			return nil, fmt.Errorf("no healthy upstream available")
		}
		if t.trace != nil {
			t.trace.upstream = target.URL
			t.trace.groupID = target.GroupID
		}
		if pendingResponse != nil {
			_ = pendingResponse.Body.Close()
			pendingResponse = nil
		}
		parsed, err := url.Parse(target.URL)
		if err != nil {
			t.manager.finishUpstream(selectedRoute, target, time.Now(), 0, err)
			lastErr = err
			continue
		}
		out := request.Clone(request.Context())
		out.URL = cloneURL(request.URL)
		if request.GetBody != nil {
			body, bodyErr := request.GetBody()
			if bodyErr != nil {
				t.manager.finishUpstream(selectedRoute, target, time.Now(), 0, bodyErr)
				return nil, bodyErr
			}
			out.Body = body
		}
		out.URL.Scheme, out.URL.Host = parsed.Scheme, parsed.Host
		out.URL.Path = joinURLPath(parsed.Path, request.URL.Path)
		out.Host = parsed.Host
		started := time.Now()
		response, roundErr := defaultTransport.RoundTrip(out)
		elapsed := time.Since(started)
		if t.trace != nil {
			t.trace.upstreamDurationMs += float64(elapsed.Microseconds()) / 1000
		}
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		if roundErr != nil {
			t.manager.finishUpstream(selectedRoute, target, started, status, roundErr)
		} else {
			response.Body = t.manager.finishUpstreamResponse(selectedRoute, target, started, status, response.Body)
		}
		if t.trace != nil {
			t.trace.circuitDecision = t.manager.circuitState(selectedRoute.ID, target.URL)
		}
		if roundErr != nil {
			t.manager.markTarget(selectedRoute.ID, target.URL, false)
			lastErr = roundErr
			if errors.Is(roundErr, context.Canceled) || request.Context().Err() == context.Canceled {
				return nil, roundErr
			}
			if !shouldRetry(t.route, request, 0, roundErr) || attempt == t.maxRetries {
				if attempt > 0 {
					t.manager.state.retryExhausted.Add(1)
				}
				return nil, roundErr
			}
			t.manager.state.retries.Add(1)
			if t.logger != nil {
				t.logger.Warn("retrying upstream request", "route_id", t.route.ID, "attempt", attempt+1, "failure", "connection_or_timeout")
			}
			continue
		}
		if attempt < t.maxRetries && shouldRetry(t.route, request, status, nil) {
			t.manager.state.retries.Add(1)
			if t.logger != nil {
				t.logger.Warn("retrying upstream request", "route_id", t.route.ID, "attempt", attempt+1, "upstream_status", status)
			}
			pendingResponse = response
			continue
		}
		if attempt > 0 {
			if status >= 200 && status < 500 {
				t.manager.state.retrySuccesses.Add(1)
			} else {
				t.manager.state.retryExhausted.Add(1)
			}
		}
		response.Header.Set("X-Gateway-Upstream", target.URL)
		response.Header.Set("X-Gateway-Group", target.GroupID)
		response.Header.Set("X-Gateway-Retries", strconv.Itoa(attempt))
		response.Header.Set("X-Gateway-Upstream-Duration-Ms", strconv.FormatFloat(float64(time.Since(started).Microseconds())/1000, 'f', 3, 64))
		return response, nil
	}
	return nil, lastErr
}

func retryMethodAllowed(route Route, request *http.Request) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodOptions || request.Method == http.MethodPut || request.Method == http.MethodDelete || request.Method == http.MethodTrace {
		return true
	}
	return route.RetryNonIdempotent && request.Header.Get("Idempotency-Key") != ""
}

func cloneURL(in *url.URL) *url.URL { cloned := *in; return &cloned }
