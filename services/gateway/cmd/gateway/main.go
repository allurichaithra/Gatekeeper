package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gatekeeper/gatekeeper/services/gateway/internal/gateway"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	if err := run(logger); err != nil {
		logger.Error("gateway stopped with an error", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	gatewayAPIKey := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY"))
	if len(gatewayAPIKey) < 32 {
		return errors.New("GATEWAY_API_KEY must be configured with at least 32 characters")
	}
	requestTimeout, err := durationEnv("GATEWAY_REQUEST_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}
	upstreamTimeout, err := durationEnv("GATEWAY_UPSTREAM_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}

	configFile := env("GATEWAY_CONFIG_FILE", defaultConfigPath())
	if !filepath.IsAbs(configFile) {
		configFile = filepath.Join(projectRoot(), configFile)
	}
	manager, err := gateway.NewManager(configFile)
	if err != nil {
		return err
	}
	server, err := gateway.New(gateway.Config{
		Addr:            env("GATEWAY_LISTEN_ADDR", ":8080"),
		BackendURL:      os.Getenv("GATEWAY_BACKEND_URL"),
		RequestTimeout:  requestTimeout,
		UpstreamTimeout: upstreamTimeout,
		Logger:          logger,
		Manager:         manager,
		AdminToken:      gatewayAPIKey,
	})
	if err != nil {
		return err
	}

	serverErrors := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.RunHealthChecks(ctx, 5*time.Second)
	go func() {
		logger.Info("gateway listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()
	go watchConfigFile(ctx, configFile, manager, logger)
	if controlURL := strings.TrimRight(os.Getenv("CONTROL_PLANE_URL"), "/"); controlURL != "" {
		go pollConfiguration(ctx, controlURL, env("GATEWAY_ID", ""), os.Getenv("GATEWAY_TOKEN"), manager, logger)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-stop:
		cancel()
		logger.Info("shutdown signal received", "signal", sig.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return err
	}
	logger.Info("gateway shutdown complete")
	return nil
}

func defaultConfigPath() string {
	return filepath.Join(projectRoot(), "data", "gateway-config.json")
}

func projectRoot() string {
	working, err := os.Getwd()
	if err != nil {
		return "."
	}
	root := working
	if _, err := os.Stat(filepath.Join(working, "services", "gateway", "go.mod")); err != nil {
		if _, gatewayModuleErr := os.Stat(filepath.Join(working, "go.mod")); gatewayModuleErr == nil {
			root = filepath.Clean(filepath.Join(working, "..", ".."))
		}
	}
	return root
}

func watchConfigFile(ctx context.Context, path string, manager *gateway.Manager, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			signature := string(data)
			if signature == last {
				continue
			}
			var snapshot gateway.Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				logger.Warn("gateway config file is invalid; retaining active configuration", "error", err.Error())
				if restoreErr := manager.RestoreCurrent(); restoreErr != nil {
					logger.Error("could not restore last-known-good configuration file", "error", restoreErr.Error())
				}
				continue
			}
			if err := manager.Apply(snapshot); err != nil {
				logger.Warn("gateway config rejected; retaining active configuration", "error", err.Error())
				if restoreErr := manager.RestoreCurrent(); restoreErr != nil {
					logger.Error("could not restore last-known-good configuration file", "error", restoreErr.Error())
				}
				continue
			}
			last = signature
			logger.Info("configuration file activated", "version", snapshot.Version)
		}
	}
}

func pollConfiguration(ctx context.Context, controlURL, gatewayID, token string, manager *gateway.Manager, logger *slog.Logger) {
	if gatewayID == "" || token == "" {
		logger.Error("control-plane polling disabled: gateway identity or token is missing")
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	etag := ""
	for {
		pollOnce(ctx, client, controlURL, gatewayID, token, manager, &etag, logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func pollOnce(ctx context.Context, client *http.Client, controlURL, gatewayID, token string, manager *gateway.Manager, etag *string, logger *slog.Logger) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, controlURL+"/internal/gateway/config", nil)
	if err != nil {
		logger.Error("build config poll request failed")
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Gateway-ID", gatewayID)
	if *etag != "" {
		request.Header.Set("If-None-Match", *etag)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("configuration poll failed; retaining last known good configuration", "error", err.Error())
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return
	}
	if response.StatusCode != http.StatusOK {
		logger.Warn("configuration poll rejected by control plane", "status", response.StatusCode)
		return
	}
	var snapshot gateway.Snapshot
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&snapshot); err != nil {
		logger.Error("decode configuration snapshot failed", "error", err.Error())
		return
	}
	if err := manager.Apply(snapshot); err != nil {
		logger.Error("configuration snapshot rejected; retaining last known good configuration", "version", snapshot.Version, "error", err.Error())
		ackConfiguration(ctx, client, controlURL, gatewayID, token, snapshot.Version, "REJECTED", err.Error(), logger)
		return
	}
	*etag = response.Header.Get("ETag")
	if *etag == "" {
		*etag = "\"" + snapshot.Version + "\""
	}
	logger.Info("configuration activated", "version", snapshot.Version)
	ackConfiguration(ctx, client, controlURL, gatewayID, token, snapshot.Version, "APPLIED", "", logger)
}

func ackConfiguration(ctx context.Context, client *http.Client, controlURL, gatewayID, token, version, status, message string, logger *slog.Logger) {
	versionNumber, err := strconv.ParseUint(version, 10, 64)
	if err != nil || versionNumber == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"version": versionNumber, "status": status, "message": message})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL+"/internal/gateway/config/ack", bytes.NewReader(body))
	if err != nil {
		logger.Error("build configuration acknowledgement failed")
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Gateway-ID", gatewayID)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("configuration acknowledgement failed", "version", version)
		}
		return
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		logger.Warn("configuration acknowledgement rejected", "version", version, "status", response.StatusCode)
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New(key + " must be a positive duration such as 10s")
	}
	return duration, nil
}

func logLevel() slog.Level {
	switch strings.ToLower(env("GATEKEEPER_LOG_LEVEL", "info")) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

