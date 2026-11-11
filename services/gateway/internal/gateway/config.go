package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is a complete, versioned gateway configuration document.
type Snapshot struct {
	Version string  `json:"version"`
	Routes  []Route `json:"routes"`
}

// Route describes matching, rewriting, timeout, and target behavior.
type Route struct {
	ID                       string        `json:"id"`
	Path                     string        `json:"path"`
	MatchMode                string        `json:"matchMode"`
	Methods                  []string      `json:"methods"`
	StripPrefix              bool          `json:"stripPrefix"`
	RewritePrefix            string        `json:"rewritePrefix,omitempty"`
	TimeoutMs                int           `json:"timeoutMs"`
	Enabled                  bool          `json:"enabled"`
	ServiceID                string        `json:"serviceId"`
	Targets                  []Target      `json:"targets"`
	UpstreamGroups           []TargetGroup `json:"upstreamGroups,omitempty"`
	RateLimit                int           `json:"rateLimit,omitempty"` // requests per minute per client IP; zero disables
	RateLimitMode            string        `json:"rateLimitMode,omitempty"`
	AdaptiveEnabled          bool          `json:"adaptiveEnabled,omitempty"`
	MinimumRateLimit         int           `json:"minimumRateLimit,omitempty"`
	MaximumRateLimit         int           `json:"maximumRateLimit,omitempty"`
	SpikeSensitivity         float64       `json:"spikeSensitivity,omitempty"`
	RecoveryCooldownMs       int           `json:"recoveryCooldownMs,omitempty"`
	ManualOverrideLimit      int           `json:"manualOverrideLimit,omitempty"`
	ManualOverrideExpiresAt  int64         `json:"manualOverrideExpiresAt,omitempty"`
	CacheTtlMs               int           `json:"cacheTtlMs,omitempty"`
	LoadBalancingAlgorithm   string        `json:"loadBalancingAlgorithm,omitempty"`
	CircuitBreakerEnabled    bool          `json:"circuitBreakerEnabled,omitempty"`
	CircuitFailureThreshold  int           `json:"circuitFailureThreshold,omitempty"`
	CircuitRecoveryTimeoutMs int           `json:"circuitRecoveryTimeoutMs,omitempty"`
	CircuitHalfOpenProbes    int           `json:"circuitHalfOpenProbes,omitempty"`
	MaxRetries               int           `json:"maxRetries,omitempty"`
	RetryBackoffMs           int           `json:"retryBackoffMs,omitempty"`
	RetryMaxBackoffMs        int           `json:"retryMaxBackoffMs,omitempty"`
	RetryJitter              bool          `json:"retryJitter,omitempty"`
	RetryStatuses            []int         `json:"retryStatuses,omitempty"`
	RetryConnectionErrors    bool          `json:"retryConnectionErrors,omitempty"`
	RetryTimeouts            bool          `json:"retryTimeouts,omitempty"`
	RetryNonIdempotent       bool          `json:"retryNonIdempotent,omitempty"`
}

// Target is a backend instance for a service.
type Target struct {
	URL     string `json:"url"`
	Weight  int    `json:"weight"`
	GroupID string `json:"groupId,omitempty"`
}

type TargetGroup struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Percentage int      `json:"percentage"`
	Targets    []Target `json:"targets"`
}

// Manager validates, persists, and atomically activates gateway snapshots.
type Manager struct {
	current       atomic.Pointer[Snapshot]
	configFile    string
	backupFile    string
	sequence      sync.Map
	groupSequence sync.Map
	applyMu       sync.Mutex
	state         *runtimeState
}

type runtimeState struct {
	mu                   sync.Mutex
	clients              map[string][]time.Time
	cache                map[string]cacheEntry
	requests             atomic.Uint64
	completed            atomic.Uint64
	errors               atomic.Uint64
	clientErrors         atomic.Uint64
	expectedClientErrors atomic.Uint64
	cacheHits            atomic.Uint64
	cacheMisses          atomic.Uint64
	latencyNanos         atomic.Uint64
	statusCodes          map[int]uint64
	buckets              [60]metricBucket
	started              time.Time
	probes               map[string]bool
	adaptive             map[string]*adaptiveRouteState
	decisions            []AdaptiveDecision
	throttled            map[string]uint64
	decisionCount        uint64
	clientHitCount       int
	upstreams            map[string]*upstreamRuntime
	retries              atomic.Uint64
	retrySuccesses       atomic.Uint64
	retryExhausted       atomic.Uint64
	circuitOpenRejects   atomic.Uint64
	circuitEvents        []CircuitEvent
	requestsHistory      []RequestRecord
}

