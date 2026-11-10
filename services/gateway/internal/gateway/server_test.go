package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseBackendURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "http target", input: "http://localhost:4001"},
		{name: "https target", input: "https://backend.example/base"},
		{name: "relative target", input: "/backend", wantErr: true},
		{name: "unsupported scheme", input: "ftp://localhost:21", wantErr: true},
		{name: "credentials", input: "http://user:pass@localhost:4001", wantErr: true},
		{name: "fragment", input: "http://localhost:4001#fragment", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBackendURL(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseBackendURL(%q) error = %v, wantErr %v", test.input, err, test.wantErr)
			}
		})
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	base := Config{
		Addr:            ":8080",
		BackendURL:      "http://localhost:4001",
		RequestTimeout:  time.Second,
		UpstreamTimeout: time.Second,
		Logger:          slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "missing address", change: func(cfg *Config) { cfg.Addr = "" }},
		{name: "missing backend", change: func(cfg *Config) { cfg.BackendURL = "" }},
		{name: "zero request timeout", change: func(cfg *Config) { cfg.RequestTimeout = 0 }},
		{name: "negative upstream timeout", change: func(cfg *Config) { cfg.UpstreamTimeout = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.change(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New() succeeded with invalid configuration")
			}
		})
	}
}

func TestNewRequestIDIsUUIDAndUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id, err := newRequestID()
		if err != nil {
			t.Fatalf("newRequestID() error = %v", err)
		}
		if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("request ID %q is not a UUID v4", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate request ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestRequestLoggingExcludesSensitiveHeaders(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	server := mustGateway(t, backend.URL, logger, time.Second, time.Second)
	request := httptest.NewRequest(http.MethodGet, "/private?token=query-secret", nil)
	request.Header.Set("Authorization", "Bearer header-secret")
	request.Header.Set("X-API-Key", "api-key-secret")
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if response.Header().Get(requestIDHeader) == "" {
		t.Fatal("response missing request ID")
	}
	for _, secret := range []string{"header-secret", "api-key-secret", "query-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("access log contains sensitive value %q: %s", secret, logs.String())
		}
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("access log is not JSON: %v", err)
	}
	if record["status"] != float64(http.StatusNoContent) {
		t.Fatalf("logged status = %v, want %d", record["status"], http.StatusNoContent)
	}
	if record["duration_ms"] == nil || record["request_id"] != response.Header().Get(requestIDHeader) {
		t.Fatalf("access log missing request metadata: %#v", record)
	}
}

func TestTimeoutRoundTripperTimesOutUpstream(t *testing.T) {
	transport := timeoutRoundTripper{
		next: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		timeout: 10 * time.Millisecond,
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://backend.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.RoundTrip(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip() error = %v, want deadline exceeded", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func mustGateway(t *testing.T, backendURL string, logger *slog.Logger, requestTimeout, upstreamTimeout time.Duration) *http.Server {
	t.Helper()
	server, err := New(Config{
		Addr:            ":8080",
		BackendURL:      backendURL,
		RequestTimeout:  requestTimeout,
		UpstreamTimeout: upstreamTimeout,
		Logger:          logger,
		AdminToken:      "local-integration-key-which-is-long-enough",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server
}
