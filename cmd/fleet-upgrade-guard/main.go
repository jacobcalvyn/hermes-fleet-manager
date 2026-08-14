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
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

const snapshotFormatVersion = 1

type snapshot struct {
	FormatVersion int               `json:"format_version"`
	CreatedAt     time.Time         `json:"created_at"`
	Backup        backup.Metadata   `json:"backup"`
	Instances     []domain.Instance `json:"instances"`
}

type systemInfo struct {
	BuildID string `json:"build_id"`
}

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: fleet-upgrade-guard snapshot|verify [flags]"))
	}
	var err error
	switch os.Args[1] {
	case "snapshot":
		err = runSnapshot(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func runSnapshot(arguments []string) error {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	baseURL := flags.String("url", "", "Fleet Manager URL")
	token := flags.String("token", "", "Fleet admin token")
	outputDirectory := flags.String("output-dir", "", "snapshot output directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*token) == "" || strings.TrimSpace(*outputDirectory) == "" {
		return errors.New("url, token, and output-dir are required")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := requireReady(ctx, client, *baseURL); err != nil {
		return err
	}
	var instances []domain.Instance
	if err := requestJSON(ctx, client, http.MethodGet, endpoint(*baseURL, "/api/v1/instances"), *token, nil, &instances); err != nil {
		return fmt.Errorf("snapshot managed instances: %w", err)
	}
	var metadata backup.Metadata
	if err := requestJSON(ctx, client, http.MethodPost, endpoint(*baseURL, "/api/v1/backups"), *token, nil, &metadata); err != nil {
		return fmt.Errorf("create verified control-plane backup: %w", err)
	}
	if err := os.MkdirAll(*outputDirectory, 0o700); err != nil {
		return fmt.Errorf("create upgrade snapshot directory: %w", err)
	}
	databasePath := filepath.Join(*outputDirectory, "control-plane.sqlite")
	if err := download(ctx, client, endpoint(*baseURL, "/api/v1/backups/"+metadata.ID+"/download"), *token, databasePath); err != nil {
		return fmt.Errorf("download control-plane backup: %w", err)
	}
	if err := verifyMigrationCopy(ctx, databasePath, *outputDirectory); err != nil {
		return fmt.Errorf("candidate database migration preflight: %w", err)
	}
	state := snapshot{FormatVersion: snapshotFormatVersion, CreatedAt: time.Now().UTC(), Backup: metadata, Instances: instances}
	if err := writeJSON(filepath.Join(*outputDirectory, "snapshot.json"), state); err != nil {
		return err
	}
	return nil
}

func runVerify(arguments []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	baseURL := flags.String("url", "", "Fleet Manager URL")
	token := flags.String("token", "", "Fleet admin token")
	snapshotPath := flags.String("snapshot", "", "upgrade snapshot manifest")
	expectedBuildID := flags.String("build-id", "", "expected Fleet build ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*token) == "" || strings.TrimSpace(*expectedBuildID) == "" {
		return errors.New("url, token, and build-id are required")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := requireReady(ctx, client, *baseURL); err != nil {
		return err
	}
	var currentSystem systemInfo
	if err := requestJSON(ctx, client, http.MethodGet, endpoint(*baseURL, "/api/v1/system"), *token, nil, &currentSystem); err != nil {
		return fmt.Errorf("read deployed Fleet identity: %w", err)
	}
	if currentSystem.BuildID != *expectedBuildID {
		return fmt.Errorf("deployed build ID %q does not match candidate %q", currentSystem.BuildID, *expectedBuildID)
	}
	if strings.TrimSpace(*snapshotPath) == "" {
		return nil
	}
	var before snapshot
	if err := readJSON(*snapshotPath, &before); err != nil {
		return err
	}
	if before.FormatVersion != snapshotFormatVersion {
		return errors.New("upgrade snapshot format is unsupported")
	}
	var after []domain.Instance
	if err := requestJSON(ctx, client, http.MethodGet, endpoint(*baseURL, "/api/v1/instances"), *token, nil, &after); err != nil {
		return fmt.Errorf("verify managed instances: %w", err)
	}
	return compareStableInstanceStates(before.Instances, after)
}

func compareStableInstanceStates(before, after []domain.Instance) error {
	current := make(map[string]domain.Instance, len(after))
	for _, instance := range after {
		current[instance.ID] = instance
	}
	for _, instance := range before {
		if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
			continue
		}
		observed, ok := current[instance.ID]
		if !ok {
			return fmt.Errorf("managed instance %s disappeared during upgrade", instance.Name)
		}
		if observed.Status != instance.Status {
			return fmt.Errorf("managed instance %s changed from %s to %s during upgrade", instance.Name, instance.Status, observed.Status)
		}
	}
	return nil
}

func requireReady(ctx context.Context, client *http.Client, baseURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(baseURL, "/readyz"), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Fleet readiness request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Fleet readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func requestJSON(ctx context.Context, client *http.Client, method, url, token string, body io.Reader, target any) error {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func download(ctx context.Context, client *http.Client, url, token, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	temporary := destination + ".tmp"
	defer os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporary, destination)
}

func verifyMigrationCopy(ctx context.Context, source, directory string) error {
	destination := filepath.Join(directory, "migration-preflight.sqlite")
	defer os.Remove(destination)
	if err := copyFile(source, destination); err != nil {
		return err
	}
	dataStore, err := store.Open(destination)
	if err != nil {
		return err
	}
	readyErr := dataStore.Ready(ctx)
	closeErr := dataStore.Close()
	return errors.Join(readyErr, closeErr)
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func endpoint(baseURL, path string) string { return strings.TrimRight(baseURL, "/") + path }

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode upgrade snapshot: %w", err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
