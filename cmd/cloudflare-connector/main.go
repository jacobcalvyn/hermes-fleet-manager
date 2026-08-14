package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultTokenPath     = "/run/hermes-fleet-cloudflare/token"
	defaultConfigPath    = "/run/hermes-fleet-cloudflare/config.yml"
	defaultHealthAddress = ":9081"
	defaultHealthURL     = "http://127.0.0.1:9081/healthz"
	cloudflaredReadyURL  = "http://127.0.0.1:9082/ready"
	pollInterval         = 2 * time.Second
	restartDelay         = 5 * time.Second
	stopTimeout          = 10 * time.Second
)

type runtimeSpec struct {
	Mode   string
	Path   string
	Digest [sha256.Size]byte
}

type healthSnapshot struct {
	State               string    `json:"state"`
	Ready               bool      `json:"ready"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type connectorHealth struct {
	mu       sync.RWMutex
	snapshot healthSnapshot
}

func newConnectorHealth() *connectorHealth {
	return &connectorHealth{snapshot: healthSnapshot{State: "disabled", Ready: true, UpdatedAt: time.Now().UTC()}}
}

func (health *connectorHealth) set(state string, ready bool, err error) {
	health.mu.Lock()
	defer health.mu.Unlock()
	health.snapshot.State = state
	health.snapshot.Ready = ready
	health.snapshot.UpdatedAt = time.Now().UTC()
	if err != nil {
		health.snapshot.ConsecutiveFailures++
		health.snapshot.LastError = err.Error()
		return
	}
	if ready {
		health.snapshot.ConsecutiveFailures = 0
		health.snapshot.LastError = ""
	}
}

func (health *connectorHealth) value() healthSnapshot {
	health.mu.RLock()
	defer health.mu.RUnlock()
	return health.snapshot
}

func (health *connectorHealth) handler(w http.ResponseWriter, _ *http.Request) {
	snapshot := health.value()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if !snapshot.Ready {
		status = http.StatusServiceUnavailable
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(snapshot)
}

type childProcess interface {
	Signal(os.Signal) error
	Kill() error
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(envOr("FLEET_CONNECTOR_HEALTH_URL", defaultHealthURL)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	health := newConnectorHealth()
	healthServer := &http.Server{
		Addr: envOr("FLEET_CONNECTOR_HEALTH_ADDRESS", defaultHealthAddress), Handler: http.HandlerFunc(health.handler),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("connector health server failed", "error", err)
			stop()
		}
	}()
	defer func() {
		health.set("stopping", false, nil)
		shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdownContext)
	}()
	for ctx.Err() == nil {
		spec, err := readRuntimeSpec(defaultConfigPath, defaultTokenPath)
		if err != nil {
			logger.Warn("read connector runtime", "error", err)
			health.set("retrying", false, err)
			wait(ctx, pollInterval)
			continue
		}
		if spec.Mode == "disabled" {
			health.set("disabled", true, nil)
			wait(ctx, pollInterval)
			continue
		}
		health.set("starting", false, nil)
		logger.Info("starting Cloudflare connector", "mode", spec.Mode)
		args := []string{"tunnel", "--no-autoupdate", "--metrics", "127.0.0.1:9082"}
		if spec.Mode == "local" {
			args = append(args, "--config", spec.Path, "run")
		} else {
			args = append(args, "run", "--token-file", spec.Path)
		}
		command := exec.Command("/usr/local/bin/cloudflared", args...)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			logger.Error("start Cloudflare connector", "error", err)
			health.set("retrying", false, err)
			wait(ctx, restartDelay)
			continue
		}
		exited := make(chan error, 1)
		go func() { exited <- command.Wait() }()
		ticker := time.NewTicker(pollInterval)
		running := true
		for running {
			select {
			case <-ctx.Done():
				stopChild(command.Process, exited, stopTimeout)
				running = false
			case err := <-exited:
				if err != nil && ctx.Err() == nil {
					logger.Warn("Cloudflare connector stopped", "error", err)
					health.set("retrying", false, err)
				} else if ctx.Err() == nil {
					health.set("retrying", false, errors.New("cloudflared exited unexpectedly"))
				}
				running = false
			case <-ticker.C:
				current, readErr := readRuntimeSpec(defaultConfigPath, defaultTokenPath)
				if readErr != nil || current.Mode != spec.Mode || current.Path != spec.Path || current.Digest != spec.Digest {
					logger.Info("Cloudflare connector configuration changed; restarting")
					health.set("starting", false, nil)
					stopChild(command.Process, exited, stopTimeout)
					running = false
					continue
				}
				if err := probeCloudflared(); err != nil {
					health.set("starting", false, nil)
				} else {
					health.set("running", true, nil)
				}
			}
		}
		ticker.Stop()
		if ctx.Err() == nil {
			wait(ctx, restartDelay)
		}
	}
}

func probeCloudflared() error {
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(cloudflaredReadyURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("cloudflared readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func runHealthcheck(endpoint string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("connector health request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("connector health returned HTTP %d", response.StatusCode)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func stopChild(process childProcess, exited <-chan error, timeout time.Duration) {
	if process == nil {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
		_ = process.Kill()
	}
}

func readToken(path string) (string, [sha256.Size]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", [sha256.Size]byte{}, err
	}
	token := strings.TrimSpace(string(data))
	return token, sha256.Sum256([]byte(token)), nil
}

func readRuntimeSpec(configPath, tokenPath string) (runtimeSpec, error) {
	config, err := os.ReadFile(configPath)
	if err == nil {
		if len(strings.TrimSpace(string(config))) == 0 {
			return runtimeSpec{}, errors.New("Cloudflare connector configuration is empty")
		}
		return runtimeSpec{Mode: "local", Path: configPath, Digest: sha256.Sum256(config)}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return runtimeSpec{}, err
	}
	token, digest, err := readToken(tokenPath)
	if err == nil {
		if token == "" {
			return runtimeSpec{}, errors.New("Cloudflare connector token is empty")
		}
		return runtimeSpec{Mode: "token", Path: tokenPath, Digest: digest}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return runtimeSpec{Mode: "disabled"}, nil
	}
	return runtimeSpec{}, err
}

func wait(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