// RequestRecord contains bounded, sanitized request metadata. It never stores headers or bodies.
type RequestRecord struct {
	Timestamp          time.Time `json:"timestamp"`
	RequestID          string    `json:"requestId"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	RouteID            string    `json:"routeId,omitempty"`
	Upstream           string    `json:"upstream,omitempty"`
	GroupID            string    `json:"groupId,omitempty"`
	Status             int       `json:"status"`
	DurationMs         float64   `json:"durationMs"`
	Cache              string    `json:"cache"`
	RetryCount         int       `json:"retryCount"`
	CircuitDecision    string    `json:"circuitDecision"`
	RateLimitDecision  string    `json:"rateLimitDecision"`
	UpstreamDurationMs float64   `json:"upstreamDurationMs"`
}

const maxRequestRecords = 1000

type metricBucket struct {
	second       int64
	requests     uint64
	completed    uint64
	latencyNanos uint64
	errors       uint64
	cacheHits    uint64
	cacheMisses  uint64
}

type metricPoint struct {
	Timestamp         int64   `json:"timestamp"`
	Requests          uint64  `json:"requests"`
	AverageResponseMs float64 `json:"averageResponseMs"`
	Errors            uint64  `json:"errors"`
	CacheHits         uint64  `json:"cacheHits"`
	CacheMisses       uint64  `json:"cacheMisses"`
}
type cacheEntry struct {
	body    []byte
	header  http.Header
	status  int
	expires time.Time
}

type combinedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *combinedReadCloser) Close() error { return r.closer.Close() }

func NewManager(configFile string) (*Manager, error) {
	manager := &Manager{configFile: configFile, backupFile: configFile + ".last-good", state: &runtimeState{clients: map[string][]time.Time{}, cache: map[string]cacheEntry{}, probes: map[string]bool{}, statusCodes: map[int]uint64{}, adaptive: map[string]*adaptiveRouteState{}, throttled: map[string]uint64{}, upstreams: map[string]*upstreamRuntime{}, started: time.Now()}}
	empty := Snapshot{Version: "0", Routes: []Route{}}
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read last-known-good config: %w", err)
		}
		if err == nil {
			var primary Snapshot
			if json.Unmarshal(data, &primary) == nil && ValidateSnapshot(primary) == nil {
				empty = primary
			} else {
				data, backupErr := os.ReadFile(manager.backupFile)
				if backupErr == nil {
					var backup Snapshot
					if json.Unmarshal(data, &backup) == nil && ValidateSnapshot(backup) == nil {
						empty = backup
					}
				}
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			if data, backupErr := os.ReadFile(manager.backupFile); backupErr == nil {
				var backup Snapshot
				if json.Unmarshal(data, &backup) == nil && ValidateSnapshot(backup) == nil {
					empty = backup
				}
			}
		}
	}
	if err := ValidateSnapshot(empty); err != nil {
		return nil, fmt.Errorf("invalid last-known-good config: %w", err)
	}
	manager.current.Store(cloneSnapshot(empty))
	if configFile != "" {
		if err := persistSnapshot(configFile, cloneSnapshot(empty)); err != nil {
			return nil, fmt.Errorf("restore active config: %w", err)
		}
		if err := persistSnapshot(manager.backupFile, cloneSnapshot(empty)); err != nil {
			return nil, fmt.Errorf("persist backup config: %w", err)
		}
	}
	return manager, nil
}

func (m *Manager) RestoreCurrent() error {
	if m.configFile == "" {
		return nil
	}
	return persistSnapshot(m.configFile, m.current.Load())
}

func (m *Manager) Current() Snapshot { return *cloneSnapshot(*m.current.Load()) }

func (m *Manager) Metrics() map[string]any {
	requests := m.state.requests.Load()
	completed := m.state.completed.Load()
	failures := m.state.errors.Load()
	clientErrors := m.state.clientErrors.Load()
	expectedClientErrors := m.state.expectedClientErrors.Load()
	cacheHits, cacheMisses := m.state.cacheHits.Load(), m.state.cacheMisses.Load()
	averageResponseMs, errorRate, clientErrorRate, cacheHitRate := float64(0), float64(0), float64(0), float64(0)
	if completed > 0 {
		averageResponseMs = float64(m.state.latencyNanos.Load()) / float64(completed) / float64(time.Millisecond)
	}
	if requests > 0 {
		errorRate = float64(failures) / float64(requests)
		clientErrorRate = float64(clientErrors) / float64(requests)
	}
	if cacheHits+cacheMisses > 0 {
		cacheHitRate = float64(cacheHits) / float64(cacheHits+cacheMisses)
	}

	now := time.Now()
	nowSecond := now.Unix()
	m.state.mu.Lock()
	statusCodes := make(map[string]uint64, len(m.state.statusCodes))
	for status, count := range m.state.statusCodes {
		statusCodes[strconv.Itoa(status)] = count
	}
	points := make([]metricPoint, 0, 60)
	latencies := make([]float64, 0, len(m.state.requestsHistory))
	routeTraffic := make(map[string]uint64)
	routeErrors := make(map[string]uint64)
	for _, request := range m.state.requestsHistory {
		latencies = append(latencies, request.DurationMs)
		if request.RouteID != "" {
			routeTraffic[request.RouteID]++
			if request.Status >= 500 {
				routeErrors[request.RouteID]++
			}
		}
	}
	sort.Float64s(latencies)
	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		index := int(math.Ceil(p*float64(len(latencies)))) - 1
		if index < 0 {
			index = 0
		}
		return latencies[index]
	}
	var recentRequests uint64
	for second := nowSecond - 59; second <= nowSecond; second++ {
		bucket := m.state.buckets[second%int64(len(m.state.buckets))]
		point := metricPoint{Timestamp: second}
		if bucket.second == second {
			point.Requests = bucket.requests
			point.Errors = bucket.errors
			point.CacheHits = bucket.cacheHits
			point.CacheMisses = bucket.cacheMisses
			if bucket.completed > 0 {
				point.AverageResponseMs = float64(bucket.latencyNanos) / float64(bucket.completed) / float64(time.Millisecond)
			}
		}
		points = append(points, point)
		if nowSecond-second < 10 && nowSecond-second >= 0 {
			recentRequests += point.Requests
		}
	}
	m.state.mu.Unlock()
	windowSeconds := time.Since(m.state.started).Seconds()
	if windowSeconds > 10 {
		windowSeconds = 10
	}
	if windowSeconds < 0.001 {
		windowSeconds = 0.001
	}
	rps := float64(recentRequests) / windowSeconds
	return map[string]any{"totalRequests": requests, "requestsPerSecond": rps, "averageResponseMs": averageResponseMs, "p50ResponseMs": percentile(.50), "p95ResponseMs": percentile(.95), "p99ResponseMs": percentile(.99), "routeTraffic": routeTraffic, "routeErrors": routeErrors, "errors": failures, "errorRate": errorRate, "clientErrors": clientErrors, "expectedClientErrors": expectedClientErrors, "clientErrorRate": clientErrorRate, "statusCodes": statusCodes, "cacheHits": cacheHits, "cacheMisses": cacheMisses, "cacheHitRate": cacheHitRate, "timeseries": points, "uptimeSeconds": int64(time.Since(m.state.started).Seconds()), "configVersion": m.Current().Version, "retryAttempts": m.state.retries.Load(), "retrySuccesses": m.state.retrySuccesses.Load(), "retryExhausted": m.state.retryExhausted.Load(), "circuitOpenRejections": m.state.circuitOpenRejects.Load(), "upstreams": m.UpstreamTelemetry()}
}

func (m *Manager) RequestHistory(filters map[string]string, limit int) []RequestRecord {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	result := make([]RequestRecord, 0, limit)
	for i := len(m.state.requestsHistory) - 1; i >= 0 && len(result) < limit; i-- {
		item := m.state.requestsHistory[i]
		if value := filters["requestId"]; value != "" && !strings.Contains(item.RequestID, value) {
			continue
		}
		if value := filters["route"]; value != "" && item.RouteID != value {
			continue
		}
		if value := filters["upstream"]; value != "" && !strings.Contains(item.Upstream, value) {
			continue
		}
		if value := filters["status"]; value != "" && strconv.Itoa(item.Status) != value {
			continue
		}
		result = append(result, item)
	}
	return result
}

func (m *Manager) RequestByID(id string) (RequestRecord, bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	for i := len(m.state.requestsHistory) - 1; i >= 0; i-- {
		if m.state.requestsHistory[i].RequestID == id {
			return m.state.requestsHistory[i], true
		}
	}
	return RequestRecord{}, false
}

func (m *Manager) circuitState(routeID, target string) string {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if runtime := m.state.upstreams[upstreamKey(routeID, target)]; runtime != nil {
		return runtime.state
	}
	return circuitClosed
}

func (m *Manager) UpstreamTelemetry() map[string]any {
	snapshot := m.Current()
	items := make([]map[string]any, 0)
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	for _, route := range snapshot.Routes {
		for _, target := range route.Targets {
			runtime := m.upstreamLocked(route.ID, target.URL)
			var lastTransition any
			if !runtime.lastTransition.IsZero() {
				lastTransition = runtime.lastTransition
			}
			failureRate := float64(0)
			if runtime.requests > 0 {
				failureRate = float64(runtime.failureCount) / float64(runtime.requests)
			}
			items = append(items, map[string]any{"routeId": route.ID, "serviceId": route.ServiceID, "upstream": target.URL, "algorithm": normalizedAlgorithm(route.LoadBalancingAlgorithm), "circuitState": runtime.state, "requests": runtime.requests, "failures": runtime.failureCount, "failureRate": failureRate, "consecutiveFailures": runtime.failures, "activeConnections": runtime.active, "averageResponseMs": float64(runtime.avgLatencyNs) / float64(time.Millisecond), "lastTransition": lastTransition})
		}
	}
	events := append([]CircuitEvent(nil), m.state.circuitEvents...)
	return map[string]any{"upstreams": items, "circuitEvents": events}
}

func normalizedAlgorithm(value string) string {
	if value == "" {
		return "WEIGHTED_ROUND_ROBIN"
	}
	return strings.ToUpper(value)
}

func (m *Manager) recordRequestStart(started time.Time) {
	m.state.requests.Add(1)
	second := started.Unix()
	m.state.mu.Lock()
	bucket := &m.state.buckets[second%int64(len(m.state.buckets))]
	if bucket.second != second {
		*bucket = metricBucket{second: second}
	}
	bucket.requests++
	m.state.mu.Unlock()
}

func (m *Manager) recordRequestComplete(started time.Time, status int, duration time.Duration, cacheLookup int) {
	if duration < 0 {
		duration = 0
	}
	nanos := uint64(duration.Nanoseconds())
	m.state.latencyNanos.Add(nanos)
	m.state.completed.Add(1)
	if status >= 500 {
		m.state.errors.Add(1)
	}
	if status >= 400 && status < 500 {
		m.state.clientErrors.Add(1)
		if status == 400 || status == 404 || status == 405 || status == 429 {
			m.state.expectedClientErrors.Add(1)
		}
	}
	if cacheLookup == 1 {
		m.state.cacheHits.Add(1)
	} else if cacheLookup == 2 {
		m.state.cacheMisses.Add(1)
	}
	second := started.Unix()
	m.state.mu.Lock()
	m.state.statusCodes[status]++
	bucket := &m.state.buckets[second%int64(len(m.state.buckets))]
	if bucket.second != second {
		*bucket = metricBucket{second: second}
	}
	bucket.completed++
	bucket.latencyNanos += nanos
	if status >= 500 {
		bucket.errors++
	}
	if cacheLookup == 1 {
		bucket.cacheHits++
	} else if cacheLookup == 2 {
		bucket.cacheMisses++
	}
	m.state.mu.Unlock()
}
func (m *Manager) Health() map[string]bool {
	result := map[string]bool{}
	client := http.Client{Timeout: time.Second}
	for _, r := range m.current.Load().Routes {
		for i, t := range r.Targets {
			key := r.ID + ":" + strconv.Itoa(i)
			u, err := url.Parse(strings.TrimRight(t.URL, "/") + "/health")
			if err != nil {
				result[key] = false
				m.markTarget(r.ID, t.URL, false)
				continue
			}
			req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
			response, err := client.Do(req)
			if err != nil {
				result[key] = false
				m.markTarget(r.ID, t.URL, false)
				continue
			}
			_ = response.Body.Close()
			result[key] = response.StatusCode >= 200 && response.StatusCode < 400
			m.markTarget(r.ID, t.URL, result[key])
		}
	}
	return result
}

func (m *Manager) RunHealthChecks(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			_ = m.Health()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) Apply(next Snapshot) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if err := ValidateSnapshot(next); err != nil {
		return err
	}
	current := m.current.Load()
	currentVersion, _ := strconv.ParseUint(current.Version, 10, 64)
	nextVersion, _ := strconv.ParseUint(next.Version, 10, 64)
	if nextVersion < currentVersion {
		return fmt.Errorf("configuration version %s is older than active version %s", next.Version, current.Version)
	}
	if nextVersion == currentVersion {
		currentJSON, _ := json.Marshal(current)
		nextJSON, _ := json.Marshal(next)
		if string(currentJSON) != string(nextJSON) {
			return fmt.Errorf("configuration version %s was reused with different content", next.Version)
		}
		return nil
	}
	immutable := cloneSnapshot(next)
	if m.configFile != "" {
		if err := persistSnapshot(m.configFile, immutable); err != nil {
			return fmt.Errorf("persist configuration version %s: %w", next.Version, err)
		}
		if err := persistSnapshot(m.backupFile, immutable); err != nil {
			return fmt.Errorf("persist backup configuration version %s: %w", next.Version, err)
		}
	}
	m.current.Store(immutable)
	m.pruneRuntime(immutable, current)
	return nil
}

func (m *Manager) pruneRuntime(snapshot, previous *Snapshot) {
	validRoutes := make(map[string]struct{}, len(snapshot.Routes))
	validTargets := make(map[string]struct{})
	for _, route := range snapshot.Routes {
		validRoutes[route.ID] = struct{}{}
		for _, target := range route.Targets {
			validTargets[route.ID+"|"+target.URL] = struct{}{}
		}
	}
	m.sequence.Range(func(key, value any) bool {
		if _, ok := validRoutes[key.(string)]; !ok {
			m.sequence.Delete(key)
		}
		return true
	})
	m.groupSequence.Range(func(key, value any) bool {
		if _, ok := validRoutes[key.(string)]; !ok {
			m.groupSequence.Delete(key)
		}
		return true
	})
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	previousRoutes := make(map[string]Route, len(previous.Routes))
	for _, route := range previous.Routes {
		previousRoutes[route.ID] = route
	}
	m.state.cache = make(map[string]cacheEntry)
	for key := range m.state.clients {
		valid := false
		for routeID := range validRoutes {
			if strings.HasPrefix(key, routeID+"|") {
				valid = true
				break
			}
		}
		if !valid {
			m.state.clientHitCount -= len(m.state.clients[key])
			delete(m.state.clients, key)
		}
	}
	for key := range m.state.probes {
		if _, ok := validTargets[key]; !ok {
			delete(m.state.probes, key)
		}
	}
	for key := range m.state.upstreams {
		if _, ok := validTargets[key]; !ok {
			delete(m.state.upstreams, key)
		}
	}
	for key := range m.state.adaptive {
		if _, ok := validRoutes[key]; !ok {
			delete(m.state.adaptive, key)
			delete(m.state.throttled, key)
		}
	}
	for _, route := range snapshot.Routes {
		if old, exists := previousRoutes[route.ID]; exists {
			if adaptiveConfigChanged(old, route) {
				delete(m.state.adaptive, route.ID)
			}
			if reliabilityConfigChanged(old, route) {
				for key := range m.state.upstreams {
					if strings.HasPrefix(key, route.ID+"|") {
						delete(m.state.upstreams, key)
					}
				}
			}
		}
		state := m.state.adaptive[route.ID]
		oldLimit, oldExpiry := 0, int64(0)
		if old, exists := previousRoutes[route.ID]; exists {
			oldLimit, oldExpiry = old.ManualOverrideLimit, old.ManualOverrideExpiresAt
		}
		if state != nil {
			oldLimit, oldExpiry = state.overrideLimit, state.overrideExpiry
		}
		if route.ManualOverrideLimit != oldLimit || route.ManualOverrideExpiresAt != oldExpiry {
			if state == nil {
				state = &adaptiveRouteState{effective: route.RateLimit}
				m.state.adaptive[route.ID] = state
			}
			if route.ManualOverrideLimit > 0 {
				m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "administrator_override_applied", At: time.Now().UTC(), Explanation: fmt.Sprintf("Administrator set a temporary effective limit of %d requests/minute.", route.ManualOverrideLimit), EffectiveLimit: route.ManualOverrideLimit})
			} else if oldLimit > 0 {
				m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "administrator_override_cleared", At: time.Now().UTC(), Explanation: "Administrator returned the route to automatic rate-limit control.", EffectiveLimit: state.effective})
			}
		}
		if state != nil {
			state.overrideLimit, state.overrideExpiry = route.ManualOverrideLimit, route.ManualOverrideExpiresAt
		}
	}
}

func adaptiveConfigChanged(a, b Route) bool {
	return a.RateLimit != b.RateLimit || a.RateLimitMode != b.RateLimitMode || a.AdaptiveEnabled != b.AdaptiveEnabled || a.MinimumRateLimit != b.MinimumRateLimit || a.MaximumRateLimit != b.MaximumRateLimit || a.SpikeSensitivity != b.SpikeSensitivity || a.RecoveryCooldownMs != b.RecoveryCooldownMs
}

func reliabilityConfigChanged(a, b Route) bool {
	return a.LoadBalancingAlgorithm != b.LoadBalancingAlgorithm || a.CircuitBreakerEnabled != b.CircuitBreakerEnabled || a.CircuitFailureThreshold != b.CircuitFailureThreshold || a.CircuitRecoveryTimeoutMs != b.CircuitRecoveryTimeoutMs || a.CircuitHalfOpenProbes != b.CircuitHalfOpenProbes || a.MaxRetries != b.MaxRetries || a.RetryBackoffMs != b.RetryBackoffMs || a.RetryMaxBackoffMs != b.RetryMaxBackoffMs || a.RetryJitter != b.RetryJitter || a.RetryConnectionErrors != b.RetryConnectionErrors || a.RetryTimeouts != b.RetryTimeouts || a.RetryNonIdempotent != b.RetryNonIdempotent || !equalInts(a.RetryStatuses, b.RetryStatuses)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ValidateSnapshot(snapshot Snapshot) error {
	if len(snapshot.Routes) > 1000 {
		return errors.New("snapshot may contain at most 1000 routes")
	}
	version, err := strconv.ParseUint(snapshot.Version, 10, 64)
	if err != nil || version == 0 && snapshot.Version != "0" {
		return errors.New("version must be a non-negative integer string")
	}
	seenIDs := make(map[string]struct{}, len(snapshot.Routes))
	seenRoutes := make(map[string]struct{}, len(snapshot.Routes))
	for i := range snapshot.Routes {
		route := &snapshot.Routes[i]
		if route.ID == "" || route.ServiceID == "" {
			return fmt.Errorf("route %d must have an id and serviceId", i)
		}
		if _, ok := seenIDs[route.ID]; ok {
			return fmt.Errorf("duplicate route id %q", route.ID)
		}
		seenIDs[route.ID] = struct{}{}
		if !strings.HasPrefix(route.Path, "/") || strings.ContainsAny(route.Path, "?#\\") {
			return fmt.Errorf("route %q path must be an absolute path without query, fragment, or backslash", route.ID)
		}
		if route.MatchMode != "PREFIX" && route.MatchMode != "EXACT" {
			return fmt.Errorf("route %q has unsupported matchMode", route.ID)
		}
		routeKey := route.MatchMode + ":" + route.Path
		if _, ok := seenRoutes[routeKey]; ok {
			return fmt.Errorf("duplicate route %q", routeKey)
		}
		seenRoutes[routeKey] = struct{}{}
		if route.TimeoutMs < 1 || route.TimeoutMs > 300000 {
			return fmt.Errorf("route %q timeoutMs must be between 1 and 300000", route.ID)
		}
		if route.RateLimit < 0 || route.RateLimit > 1000000 || route.CacheTtlMs < 0 || route.CacheTtlMs > 3600000 {
			return fmt.Errorf("route %q has invalid rateLimit or cacheTtlMs", route.ID)
		}
		if route.RateLimitMode == "" {
			route.RateLimitMode = "FIXED"
		}
		if route.RateLimitMode != "FIXED" && route.RateLimitMode != "ADAPTIVE" {
			return fmt.Errorf("route %q rateLimitMode must be FIXED or ADAPTIVE", route.ID)
		}
		if route.AdaptiveEnabled && (route.RateLimitMode != "ADAPTIVE" || route.RateLimit < 1) {
			return fmt.Errorf("route %q adaptive mode requires ADAPTIVE mode and a positive base rateLimit", route.ID)
		}
		minimum := route.MinimumRateLimit
		if minimum == 0 && route.RateLimit > 0 {
			minimum = max(1, int(math.Ceil(float64(route.RateLimit)*0.2)))
		}
		maximum := route.MaximumRateLimit
		if maximum == 0 {
			maximum = route.RateLimit
		}
		if minimum < 0 || maximum < 0 || minimum > 1000000 || maximum > 1000000 || route.RateLimit > 0 && (minimum > route.RateLimit || maximum < minimum || maximum < route.RateLimit) {
			return fmt.Errorf("route %q has invalid adaptive minimum or maximum", route.ID)
		}
		if route.SpikeSensitivity != 0 && (route.SpikeSensitivity < 1.1 || route.SpikeSensitivity > 10) {
			return fmt.Errorf("route %q spikeSensitivity must be between 1.1 and 10", route.ID)
		}
		if route.RecoveryCooldownMs != 0 && (route.RecoveryCooldownMs < 1000 || route.RecoveryCooldownMs > 3600000) {
			return fmt.Errorf("route %q recoveryCooldownMs must be between 1000 and 3600000", route.ID)
		}
		if route.LoadBalancingAlgorithm == "" {
			route.LoadBalancingAlgorithm = "WEIGHTED_ROUND_ROBIN"
		}
		route.LoadBalancingAlgorithm = strings.ToUpper(route.LoadBalancingAlgorithm)
		if route.LoadBalancingAlgorithm != "ROUND_ROBIN" && route.LoadBalancingAlgorithm != "WEIGHTED_ROUND_ROBIN" && route.LoadBalancingAlgorithm != "LEAST_CONNECTIONS" && route.LoadBalancingAlgorithm != "LEAST_RESPONSE_TIME" {
			return fmt.Errorf("route %q has unsupported loadBalancingAlgorithm", route.ID)
		}
		if route.CircuitFailureThreshold == 0 {
			route.CircuitFailureThreshold = 5
		}
		if route.CircuitRecoveryTimeoutMs == 0 {
			route.CircuitRecoveryTimeoutMs = 30000
		}
		if route.CircuitHalfOpenProbes == 0 {
			route.CircuitHalfOpenProbes = 1
		}
		if route.CircuitFailureThreshold < 1 || route.CircuitFailureThreshold > 1000 || route.CircuitRecoveryTimeoutMs < 100 || route.CircuitRecoveryTimeoutMs > 3600000 || route.CircuitHalfOpenProbes < 1 || route.CircuitHalfOpenProbes > 100 {
			return fmt.Errorf("route %q has invalid circuit breaker configuration", route.ID)
		}
		if route.MaxRetries < 0 || route.MaxRetries > 5 || route.RetryBackoffMs < 0 || route.RetryBackoffMs > 30000 || route.MaxRetries > 0 && route.RetryBackoffMs < 1 || route.RetryMaxBackoffMs < 0 || route.RetryMaxBackoffMs > 300000 || route.RetryMaxBackoffMs > 0 && route.RetryMaxBackoffMs < route.RetryBackoffMs {
			return fmt.Errorf("route %q has invalid retry configuration", route.ID)
		}
		if route.RetryMaxBackoffMs == 0 {
			route.RetryMaxBackoffMs = 1000
		}
		statusSet := map[int]struct{}{}
		for _, status := range route.RetryStatuses {
			if status < 500 || status > 599 {
				return fmt.Errorf("route %q retry statuses must be server errors between 500 and 599", route.ID)
			}
			if _, exists := statusSet[status]; exists {
				return fmt.Errorf("route %q contains duplicate retry status", route.ID)
			}
			statusSet[status] = struct{}{}
		}
		if route.ManualOverrideLimit < 0 || route.ManualOverrideLimit > 1000000 || route.ManualOverrideLimit > 0 && route.RateLimit > 0 && (route.ManualOverrideLimit < minimum || route.ManualOverrideLimit > maximum) {
			return fmt.Errorf("route %q manual override is outside its adaptive bounds", route.ID)
		}
		if len(route.Methods) == 0 {
			return fmt.Errorf("route %q must allow at least one method", route.ID)
		}
		methodSet := make(map[string]struct{}, len(route.Methods))
		for _, method := range route.Methods {
			if method == "" || strings.ToUpper(method) != method || strings.ContainsAny(method, " \t\r\n") {
				return fmt.Errorf("route %q contains an invalid method", route.ID)
			}
			methodSet[method] = struct{}{}
		}
		if len(route.UpstreamGroups) > 0 {
			if len(route.UpstreamGroups) < 2 || len(route.UpstreamGroups) > 10 {
				return fmt.Errorf("route %q must contain 2 to 10 upstream groups", route.ID)
			}
			seenGroups := map[string]struct{}{}
			percentageTotal, activeGroups := 0, 0
			flattened := make([]Target, 0)
			for _, group := range route.UpstreamGroups {
				if group.ID == "" || len(group.ID) > 32 || strings.ContainsAny(group.ID, " /\\?#") {
					return fmt.Errorf("route %q has an invalid upstream group id", route.ID)
				}
				if _, exists := seenGroups[group.ID]; exists {
					return fmt.Errorf("route %q has duplicate upstream group ids", route.ID)
				}
				seenGroups[group.ID] = struct{}{}
				if strings.TrimSpace(group.Name) == "" || len(group.Name) > 64 || group.Percentage < 0 || group.Percentage > 100 || len(group.Targets) == 0 || len(group.Targets) > 100 {
					return fmt.Errorf("route %q has invalid upstream group settings", route.ID)
				}
				percentageTotal += group.Percentage
				if group.Percentage > 0 {
					activeGroups++
				}
				for _, target := range group.Targets {
					target.GroupID = group.ID
					flattened = append(flattened, target)
				}
			}
			if percentageTotal != 100 || activeGroups == 0 {
				return fmt.Errorf("route %q upstream group percentages must total 100", route.ID)
			}
			if len(flattened) > 100 {
				return fmt.Errorf("route %q may have at most 100 total upstream instances", route.ID)
			}
			route.Targets = flattened
		}
		if len(route.Targets) == 0 {
			return fmt.Errorf("route %q must have at least one target", route.ID)
		}
		if len(route.Targets) > 100 {
			return fmt.Errorf("route %q may have at most 100 targets", route.ID)
		}
		for _, target := range route.Targets {
			parsed, err := url.Parse(target.URL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
				return fmt.Errorf("route %q contains an invalid target URL", route.ID)
			}
			if target.Weight < 1 || target.Weight > 1000 {
				return fmt.Errorf("route %q target weight must be between 1 and 1000", route.ID)
			}
		}
		if route.RewritePrefix != "" && (!strings.HasPrefix(route.RewritePrefix, "/") || strings.ContainsAny(route.RewritePrefix, "?#\\")) {
			return fmt.Errorf("route %q has an invalid rewritePrefix", route.ID)
		}
	}
	return nil
}

func cloneSnapshot(snapshot Snapshot) *Snapshot {
	encoded, _ := json.Marshal(snapshot)
	var cloned Snapshot
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}

func persistSnapshot(path string, snapshot *Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".gateway-config-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func (m *Manager) Handler(globalTimeout time.Duration, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		capture := &statusWriter{ResponseWriter: w}
		w = capture
		cacheLookup := 0
		trace := &requestTrace{}
		record := RequestRecord{Timestamp: started.UTC(), RequestID: r.Header.Get(requestIDHeader), Method: r.Method, Path: r.URL.Path, Cache: "BYPASS", RateLimitDecision: "NOT_CONFIGURED", CircuitDecision: "NOT_USED"}
		m.recordRequestStart(started)
		defer func() {
			status := capture.status
			if status == 0 {
				status = http.StatusOK
			}
			m.recordRequestComplete(started, status, time.Since(started), cacheLookup)
			record.Status = status
			record.RetryCount = int(trace.retries.Load())
			if record.Upstream == "" {
				record.Upstream = trace.upstream
			}
			if record.GroupID == "" {
				record.GroupID = trace.groupID
			}
			if trace.circuitDecision != "" {
				record.CircuitDecision = trace.circuitDecision
			}
			record.UpstreamDurationMs = trace.upstreamDurationMs
			record.DurationMs = float64(time.Since(started).Microseconds()) / 1000
			if cacheLookup == 1 {
				record.Cache = "HIT"
			} else if cacheLookup == 2 {
				record.Cache = "MISS"
			}
			m.state.mu.Lock()
			m.state.requestsHistory = append(m.state.requestsHistory, record)
			if len(m.state.requestsHistory) > maxRequestRecords {
				m.state.requestsHistory = append([]RequestRecord(nil), m.state.requestsHistory[len(m.state.requestsHistory)-maxRequestRecords:]...)
			}
			m.state.mu.Unlock()
		}()
		route, pathMatch := lookup(m.current.Load(), r.URL.Path, r.Method)
		if route == nil {
			if pathMatch != nil {
				w.Header().Set("Allow", strings.Join(pathMatch.Methods, ", "))
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			http.NotFound(w, r)
			return
		}
		if !route.Enabled {
			http.NotFound(w, r)
			return
		}
		record.RouteID = route.ID
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		if route.RateLimit > 0 {
			decision := m.rateLimitDecision(*route, clientIP, time.Now())
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(decision.EffectiveLimit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))
			w.Header().Set("X-RateLimit-Mode", decision.Mode)
			if !decision.Allowed {
				record.RateLimitDecision = "REJECTED"
				seconds := int(math.Ceil(decision.RetryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "rate_limit_exceeded", "message": "Route request limit exceeded; retry after the current window expires.", "routeId": route.ID, "retryAfterSeconds": seconds, "reason": decision.Reason, "baseLimit": decision.BaseLimit, "effectiveLimit": decision.EffectiveLimit, "remaining": decision.Remaining, "mode": decision.Mode})
				return
			}
			record.RateLimitDecision = "ALLOWED"
		}
		cacheKey := route.ID + "|" + r.URL.RequestURI()
		if r.Method == http.MethodGet && route.CacheTtlMs > 0 {
			cacheLookup = 2
			if entry, ok := m.cached(cacheKey); ok {
				cacheLookup = 1
				for k, values := range entry.header {
					for _, value := range values {
						w.Header().Add(k, value)
					}
				}
				w.Header().Set(requestIDHeader, r.Header.Get(requestIDHeader))
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.status)
				_, _ = w.Write(entry.body)
				return
			}
		}
		groupRoutes := m.requestGroups(*route)
		if len(groupRoutes) == 0 {
			http.Error(w, "no configured upstream group", http.StatusBadGateway)
			return
		}
		activeRoute := groupRoutes[0]
		target := activeRoute.Targets[0]
		parsed, _ := url.Parse(target.URL)
		proxy := httputil.NewSingleHostReverseProxy(parsed)
		maxRetries := route.MaxRetries
		if !retryMethodAllowed(*route, r) {
			maxRetries = 0
		}
		if maxRetries > 0 && r.Body != nil {
			body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
			if readErr != nil {
				http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
		proxy.Transport = timeoutRoundTripper{next: retryRoundTripper{manager: m, route: activeRoute, fallbackRoutes: groupRoutes[1:], maxRetries: maxRetries, logger: logger, trace: trace}, timeout: time.Duration(route.TimeoutMs) * time.Millisecond}
		proxy.Director = func(out *http.Request) {
			out.URL.Scheme = parsed.Scheme
			out.URL.Host = parsed.Host
			out.Host = parsed.Host
			out.URL.Path = rewritePath(r.URL.Path, *route)
			out.URL.RawPath = ""
			out.URL.RawQuery = r.URL.RawQuery
		}
		proxy.ModifyResponse = func(response *http.Response) error {
			m.markTarget(route.ID, response.Header.Get("X-Gateway-Upstream"), true)
			record.Upstream = response.Header.Get("X-Gateway-Upstream")
			record.GroupID = response.Header.Get("X-Gateway-Group")
			m.state.mu.Lock()
			if runtime := m.state.upstreams[upstreamKey(route.ID, record.Upstream)]; runtime != nil {
				record.CircuitDecision = runtime.state
			}
			m.state.mu.Unlock()
			record.RetryCount, _ = strconv.Atoi(response.Header.Get("X-Gateway-Retries"))
			record.UpstreamDurationMs, _ = strconv.ParseFloat(response.Header.Get("X-Gateway-Upstream-Duration-Ms"), 64)
			response.Header.Del("X-Gateway-Retries")
			response.Header.Del("X-Gateway-Upstream-Duration-Ms")
			response.Header.Del("X-Gateway-Group")
			response.Header.Del("X-Gateway-Upstream")
			response.Header.Set(requestIDHeader, r.Header.Get(requestIDHeader))
			if r.Method == http.MethodGet && route.CacheTtlMs > 0 && response.StatusCode >= 200 && response.StatusCode < 300 && len(response.Header.Values("Set-Cookie")) == 0 && response.Body != nil {
				body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
				if err != nil {
					return err
				}
				response.Header.Set("X-Cache", "MISS")
				if len(body) <= 2<<20 {
					_ = response.Body.Close()
					response.Body = io.NopCloser(bytes.NewReader(body))
					m.storeCache(cacheKey, cacheEntry{body: body, header: response.Header.Clone(), status: response.StatusCode, expires: time.Now().Add(time.Duration(route.CacheTtlMs) * time.Millisecond)})
				} else {
					response.Body = &combinedReadCloser{Reader: io.MultiReader(bytes.NewReader(body), response.Body), closer: response.Body}
				}
			}
			return nil
		}
		proxy.ErrorHandler = func(response http.ResponseWriter, request *http.Request, err error) {
			status, failure := http.StatusBadGateway, "backend_connection_failed"
			var networkError interface{ Timeout() bool }
			if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
				status, failure = http.StatusGatewayTimeout, "backend_timeout"
			}
			logger.Error("upstream request failed", "request_id", request.Header.Get(requestIDHeader), "failure", failure, "route_id", route.ID)
			http.Error(response, http.StatusText(status), status)
		}
		ctx, cancel := context.WithTimeout(r.Context(), min(globalTimeout, time.Duration(route.TimeoutMs)*time.Millisecond))
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *Manager) allow(client, route string, limit int) (bool, time.Duration) {
	s := m.state
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) > 10000 {
		for k, v := range s.clients {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > time.Minute {
				delete(s.clients, k)
			}
		}
		if len(s.clients) > 10000 {
			return false, time.Minute
		}
	}
	key := route + "|" + client
	hits := s.clients[key][:0]
	for _, t := range s.clients[key] {
		if now.Sub(t) < time.Minute {
			hits = append(hits, t)
		}
	}
	if len(hits) >= limit {
		s.clients[key] = hits
		return false, time.Until(hits[0].Add(time.Minute))
	}
	s.clients[key] = append(hits, now)
	return true, 0
}
func (m *Manager) cached(key string) (cacheEntry, bool) {
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok {
		return e, false
	}
	if time.Now().After(e.expires) {
		delete(s.cache, key)
		return e, false
	}
	return e, true
}
func (m *Manager) storeCache(key string, e cacheEntry) {
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= 1000 {
		for k, v := range s.cache {
			if time.Now().After(v.expires) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= 1000 {
			for k := range s.cache {
				delete(s.cache, k)
				break
			}
		}
	}
	s.cache[key] = e
}

func joinURLPath(base, incoming string) string {
	if base == "" || base == "/" {
		return incoming
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(incoming, "/")
}

var defaultTransport = &http.Transport{
	Proxy:             http.ProxyFromEnvironment,
	DialContext:       (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2: true, MaxIdleConns: 200, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
	TLSHandshakeTimeout: 10 * time.Second,
}

func lookup(snapshot *Snapshot, requestPath, method string) (*Route, *Route) {
	var matched, pathOnly *Route
	for index := range snapshot.Routes {
		route := &snapshot.Routes[index]
		if !routeMatches(*route, requestPath) {
			continue
		}
		if pathOnly == nil || routeMoreSpecific(*route, *pathOnly) {
			pathOnly = route
		}
		if contains(route.Methods, method) && (matched == nil || routeMoreSpecific(*route, *matched)) {
			matched = route
		}
	}
	if matched != nil {
		return matched, pathOnly
	}
	return nil, pathOnly
}

func routeMatches(route Route, requestPath string) bool {
	if route.MatchMode == "EXACT" {
		return requestPath == route.Path
	}
	if route.Path == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	return requestPath == route.Path || strings.HasPrefix(requestPath, strings.TrimSuffix(route.Path, "/")+"/")
}

func routeMoreSpecific(a, b Route) bool {
	if len(a.Path) != len(b.Path) {
		return len(a.Path) > len(b.Path)
	}
	return a.MatchMode == "EXACT" && b.MatchMode != "EXACT"
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value || item == "*" {
			return true
		}
	}
	return false
}

func (m *Manager) markTarget(routeID, target string, healthy bool) {
	m.state.mu.Lock()
	m.state.probes[routeID+"|"+target] = healthy
	m.state.mu.Unlock()
}

func rewritePath(requestPath string, route Route) string {
	if route.MatchMode == "EXACT" {
		if route.RewritePrefix != "" {
			return route.RewritePrefix
		}
		return requestPath
	}
	if route.RewritePrefix != "" {
		suffix := strings.TrimPrefix(requestPath, strings.TrimSuffix(route.Path, "/"))
		return strings.TrimSuffix(route.RewritePrefix, "/") + suffix
	}
	if route.StripPrefix {
		suffix := strings.TrimPrefix(requestPath, strings.TrimSuffix(route.Path, "/"))
		if suffix == "" {
			return "/"
		}
		return suffix
	}
	return requestPath
}

func sortRoutes(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool { return routeMoreSpecific(routes[i], routes[j]) })
}
