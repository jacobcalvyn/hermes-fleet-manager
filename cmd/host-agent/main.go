package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/agent"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/provisioner"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/rotatinglog"
)

const (
	probeAuthenticationRejectedExitCode = 10
	enrollmentConflictExitCode          = 11
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "enroll":
		enroll(os.Args[2:])
	case "recover":
		recover(os.Args[2:])
	case "run":
		run(os.Args[2:])
	case "validate":
		validate(os.Args[2:])
	case "probe":
		probe(os.Args[2:])
	default:
		usage()
	}
}

func enroll(args []string) {
	flags := flag.NewFlagSet("enroll", flag.ExitOnError)
	url := flags.String("url", "http://127.0.0.1:9180", "Fleet Manager URL")
	token := flags.String("token", "", "local enrollment token")
	tokenStdin := flags.Bool("token-stdin", false, "read the local enrollment token from stdin")
	name := flags.String("name", "local-mac", "host name")
	managedRoot := flags.String("managed-root", defaultManagedRoot(), "managed instance root")
	configPath := flags.String("config", defaultConfigPath(), "agent config path")
	flags.Parse(args)
	enrollmentToken := strings.TrimSpace(*token)
	if *tokenStdin {
		if enrollmentToken != "" {
			fatal("--token and --token-stdin cannot be used together")
		}
		var err error
		enrollmentToken, err = readSecretFromStdin(os.Stdin, "enrollment token")
		if err != nil {
			fatal(err.Error())
		}
	}
	if enrollmentToken == "" {
		fatal("--token or --token-stdin is required")
	}
	hostname, _ := os.Hostname()
	enrollment, err := agent.Enroll(context.Background(), *url, enrollmentToken, *name, hostname)
	if err != nil {
		if agent.IsHTTPStatus(err, http.StatusConflict) {
			fatalWithCode(enrollmentConflictExitCode, err.Error())
		}
		fatal(err.Error())
	}
	config := agent.Config{
		ControlPlaneURL: *url, HostID: enrollment.HostID, HostToken: enrollment.HostToken,
		Name: *name, Hostname: hostname, ManagedRoot: *managedRoot,
	}
	if err := writeConfig(*configPath, config); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("Host enrolled as %s. Credential written to %s\n", *name, *configPath)
}

func recover(args []string) {
	flags := flag.NewFlagSet("recover", flag.ExitOnError)
	url := flags.String("url", "http://127.0.0.1:9180", "Fleet Manager URL")
	name := flags.String("name", "local-mac", "exact enrolled host name")
	managedRoot := flags.String("managed-root", defaultManagedRoot(), "managed instance root")
	configPath := flags.String("config", defaultConfigPath(), "agent config path")
	adminTokenStdin := flags.Bool("admin-token-stdin", false, "read the Fleet admin token from stdin")
	flags.Parse(args)

	adminToken, err := readAdminToken(os.Stdin, os.Getenv("FLEET_ADMIN_TOKEN"), *adminTokenStdin)
	if err != nil {
		fatal(err.Error())
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		fatal("could not determine the local hostname")
	}
	enrollment, err := agent.Recover(context.Background(), *url, adminToken, *name, hostname)
	if err != nil {
		switch {
		case agent.IsHTTPStatus(err, 404):
			fatal("host credential recovery failed: the exact host name is not enrolled")
		case agent.IsHTTPStatus(err, 409):
			fatal("host credential recovery was rejected: host identity does not match or the host has active jobs")
		default:
			fatal(err.Error())
		}
	}
	config := agent.Config{
		ControlPlaneURL: *url, HostID: enrollment.HostID, HostToken: enrollment.HostToken,
		Name: *name, Hostname: hostname, ManagedRoot: *managedRoot,
	}
	if err := writeConfig(*configPath, config); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("Host credential recovered for %s. Credential written to %s\n", *name, *configPath)
}

func readAdminToken(reader io.Reader, environmentToken string, fromStdin bool) (string, error) {
	if !fromStdin {
		token := strings.TrimSpace(environmentToken)
		if token == "" {
			return "", errors.New("set FLEET_ADMIN_TOKEN or use --admin-token-stdin")
		}
		return token, nil
	}
	return readSecretFromStdin(reader, "Fleet admin token")
}

func readSecretFromStdin(reader io.Reader, label string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil {
		return "", fmt.Errorf("read %s from stdin: %w", label, err)
	}
	if len(data) > 4096 {
		return "", fmt.Errorf("%s from stdin is too large", label)
	}
	token := strings.TrimSpace(string(data))
	for index := range data {
		data[index] = 0
	}
	if token == "" {
		return "", fmt.Errorf("%s from stdin is empty", label)
	}
	return token, nil
}

