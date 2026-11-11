package gateway

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	adaptiveInterval         = 5 * time.Second
	adaptiveWindowBuckets    = 12
	adaptiveMinSpikeRequests = 10
	adaptiveReduction        = 0.25
	adaptiveMaxDecisions     = 500
	adaptiveMaxClients       = 10000
	adaptiveMaxClientHits    = 1000000
)

type adaptiveBucket struct {
	epoch          int64
	requests       uint64
	throttled      uint64
	baselineRPS    float64
	effectiveLimit int
}

type adaptiveRouteState struct {
	buckets        [adaptiveWindowBuckets]adaptiveBucket
	currentEpoch   int64
	baselineRPS    float64
	evaluations    int
	spikes         uint64
	lastSpike      time.Time
	lastAdapted    time.Time
	lastRecovery   time.Time
	effective      int
	recovering     bool
	overrideLimit  int
	overrideExpiry int64
}

// AdaptiveDecision is a bounded, privacy-safe explanation of a rate-limit change.
type AdaptiveDecision struct {
	RouteID        string    `json:"routeId"`
	Type           string    `json:"type"`
	At             time.Time `json:"at"`
	Explanation    string    `json:"explanation"`
	EffectiveLimit int       `json:"effectiveLimit"`
}

// RateLimitDecision describes the enforcement result for one route/client request.
type RateLimitDecision struct {
	Allowed        bool          `json:"allowed"`
	BaseLimit      int           `json:"baseLimit"`
	EffectiveLimit int           `json:"effectiveLimit"`
	Remaining      int           `json:"remaining"`
	RetryAfter     time.Duration `json:"retryAfter"`
	Mode           string        `json:"mode"`
	Reason         string        `json:"reason,omitempty"`
}

type adaptivePoint struct {
	Timestamp      int64   `json:"timestamp"`
	IncomingRPS    float64 `json:"incomingRps"`
	BaselineRPS    float64 `json:"baselineRps"`
	EffectiveLimit int     `json:"effectiveLimit"`
	Throttled      uint64  `json:"throttled"`
}

func (m *Manager) rateLimitDecision(route Route, client string, now time.Time) RateLimitDecision {
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisionCount++
	if s.decisionCount%128 == 0 {
		for key, hits := range s.clients {
			if len(hits) == 0 || now.Sub(hits[len(hits)-1]) >= time.Minute {
				s.clientHitCount -= len(hits)
				delete(s.clients, key)
			}
		}
	}
	mode := "FIXED"
	state := s.adaptive[route.ID]
	if state == nil {
		state = &adaptiveRouteState{overrideLimit: route.ManualOverrideLimit, overrideExpiry: route.ManualOverrideExpiresAt}
		s.adaptive[route.ID] = state
	}
	if state.overrideLimit > 0 && state.overrideExpiry > 0 && state.overrideExpiry <= now.UnixMilli() {
		m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "administrator_override_expired", At: now.UTC(), Explanation: "The temporary administrator override expired; automatic route protection resumed.", EffectiveLimit: state.effective})
		state.overrideLimit, state.overrideExpiry = 0, 0
	}
	adaptive := route.RateLimitMode == "ADAPTIVE" && route.AdaptiveEnabled
	if adaptive {
		mode = "ADAPTIVE"
	}
	base := route.RateLimit
	_, maximum := adaptiveBounds(route)
	if state.effective == 0 || state.effective > maximum {
		state.effective = min(base, maximum)
	}
	if adaptive {
		m.observeAdaptiveLocked(route, state, now)
	}
	effective := base
	if adaptive {
		effective = state.effective
	}
	if route.ManualOverrideLimit > 0 && route.ManualOverrideExpiresAt > now.UnixMilli() {
		effective = route.ManualOverrideLimit
		if adaptive {
			mode = "ADAPTIVE_OVERRIDE"
		} else {
			mode = "FIXED_OVERRIDE"
		}
	}

	key := route.ID + "|" + client
	if len(s.clients) >= adaptiveMaxClients && s.clients[key] == nil {
		for candidate, hits := range s.clients {
			if len(hits) == 0 || now.Sub(hits[len(hits)-1]) >= time.Minute {
				s.clientHitCount -= len(hits)
				delete(s.clients, candidate)
			}
		}
		if len(s.clients) >= adaptiveMaxClients {
			return RateLimitDecision{Allowed: false, BaseLimit: base, EffectiveLimit: effective, Mode: mode, Reason: "client_state_capacity", RetryAfter: time.Minute}
		}
	}
	oldHits := s.clients[key]
	hits := oldHits[:0]
	for _, at := range oldHits {
		if now.Sub(at) < time.Minute {
			hits = append(hits, at)
		}
	}
	s.clientHitCount -= len(oldHits) - len(hits)
	if len(hits) == 0 {
		delete(s.clients, key)
	}
	decision := RateLimitDecision{BaseLimit: base, EffectiveLimit: effective, Remaining: max(0, effective-len(hits)), Mode: mode}
	if len(hits) >= effective {
		decision.Allowed = false
		decision.Remaining = 0
		decision.Reason = "per_ip_route_limit_exceeded"
		decision.RetryAfter = hits[0].Add(time.Minute).Sub(now)
		if decision.RetryAfter < time.Second {
			decision.RetryAfter = time.Second
		}
		if adaptive {
			epoch := now.Unix() / int64(adaptiveInterval.Seconds())
			bucket := &state.buckets[epoch%adaptiveWindowBuckets]
			if bucket.epoch != epoch {
				*bucket = adaptiveBucket{epoch: epoch}
			}
			bucket.throttled++
			s.throttled[route.ID]++
		}
		if len(hits) > 0 {
			s.clients[key] = hits
		}
		return decision
	}
	if s.clientHitCount >= adaptiveMaxClientHits {
		decision.Allowed = false
		decision.Remaining = 0
		decision.Reason = "client_state_capacity"
		decision.RetryAfter = time.Minute
		return decision
	}
	s.clients[key] = append(hits, now)
	s.clientHitCount++
	decision.Allowed = true
	decision.Remaining = max(0, effective-len(hits)-1)
	return decision
}

