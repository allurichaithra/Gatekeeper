package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyGetAndPost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":1,"name":"Ada"}]`)
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.Copy(w, r.Body)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer backend.Close()

	proxy := startGateway(t, backend.URL, time.Second, time.Second)
	defer proxy.Close()

	getResponse, err := http.Get(proxy.URL + "/api/users")
	if err != nil {
		t.Fatalf("GET /api/users failed: %v", err)
	}
	getBody, _ := io.ReadAll(getResponse.Body)
	_ = getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK || !strings.Contains(string(getBody), "Ada") {
		t.Fatalf("GET response = %d %s", getResponse.StatusCode, getBody)
	}

	postResponse, err := http.Post(proxy.URL+"/api/users", "application/json", strings.NewReader(`{"name":"Grace"}`))
	if err != nil {
		t.Fatalf("POST /api/users failed: %v", err)
	}
	postBody, _ := io.ReadAll(postResponse.Body)
	_ = postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusCreated || string(postBody) != `{"name":"Grace"}` {
		t.Fatalf("POST response = %d %s", postResponse.StatusCode, postBody)
	}
	if getResponse.Header.Get(requestIDHeader) == "" || postResponse.Header.Get(requestIDHeader) == "" || getResponse.Header.Get(requestIDHeader) == postResponse.Header.Get(requestIDHeader) {
		t.Fatal("requests must receive distinct response request IDs")
	}
}

func TestHealthEndpoint(t *testing.T) {
	proxy := startGateway(t, "http://127.0.0.1:1", time.Second, time.Second)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}` {
		t.Fatalf("health response = %d %s", response.StatusCode, body)
	}
	if response.Header.Get(requestIDHeader) == "" {
		t.Fatal("health response missing request ID")
	}
}

func TestGatewayAdminEndpointsRequireAPIKey(t *testing.T) {
	proxy := startGateway(t, "http://127.0.0.1:1", time.Second, time.Second)
	defer proxy.Close()
	for _, path := range []string{"/health/config", "/api/gateway/metrics", "/api/gateway/routes", "/api/gateway/health", "/api/gateway/requests", "/api/gateway/adaptive", "/api/gateway/adaptive/history"} {
		for _, token := range []string{"", "Bearer wrong-key"} {
			request, _ := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
			if token != "" {
				request.Header.Set("Authorization", token)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s token %q returned %d", path, token, response.StatusCode)
			}
		}
		request, _ := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
		request.Header.Set("Authorization", "Bearer local-integration-key-which-is-long-enough")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s with key: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s with key returned %d", path, response.StatusCode)
		}
	}
}

func TestRequestInspectorRecordsSanitizedMetadataAndConsistentRequestID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	manager, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	route := testRoute("inspect", "/inspect", "PREFIX", backend.URL)
	if err := manager.Apply(testSnapshot("1", route)); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Addr: ":0", RequestTimeout: time.Second, UpstreamTimeout: time.Second, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Manager: manager, AdminToken: "integration-secret-key-long-enough"})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler)
	defer proxy.Close()
	request, _ := http.NewRequest(http.MethodGet, proxy.URL+"/inspect/item?private-query=hidden", nil)
	request.Header.Set("Authorization", "Bearer private-token")
	request.Header.Set("Cookie", "session=private-cookie")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	id := response.Header.Get(requestIDHeader)
	if id == "" {
		t.Fatal("response missing request ID")
	}
	record, ok := manager.RequestByID(id)
	if !ok {
		t.Fatal("request record missing")
	}
	if record.RequestID != id || record.RouteID != route.ID || record.Status != http.StatusOK || record.Upstream != backend.URL || record.Path != "/inspect/item" {
		t.Fatalf("request record does not match sanitized request: %#v", record)
	}
	encoded, _ := json.Marshal(record)
	for _, secret := range []string{"private-token", "private-cookie", "private-query"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("request history leaked %q", secret)
		}
	}
}

func TestConfigHealthIncludesRuntimeAndVersion(t *testing.T) {
	proxy := startGateway(t, "http://127.0.0.1:1", time.Second, time.Second)
	defer proxy.Close()

	request, _ := http.NewRequest(http.MethodGet, proxy.URL+"/health/config", nil)
	request.Header.Set("Authorization", "Bearer local-integration-key-which-is-long-enough")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /health/config failed: %v", err)
	}
	defer response.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != "1" || body["startedAt"] == "" {
		t.Fatalf("unexpected runtime metadata: %#v", body)
	}
}

func TestUnavailableBackendReturnsBadGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	backendURL := backend.URL
	backend.Close()

	proxy := startGateway(t, backendURL, time.Second, time.Second)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/api/users")
	if err != nil {
		t.Fatalf("GET failed before gateway response: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "Bad Gateway") {
		t.Fatalf("unavailable backend response = %d %s", response.StatusCode, body)
	}
	if response.Header.Get(requestIDHeader) == "" {
		t.Fatal("failure response missing request ID")
	}
}

func TestSlowBackendReturnsGatewayTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()

	proxy := startGateway(t, backend.URL, time.Second, 30*time.Millisecond)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/slow")
	if err != nil {
		t.Fatalf("GET failed before gateway response: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusGatewayTimeout)
	}
}

func TestGracefulShutdownWaitsForInFlightProxyRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	}))
	defer backend.Close()

	server := mustGateway(t, backend.URL, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), time.Second*5, time.Second*5)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	responseDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String() + "/in-flight")
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		responseDone <- err
	}()
	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- server.Shutdown(ctx)
	}()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before active request drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-responseDone; err != nil {
		t.Fatalf("in-flight request error = %v", err)
	}
	if err := <-serveDone; err != http.ErrServerClosed {
		t.Fatalf("Serve() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func TestRequestIDsAreNotTakenFromClient(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(requestIDHeader, "backend-chosen")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"request_id": r.Header.Get(requestIDHeader)})
	}))
	defer backend.Close()

	proxy := startGateway(t, backend.URL, time.Second, time.Second)
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodGet, proxy.URL+"/id", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(requestIDHeader, "attacker-chosen")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["request_id"] == "" || body["request_id"] == "attacker-chosen" || body["request_id"] != response.Header.Get(requestIDHeader) || response.Header.Get(requestIDHeader) == "backend-chosen" {
		t.Fatalf("request ID was not generated and propagated: body=%q header=%q", body["request_id"], response.Header.Get(requestIDHeader))
	}
}

func startGateway(t *testing.T, backendURL string, requestTimeout, upstreamTimeout time.Duration) *httptest.Server {
	t.Helper()
	server := mustGateway(t, backendURL, slog.New(slog.NewJSONHandler(io.Discard, nil)), requestTimeout, upstreamTimeout)
	return httptest.NewServer(server.Handler)
}
