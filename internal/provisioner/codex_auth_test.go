package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestAuthenticateCodexReportsOnlyDeviceFlowProgressAndVerifiesSession(t *testing.T) {
	p, payload := newCodexAuthProvisioner(t)
	var authArgs []string
	p.authRun = func(ctx context.Context, args []string, onLine func(string) error) error {
		authArgs = append([]string(nil), args...)
		for _, line := range []string{
			"To continue, follow these steps:", codexDeviceURL, "Enter this code:",
			"\x1b[1mABCD-EFGH\x1b[0m", "access_token=must-never-be-reported", "Login successful!",
		} {
			if err := onLine(line); err != nil {
				return err
			}
		}
		return nil
	}
	var progress []domain.JobProgress
	result := p.authenticateCodex(context.Background(), payload, func(_ context.Context, update domain.JobProgress) error {
		progress = append(progress, update)
		return nil
	})
	if !result.Success {
		t.Fatalf("authenticateCodex() result=%+v", result)
	}
	stages := make([]string, 0, len(progress))
	for _, update := range progress {
		stages = append(stages, update.Stage)
		encoded, _ := json.Marshal(update)
		if strings.Contains(string(encoded), "access_token") {
			t.Fatalf("progress exposed command output: %s", encoded)
		}
	}
	if !reflect.DeepEqual(stages, []string{"STARTING", "AWAITING_USER", "VERIFYING"}) {
		t.Fatalf("progress stages=%v", stages)
	}
	if progress[1].VerificationURI != codexDeviceURL || progress[1].UserCode != "ABCD-EFGH" || progress[1].ExpiresAt.IsZero() {
		t.Fatalf("device progress=%+v", progress[1])
	}
	joined := strings.Join(authArgs, " ")
	if !strings.Contains(joined, "exec -T hermes hermes auth add openai-codex --no-browser --timeout 900") {
		t.Fatalf("authentication command=%q", joined)
	}
}

func TestAuthenticateCodexStopsWhenProgressLeaseIsLost(t *testing.T) {
	p, payload := newCodexAuthProvisioner(t)
	p.authRun = func(ctx context.Context, args []string, onLine func(string) error) error {
		if err := onLine("Enter this code:"); err != nil {
			return err
		}
		return onLine("ABCD-EFGH")
	}
	result := p.authenticateCodex(context.Background(), payload, func(_ context.Context, update domain.JobProgress) error {
		if update.Stage == "AWAITING_USER" {
			return errors.New("job lease lost")
		}
		return nil
	})
	if result.Success || !strings.Contains(result.Error, "job lease lost") {
		t.Fatalf("authenticateCodex() result=%+v", result)
	}
}

func TestObserveCodexAuthMarksLoggedOutSessionAsDrift(t *testing.T) {
	p := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		return "openai-codex: logged out (No Codex credentials stored.)\n", nil
	}}
	builder := &observationBuilder{}
	p.observeCodexAuth(context.Background(), observedContainer{ID: "aaaaaaaaaaaa"}, builder)
	if len(builder.checks) != 1 || builder.checks[0].Name != "codex_auth" || builder.checks[0].Status != domain.ObservationCheckDrift {
		t.Fatalf("Codex auth observation=%+v", builder.checks)
	}
}

func TestObserveCodexModelCatalogUsesActiveHermesCatalog(t *testing.T) {
	p := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		if !slicesContain(args, codexModelCatalogProbe) {
			return "", errors.New("unexpected probe")
		}
		return `{"models":["gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-sol","bad model"],"recommended":"gpt-5.6-sol"}`, nil
	}}
	builder := &observationBuilder{}
	p.observeCodexModelCatalog(context.Background(), observedContainer{ID: "aaaaaaaaaaaa"}, builder)
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra"}
	if !reflect.DeepEqual(builder.modelCatalog, want) || builder.recommendedModel != "gpt-5.6-sol" {
		t.Fatalf("model catalog=%v recommended=%q", builder.modelCatalog, builder.recommendedModel)
	}
}

func newCodexAuthProvisioner(t *testing.T) (*Provisioner, domain.CodexAuthPayload) {
	t.Helper()
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "compose.yaml"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	instanceID := "00000000-0000-4000-8000-000000000001"
	project := "hermes-fleet-fleet-test-01-00000000"
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			containers := []map[string]any{
				{"Id": "aaaaaaaaaaaa", "Config": map[string]any{"Labels": map[string]string{"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": instanceID, "com.docker.compose.project": project, "com.docker.compose.service": "hermes"}}, "State": map[string]any{"Status": "running"}},
				{"Id": "bbbbbbbbbbbb", "Config": map[string]any{"Labels": map[string]string{"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": instanceID, "com.docker.compose.project": project, "com.docker.compose.service": "dashboard"}}, "State": map[string]any{"Status": "running"}},
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "exec":
			return "openai-codex: logged in\n", nil
		default:
			return "", errors.New("unexpected Docker command: " + strings.Join(args, " "))
		}
	}
	return p, domain.CodexAuthPayload{InstanceID: instanceID, Name: "fleet-test-01", ProjectName: project, ManagedPath: managedPath}
}