func adaptiveBounds(route Route) (int, int) {
	minimum := route.MinimumRateLimit
	if minimum == 0 && route.RateLimit > 0 {
		minimum = max(1, int(math.Ceil(float64(route.RateLimit)*0.2)))
	}
	maximum := route.MaximumRateLimit
	if maximum == 0 {
		maximum = route.RateLimit
	}
	if maximum == 0 {
		maximum = route.RateLimit
	}
	return minimum, maximum
}

func (m *Manager) observeAdaptiveLocked(route Route, state *adaptiveRouteState, now time.Time) {
	epoch := now.Unix() / int64(adaptiveInterval.Seconds())
	if state.currentEpoch == 0 {
		state.currentEpoch = epoch
	}
	if epoch > state.currentEpoch {
		steps := min(epoch-state.currentEpoch, int64(adaptiveWindowBuckets))
		for step := int64(0); step < steps; step++ {
			closedEpoch := state.currentEpoch + step
			bucket := state.buckets[closedEpoch%adaptiveWindowBuckets]
			incoming := float64(0)
			if bucket.epoch == closedEpoch {
				incoming = float64(bucket.requests) / adaptiveInterval.Seconds()
			}
			m.evaluateAdaptiveLocked(route, state, incoming, time.Unix((closedEpoch+1)*int64(adaptiveInterval.Seconds()), 0))
			if bucket.epoch == closedEpoch {
				bucket.baselineRPS, bucket.effectiveLimit = state.baselineRPS, state.effective
			}
			state.evaluations++
		}
		state.currentEpoch = epoch
	}
	bucket := &state.buckets[epoch%adaptiveWindowBuckets]
	if bucket.epoch != epoch {
		*bucket = adaptiveBucket{epoch: epoch}
	}
	bucket.requests++
	bucket.baselineRPS, bucket.effectiveLimit = state.baselineRPS, state.effective
}

