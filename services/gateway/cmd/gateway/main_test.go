package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gatekeeper/gatekeeper/services/gateway/internal/gateway"
)

func TestPollConfigurationAuthenticatesActivatesAndAcknowledges(t *testing.T) {
	manager, err := gateway.NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgements int
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gateway-ID") != "gateway-test" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing gateway credentials: headers=%v", r.Header)
		}
		switch r.URL.Path {
		case "/internal/gateway/config":
			if r.Header.Get("If-None-Match") == `"1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"1"`)
			_ = json.NewEncoder(w).Encode(gateway.Snapshot{Version: "1", Routes: []gateway.Route{{
				ID: "users", Path: "/api/users", MatchMode: "PREFIX", Methods: []string{"GET"}, TimeoutMs: 5000,
				Enabled: true, ServiceID: "users", Targets: []gateway.Target{{URL: "http://127.0.0.1:4001", Weight: 1}},
			}}})
		case "/internal/gateway/config/ack":
			var ack map[string]any
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Errorf("decode ack: %v", err)
			}
			if ack["version"] != float64(1) || ack["status"] != "APPLIED" {
				t.Errorf("unexpected acknowledgement: %#v", ack)
			}
			acknowledgements++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer controlPlane.Close()

	client := controlPlane.Client()
	etag := ""
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	pollOnce(context.Background(), client, controlPlane.URL, "gateway-test", "test-token", manager, &etag, logger)
	if manager.Current().Version != "1" {
		t.Fatalf("active version = %s, want 1", manager.Current().Version)
	}
	if etag != `"1"` {
		t.Fatalf("ETag = %q, want %q", etag, `"1"`)
	}
	pollOnce(context.Background(), client, controlPlane.URL, "gateway-test", "test-token", manager, &etag, logger)
	if acknowledgements != 1 {
		t.Fatalf("acknowledgements = %d, want 1", acknowledgements)
	}
}

func TestWatchConfigFileReloadsValidSnapshotsAndKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.json")
	manager, err := gateway.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigFile(ctx, path, manager, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	first := gateway.Snapshot{Version: "1", Routes: []gateway.Route{{ID: "users", Path: "/users", MatchMode: "PREFIX", Methods: []string{"GET"}, TimeoutMs: 1000, Enabled: true, ServiceID: "users", Targets: []gateway.Target{{URL: "http://127.0.0.1:4001", Weight: 1}}}}}
	data, _ := json.Marshal(first)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for manager.Current().Version != "1" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if manager.Current().Version != "1" {
		t.Fatal("valid configuration file was not loaded")
	}
	if err := os.WriteFile(path, []byte(`{"version":"2","routes":[{"id":"bad","serviceId":"bad","path":"/bad","matchMode":"PREFIX","methods":["GET"],"timeoutMs":0,"enabled":true,"targets":[{"url":"http://localhost:1","weight":1}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if manager.Current().Version != "1" {
		t.Fatalf("invalid file replaced last good version: %s", manager.Current().Version)
	}
}

func TestPollInvalidConfigurationRetainsLastGoodAndRejects(t *testing.T) {
	manager, err := gateway.NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(gateway.Snapshot{Version: "1", Routes: []gateway.Route{{
		ID: "users", Path: "/api/users", MatchMode: "PREFIX", Methods: []string{"GET"}, TimeoutMs: 5000,
		Enabled: true, ServiceID: "users", Targets: []gateway.Target{{URL: "http://127.0.0.1:4001", Weight: 1}},
	}}}); err != nil {
		t.Fatal(err)
	}
	var ackStatus string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/gateway/config" {
			_, _ = io.WriteString(w, `{"version":"2","routes":[{"id":"bad","path":"/bad","matchMode":"PREFIX","methods":["GET"],"timeoutMs":10,"enabled":true,"serviceId":"bad","targets":[{"url":"file:///bad","weight":1}]}]}`)
			return
		}
		if r.URL.Path == "/internal/gateway/config/ack" {
			var ack map[string]any
			_ = json.NewDecoder(r.Body).Decode(&ack)
			ackStatus = ack["status"].(string)
			if !strings.Contains(ack["message"].(string), "target URL") {
				t.Errorf("rejection reason was not included: %#v", ack)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer controlPlane.Close()

	pollOnce(context.Background(), controlPlane.Client(), controlPlane.URL, "gateway-test", "test-token", manager, new(string), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if manager.Current().Version != "1" || ackStatus != "REJECTED" {
		t.Fatalf("active version=%s acknowledgement=%s", manager.Current().Version, ackStatus)
	}
}
