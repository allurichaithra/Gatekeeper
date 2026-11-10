package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const requestIDHeader = "X-Request-ID"

// Config contains the HTTP and upstream settings needed to run the gateway.
type Config struct {
	Addr            string
	BackendURL      string
	RequestTimeout  time.Duration
	UpstreamTimeout time.Duration
	Logger          *slog.Logger
	Manager         *Manager
	AdminToken      string
}

// New constructs the gateway server. Configuration is checked before the
// listener is opened so invalid backend addresses fail at startup.
func New(cfg Config) (*http.Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("listen address is required")
	}
	if cfg.RequestTimeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if cfg.UpstreamTimeout <= 0 {
		return nil, errors.New("upstream timeout must be positive")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Manager == nil && cfg.BackendURL == "" {
		return nil, errors.New("backend URL is required when no dynamic configuration manager is supplied")
	}

	manager := cfg.Manager
	if manager == nil {
		var err error
		manager, err = NewManager("")
		if err != nil {
			return nil, err
		}
		if cfg.BackendURL != "" {
			target, err := parseBackendURL(cfg.BackendURL)
			if err != nil {
				return nil, fmt.Errorf("invalid backend URL: %w", err)
			}
			if err := manager.Apply(Snapshot{Version: "1", Routes: []Route{{
				ID: "legacy-default", ServiceID: "legacy-default", Path: "/", MatchMode: "PREFIX", Methods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"}, TimeoutMs: int(cfg.UpstreamTimeout.Milliseconds()), Enabled: true, Targets: []Target{{URL: target.String(), Weight: 1}},
			}}}); err != nil {
				return nil, err
			}
		}
	}
	startedAt := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("X-Gateway-Config-Version", manager.Current().Version)
		}
		healthHandler(w, r)
	})
	mux.Handle("/health/config", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": manager.Current().Version, "startedAt": startedAt.Format(time.RFC3339Nano)})
	})))
	mux.Handle("/api/gateway/metrics", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.Metrics())
	})))
	mux.Handle("/api/gateway/routes", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.Current())
	})))
	mux.Handle("/api/gateway/health", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.Health())
	})))
	mux.Handle("/api/gateway/adaptive", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.AdaptiveTelemetry())
	})))
	mux.Handle("/api/gateway/adaptive/history", requireGatewayKey(cfg.AdminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.AdaptiveHistory())
	})))
	if cfg.AdminToken != "" {
		mux.HandleFunc("/internal/config", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			provided := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+cfg.AdminToken)) != 1 {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			var snapshot Snapshot
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&snapshot); err != nil {
				http.Error(w, "invalid configuration payload", http.StatusBadRequest)
				return
			}
			if err := manager.Apply(snapshot); err != nil {
				http.Error(w, "configuration rejected", http.StatusUnprocessableEntity)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.Handle("/", manager.Handler(cfg.RequestTimeout, cfg.Logger))

	handler := requestIDMiddleware(cfg.Logger,
		accessLogMiddleware(cfg.Logger,
			timeoutMiddleware(cfg.RequestTimeout, mux),
		),
	)

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

func requireGatewayKey(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" {
			http.Error(w, "gateway management key is not configured", http.StatusServiceUnavailable)
			return
		}
		provided := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+key)) != 1 {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseBackendURL(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return nil, errors.New("target must be an absolute http or https URL")
	}
	if target.User != nil || target.RawQuery != "" {
		return nil, errors.New("backend URL must not contain credentials or a query string")
	}
	if target.Fragment != "" {
		return nil, errors.New("backend URL must not contain a fragment")
	}
	return target, nil
}

func newReverseProxy(target *url.URL, timeout time.Duration, logger *slog.Logger) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   min(timeout, 10*time.Second),
		ResponseHeaderTimeout: timeout,
	}
	proxy.Transport = timeoutRoundTripper{next: proxy.Transport, timeout: timeout}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		status := http.StatusBadGateway
		failure := "backend_connection_failed"
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
			status = http.StatusGatewayTimeout
			failure = "backend_timeout"
		}
		logger.Error("upstream request failed",
			"request_id", r.Header.Get(requestIDHeader),
			"failure", failure,
		)
		http.Error(w, http.StatusText(status), status)
	}
	return proxy
}

type timeoutRoundTripper struct {
	next    http.RoundTripper
	timeout time.Duration
}

func (t timeoutRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(request.Context(), t.timeout)
	response, err := t.next.RoundTrip(request.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	response.Header.Set(requestIDHeader, request.Header.Get(requestIDHeader))
	if response.Body == nil {
		cancel()
		return response, nil
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func timeoutMiddleware(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := newRequestID()
		if err != nil {
			logger.Error("could not generate request ID")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set(requestIDHeader, requestID)
		r.Header.Set(requestIDHeader, requestID)
		next.ServeHTTP(w, r)
	})
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	// UUID v4 layout gives a compact, standard representation with 122 random bits.
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], id[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], id[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], id[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], id[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], id[10:16])
	return string(encoded), nil
}

func accessLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		capture := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("request completed",
			"request_id", r.Header.Get(requestIDHeader),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

// statusWriter records status while preserving the optional interfaces used by
// net/http and ReverseProxy.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking is not supported")
	}
	return hijacker.Hijack()
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}