func (m *Manager) evaluateAdaptiveLocked(route Route, state *adaptiveRouteState, incoming float64, now time.Time) {
	base := route.RateLimit
	minimum, maximum := adaptiveBounds(route)
	if maximum == 0 {
		maximum = base
	}
	if state.effective == 0 {
		state.effective = min(base, maximum)
	}
	sensitivity := route.SpikeSensitivity
	if sensitivity == 0 {
		sensitivity = 2
	}
	cooldown := time.Duration(route.RecoveryCooldownMs) * time.Millisecond
	if cooldown == 0 {
		cooldown = 30 * time.Second
	}
	reliable := state.evaluations >= 3 && state.baselineRPS > 0
	spike := reliable && incoming >= adaptiveMinSpikeRequests/adaptiveInterval.Seconds() && incoming > state.baselineRPS*sensitivity
	if spike {
		state.spikes++
		state.lastSpike = now
		state.recovering = false
		if route.ManualOverrideLimit == 0 || route.ManualOverrideExpiresAt <= now.UnixMilli() {
			newLimit := max(minimum, int(math.Floor(float64(state.effective)*(1-adaptiveReduction))))
			if newLimit > maximum {
				newLimit = maximum
			}
			if newLimit < state.effective {
				state.effective = newLimit
				state.lastAdapted = now
				m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "spike_detected", At: now.UTC(), Explanation: fmt.Sprintf("Incoming route traffic reached %.2fx baseline (%.2f vs %.2f requests/sec).", incoming/state.baselineRPS, incoming, state.baselineRPS), EffectiveLimit: state.effective})
				m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "effective_limit_reduced", At: now.UTC(), Explanation: "The effective per-IP limit was reduced by 25% to protect this route.", EffectiveLimit: state.effective})
				return
			}
		}
		m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "spike_detected", At: now.UTC(), Explanation: fmt.Sprintf("Incoming route traffic reached %.2fx baseline (%.2f vs %.2f requests/sec).", incoming/state.baselineRPS, incoming, state.baselineRPS), EffectiveLimit: state.effective})
		return
	}
	if state.baselineRPS == 0 {
		state.baselineRPS = incoming
	} else {
		alpha := 2 / float64(adaptiveWindowBuckets+1)
		state.baselineRPS = alpha*incoming + (1-alpha)*state.baselineRPS
	}
	if state.effective < min(base, maximum) && now.Sub(state.lastSpike) >= cooldown {
		if !state.recovering {
			state.recovering = true
			state.lastRecovery = now
			m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "recovery_started", At: now.UTC(), Explanation: "Traffic has stabilized through the recovery cooldown; the effective limit is recovering.", EffectiveLimit: state.effective})
		} else if now.Sub(state.lastRecovery) >= cooldown {
			step := max(1, int(math.Ceil(float64(base)*0.05)))
			state.effective = min(min(base, maximum), state.effective+step)
			state.lastRecovery, state.lastAdapted = now, now
			m.addAdaptiveDecisionLocked(AdaptiveDecision{RouteID: route.ID, Type: "effective_limit_increased", At: now.UTC(), Explanation: fmt.Sprintf("Stable traffic allowed a gradual increase of %d requests/sec.", step), EffectiveLimit: state.effective})
			if state.effective >= min(base, maximum) {
				state.recovering = false
			}
		}
	}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (m *Manager) addAdaptiveDecisionLocked(decision AdaptiveDecision) {
	m.state.decisions = append(m.state.decisions, decision)
	if len(m.state.decisions) > adaptiveMaxDecisions {
		m.state.decisions = append([]AdaptiveDecision(nil), m.state.decisions[len(m.state.decisions)-adaptiveMaxDecisions:]...)
	}
}

// AdaptiveTelemetry returns live per-route state and the latest 60 seconds of buckets.
func (m *Manager) AdaptiveTelemetry() map[string]any {
	now := time.Now()
	s := m.state
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]map[string]any, 0)
	for _, route := range m.current.Load().Routes {
		if route.RateLimitMode != "ADAPTIVE" {
			continue
		}
		state := s.adaptive[route.ID]
		if state == nil {
			_, maximum := adaptiveBounds(route)
			state = &adaptiveRouteState{effective: min(route.RateLimit, maximum)}
		}
		minimum, maximum := adaptiveBounds(route)
		incoming, varThrottled := float64(0), uint64(0)
		series := make([]adaptivePoint, 0, adaptiveWindowBuckets)
		for epoch := now.Unix()/5 - int64(adaptiveWindowBuckets) + 1; epoch <= now.Unix()/5; epoch++ {
			bucket := state.buckets[epoch%adaptiveWindowBuckets]
			point := adaptivePoint{Timestamp: (epoch + 1) * 5, BaselineRPS: state.baselineRPS, EffectiveLimit: state.effective}
			if bucket.epoch == epoch {
				point.IncomingRPS = float64(bucket.requests) / adaptiveInterval.Seconds()
				point.Throttled = bucket.throttled
				point.BaselineRPS = bucket.baselineRPS
				if bucket.effectiveLimit > 0 {
					point.EffectiveLimit = bucket.effectiveLimit
				}
				incoming = point.IncomingRPS
				varThrottled += bucket.throttled
			}
			series = append(series, point)
		}
		effective := state.effective
		if route.ManualOverrideLimit > 0 && route.ManualOverrideExpiresAt > now.UnixMilli() {
			effective = route.ManualOverrideLimit
		}
		status := "normal"
		if state.spikes > 0 && now.Sub(state.lastSpike) < 2*adaptiveInterval {
			status = "spike_detected"
		} else if effective < min(route.RateLimit, maximum) {
			status = "rate_limit_reduced"
			if state.recovering {
				status = "recovering"
			}
		}
		result = append(result, map[string]any{"routeId": route.ID, "path": route.Path, "mode": route.RateLimitMode, "enabled": route.AdaptiveEnabled, "baselineReliable": state.evaluations >= 3 && state.baselineRPS > 0, "effectiveLimit": effective, "status": status, "incomingRps": incoming, "throttled": varThrottled, "minimum": minimum, "maximum": maximum, "series": series})
	}
	decisions := append([]AdaptiveDecision(nil), s.decisions...)
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].At.After(decisions[j].At) })
	return map[string]any{"routes": result, "decisions": decisions}
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

// AdaptiveHistory returns a copy so callers cannot mutate limiter state.
func (m *Manager) AdaptiveHistory() []AdaptiveDecision {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	result := append([]AdaptiveDecision(nil), m.state.decisions...)
	sort.Slice(result, func(i, j int) bool { return result[i].At.After(result[j].At) })
	return result
}

