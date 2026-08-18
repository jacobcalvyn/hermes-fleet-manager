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
	if progress[1].VerificationURI != codexDeviceURL || progress[1].UserCode != "ABCD-EFGH" || !progress[1].ExpiresAt.IsZero() {
		t.Fatalf("device progress=%+v", progress[1])
	}
	joined := strings.Join(authArgs, " ")
	if !strings.Contains(joined, "exec -T -e PYTHONUNBUFFERED=1 hermes hermes auth add openai-codex --no-browser --timeout 900") {
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

func TestAuthenticateGrokReportsAllowlistedDeviceURL(t *testing.T) {
	p, payload := newCodexAuthProvisioner(t)
	payload.Provider = "xai-oauth"
	p.dockerRun = wrapAuthStatus(p.dockerRun, "xai-oauth: logged in\n")
	var authArgs []string
	p.authRun = func(ctx context.Context, args []string, onLine func(string) error) error {
		authArgs = append([]string(nil), args...)
		for _, line := range []string{"1. Open: https://auth.x.ai/oauth2/device?user_code=WXYZ-1234", "Enter this code:", "WXYZ-1234"} {
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
		t.Fatalf("authenticateCodex(xai-oauth) result=%+v", result)
	}
	if !strings.Contains(strings.Join(authArgs, " "), "hermes auth add xai-oauth --no-browser") {
		t.Fatalf("authentication command=%q", authArgs)
	}
	if len(progress) < 2 || progress[1].VerificationURI != "https://auth.x.ai/oauth2/device?user_code=WXYZ-1234" || progress[1].UserCode != "WXYZ-1234" || !progress[1].ExpiresAt.IsZero() {
		t.Fatalf("device progress=%+v", progress)
	}
}

func TestAuthenticateGrokParsesInlineHermesDeviceCode(t *testing.T) {
	p, payload := newCodexAuthProvisioner(t)
	payload.Provider = "xai-oauth"
	p.dockerRun = wrapAuthStatus(p.dockerRun, "xai-oauth: logged in\n")
	p.authRun = func(ctx context.Context, args []string, onLine func(string) error) error {
		for _, line := range []string{
			"To continue:",
			"  1. Open: https://auth.x.ai/oauth2/device?user_code=WXYZ-1234",
			"  2. If prompted, enter code: WXYZ-1234",
			"Waiting for approval (polling every 5s)...",
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
		t.Fatalf("authenticateCodex(xai-oauth inline) result=%+v", result)
	}
	if len(progress) < 2 || progress[1].Stage != "AWAITING_USER" || progress[1].UserCode != "WXYZ-1234" {
		t.Fatalf("inline Grok device progress=%+v", progress)
	}
	if progress[1].VerificationURI != "https://auth.x.ai/oauth2/device?user_code=WXYZ-1234" {
		t.Fatalf("Grok verification URI=%q", progress[1].VerificationURI)
	}
}

func TestAuthenticateGrokRejectsDeviceCodeWithoutHermesVerificationURL(t *testing.T) {
	p, payload := newCodexAuthProvisioner(t)
	payload.Provider = "xai-oauth"
	p.dockerRun = wrapAuthStatus(p.dockerRun, "xai-oauth: logged in\n")
	p.authRun = func(ctx context.Context, args []string, onLine func(string) error) error {
		return onLine("If prompted, enter code: WXYZ-1234")
	}
	result := p.authenticateCodex(context.Background(), payload, func(_ context.Context, update domain.JobProgress) error {
		return nil
	})
	if result.Success || !strings.Contains(result.Error, "safe verification URL") {
		t.Fatalf("authenticateCodex(xai-oauth missing URL) result=%+v", result)
	}
}

func TestObserveGrokAuthUsesProviderCheckName(t *testing.T) {
	p := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		return "xai-oauth: logged out (No Grok credentials stored.)\n", nil
	}}
	builder := &observationBuilder{}
	p.observeProviderAuth(context.Background(), "xai-oauth", observedContainer{ID: "aaaaaaaaaaaa"}, builder, true)
	if len(builder.checks) != 1 || builder.checks[0].Name != "provider_auth" || builder.checks[0].Status != domain.ObservationCheckDrift {
		t.Fatalf("Grok auth observation=%+v", builder.checks)
	}
}

func TestObserveCodexModelCatalogUsesActiveHermesCatalog(t *testing.T) {
	p := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		if !slicesContain(args, providerModelCatalogProbe) || !slicesContain(args, "openai-codex") {
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

func TestObserveInactiveOAuthLogoutDoesNotDegradeRuntime(t *testing.T) {
	p := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		if slicesContain(args, "xai-oauth") && slicesContain(args, "status") {
			return "xai-oauth: logged out (No Grok credentials stored.)\n", nil
		}
		if slicesContain(args, "openai-codex") && slicesContain(args, "status") {
			return "openai-codex: logged in\n", nil
		}
		if slicesContain(args, providerModelCatalogProbe) {
			if slicesContain(args, "xai-oauth") {
				return `{"models":["grok-4.6"],"recommended":"grok-4.6"}`, nil
			}
			return `{"models":["gpt-5.6-sol"],"recommended":"gpt-5.6-sol"}`, nil
		}
		return "", errors.New("unexpected Docker command: " + strings.Join(args, " "))
	}}
	builder := &observationBuilder{}
	hermes := observedContainer{ID: "aaaaaaaaaaaa"}
	p.observeProviderModelCatalog(context.Background(), "openai-codex", hermes, builder)
	p.observeProviderAuth(context.Background(), "openai-codex", hermes, builder, true)
	p.observeProviderModelCatalog(context.Background(), "xai-oauth", hermes, builder)
	p.observeProviderAuth(context.Background(), "xai-oauth", hermes, builder, false)
	if builder.drift {
		t.Fatalf("inactive Grok logout marked the observation as drift: %+v", builder.checks)
	}
	if len(builder.checks) != 2 || builder.checks[0].Name != "codex_auth" || builder.checks[0].Status != domain.ObservationCheckOK ||
		builder.checks[1].Name != "provider_auth" || builder.checks[1].Status != domain.ObservationCheckDrift {
		t.Fatalf("auth checks=%+v", builder.checks)
	}
	report := builder.finish(domain.ObservationTarget{Provider: "openai-codex"})
	if report.RecommendedModel != "gpt-5.6-sol" || len(report.ProviderModelCatalogs) != 2 {
		t.Fatalf("observation catalogs=%+v recommended=%q", report.ProviderModelCatalogs, report.RecommendedModel)
	}
	if _, ok := report.ProviderModelCatalogs["xai-oauth"]; !ok {
		t.Fatalf("missing Grok catalog: %+v", report.ProviderModelCatalogs)
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

func wrapAuthStatus(current func(context.Context, ...string) (string, error), status string) func(context.Context, ...string) (string, error) {
	return func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "exec" && slicesContain(args, "status") {
			return status, nil
		}
		if current != nil {
			return current(ctx, args...)
		}
		return "", errors.New("unexpected Docker command: " + strings.Join(args, " "))
	}
}