func run(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "agent config path")
	dockerPath := flags.String("docker", "docker", "Docker CLI path")
	logPath := flags.String("log-path", defaultLogPath(), "rotating agent log path")
	logMaxBytes := flags.Int64("log-max-bytes", 25*1024*1024, "maximum bytes per agent log file")
	logMaxFiles := flags.Int("log-max-files", 4, "maximum retained agent log files")
	shutdownGracePeriod := flags.Duration(
		"shutdown-grace-period",
		agent.DefaultShutdownGracePeriod,
		"maximum time to drain active jobs before cancellation",
	)
	flags.Parse(args)
	output, err := rotatinglog.Open(*logPath, *logMaxBytes, *logMaxFiles)
	if err != nil {
		fatal(err.Error())
	}
	defer output.Close()
	config, err := readConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(output, "host-agent: %v\n", err)
		fatal(err.Error())
	}
	config.ShutdownGracePeriod = *shutdownGracePeriod
	executor, err := provisioner.New(config.ManagedRoot, *dockerPath)
	if err != nil {
		_, _ = fmt.Fprintf(output, "host-agent: %v\n", err)
		fatal(err.Error())
	}
	client := agent.NewWithOutput(config, executor, output)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	_, _ = fmt.Fprintf(output, "Host agent %s connected to %s\n", config.Name, config.ControlPlaneURL)
	if err := client.Run(ctx); err != nil && err != context.Canceled {
		_, _ = fmt.Fprintf(output, "host-agent: %v\n", err)
		fatal(err.Error())
	}
}

func defaultLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".state", "host-agent.log")
	}
	return filepath.Join(home, ".local", "state", "hermes-fleet", "host-agent.log")
}

func validate(args []string) {
	flags := flag.NewFlagSet("validate", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "agent config path")
	flags.Parse(args)
	config, err := readConfig(*configPath)
	if err != nil {
		fatal(err.Error())
	}
	if err := validateConfig(config); err != nil {
		fatal(err.Error())
	}
}

func probe(args []string) {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "agent config path")
	expectedURL := flags.String("url", "", "expected Fleet Manager URL")
	expectedName := flags.String("name", "", "expected host name")
	expectedRoot := flags.String("managed-root", "", "expected managed instance root")
	timeout := flags.Duration("timeout", 20*time.Second, "probe timeout")
	flags.Parse(args)
	config, err := readConfig(*configPath)
	if err != nil {
		fatal(err.Error())
	}
	if err := validateConfig(config); err != nil {
		fatal(err.Error())
	}
	if *expectedURL != "" && config.ControlPlaneURL != *expectedURL {
		fatal("agent config belongs to a different control plane URL")
	}
	if *expectedName != "" && config.Name != *expectedName {
		fatal("agent config belongs to a different host name")
	}
	if *expectedRoot != "" {
		configuredRoot, rootErr := filepath.Abs(config.ManagedRoot)
		wantedRoot, wantedErr := filepath.Abs(*expectedRoot)
		if rootErr != nil || wantedErr != nil || configuredRoot != wantedRoot {
			fatal("agent config uses a different managed root")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := agent.New(config, nil).Probe(ctx); err != nil {
		fatalWithCode(probeFailureExitCode(err), "authenticated control plane probe failed: "+err.Error())
	}
}

func probeFailureExitCode(err error) int {
	if agent.IsHTTPStatus(err, http.StatusUnauthorized) ||
		agent.IsHTTPStatus(err, http.StatusForbidden) {
		return probeAuthenticationRejectedExitCode
	}
	return 1
}

func validateConfig(config agent.Config) error {
	for field, value := range map[string]string{
		"control_plane_url": config.ControlPlaneURL,
		"host_id":           config.HostID,
		"host_token":        config.HostToken,
		"name":              config.Name,
		"managed_root":      config.ManagedRoot,
	} {
		if value == "" {
			return fmt.Errorf("agent config is missing %s", field)
		}
	}
	return nil
}

func writeConfig(path string, config agent.Config) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolute), ".agent-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, absolute); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readConfig(path string) (agent.Config, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return agent.Config{}, err
	}
	if !info.Mode().IsRegular() {
		return agent.Config{}, fmt.Errorf("agent config is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return agent.Config{}, fmt.Errorf("agent config permissions must be 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.Config{}, err
	}
	var config agent.Config
	if err := json.Unmarshal(data, &config); err != nil {
		return config, err
	}
	return config, nil
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hermes-fleet", "agent.json")
}

func defaultManagedRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "hermes-fleet", "instances")
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: hermes-fleet-agent <enroll|recover|run|validate|probe> [flags]")
	os.Exit(2)
}

func fatal(message string) {
	fatalWithCode(1, message)
}

func fatalWithCode(code int, message string) {
	fmt.Fprintln(os.Stderr, "host-agent:", message)
	os.Exit(code)
}
