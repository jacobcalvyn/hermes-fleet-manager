package provisioner

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/compatibility"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/mcpconfig"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/messaging"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/providers"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recoverycodec"
	runtimeassets "github.com/jacobcalvyn/hermes-fleet-manager/runtime"
	"golang.org/x/sys/unix"
)

var (
	safeNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
	instanceIDPattern  = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)
	containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	recoveryIDPattern  = regexp.MustCompile(`^recovery-[a-f0-9]{32}$`)
	imageIDPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	sha256HexPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	codexUserCode      = regexp.MustCompile(`^[A-Z0-9]{4,12}(?:-[A-Z0-9]{4,12})?$`)
	hermesVersionRef   = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+){2}(?:[+-][A-Za-z0-9.-]+)?$`)
	hermesSourceRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$`)
	hermesCommitRef    = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)
	runtimeImageRef    = regexp.MustCompile(`^local/hermes-fleet-runtime:([0-9]+\.[0-9]+\.[0-9]+)(?:-([a-f0-9]{12})(?:-([a-f0-9]{12}))?)?$`)
	ansiEscapePattern  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

const (
	codexDeviceURL                 = "https://auth.openai.com/codex/device"
	provisionDashboardReadyTimeout = 150 * time.Second
	dashboardReadinessPollInterval = 2 * time.Second
	hermesChatIdleTimeout          = 90 * time.Second
)

type Provisioner struct {
	root            string
	dockerPath      string
	dockerRun       func(context.Context, ...string) (string, error)
	dockerInputRun  func(context.Context, io.Reader, ...string) (string, error)
	authRun         func(context.Context, []string, func(string) error) error
	httpClient      *http.Client
	chatIdleTimeout time.Duration
	portCheck       func(int) error
	recoveryBlocks  map[string]string
	diskAvailable   func(string) (uint64, error)
	volumeSize      func(context.Context, string, string, int64) (int64, error)
	imageBuildMu    sync.Mutex
}

type observedContainer struct {
	ID         string `json:"Id"`
	Image      string `json:"Image"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status string `json:"Status"`
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

type observationBuilder struct {
	checks           []domain.ObservationCheck
	hermesVersion    string
	hermesSource     string
	modelCatalog     []string
	recommendedModel string
	missing          bool
	drift            bool
	unknown          bool
}

func (builder *observationBuilder) add(name, status, detail string) {
	builder.checks = append(builder.checks, domain.ObservationCheck{Name: name, Status: status, Detail: detail})
	if status == domain.ObservationCheckMissing {
		builder.missing = true
	}
	if status == domain.ObservationCheckDrift {
		builder.drift = true
	}
	if status == domain.ObservationCheckUnknown {
		builder.unknown = true
	}
}

func (builder *observationBuilder) finish(target domain.ObservationTarget) domain.InstanceObservation {
	status, summary := domain.ObservationInSync, "Runtime matches desired state"
	if builder.missing {
		status, summary = domain.ObservationMissing, "One or more Fleet-owned runtime resources are missing"
	} else if builder.drift {
		status, summary = domain.ObservationDegraded, "Runtime drift detected"
	} else if builder.unknown {
		status, summary = domain.ObservationUnknown, "Runtime state could not be fully verified"
	}
	return domain.InstanceObservation{
		InstanceID: target.InstanceID, TargetGeneration: target.Generation, RefreshRequestID: target.RefreshRequestID,
		HermesVersion: builder.hermesVersion, HermesSource: builder.hermesSource,
		ModelCatalog: builder.modelCatalog, RecommendedModel: builder.recommendedModel,
		Status: status, Summary: summary, Checks: builder.checks, ObservedAt: time.Now().UTC(),
	}
}

func unknownObservation(target domain.ObservationTarget, detail string, checks []domain.ObservationCheck) domain.InstanceObservation {
	if len(checks) == 0 {
		checks = []domain.ObservationCheck{{Name: "observation", Status: domain.ObservationCheckUnknown, Detail: detail}}
	}
	return domain.InstanceObservation{
		InstanceID: target.InstanceID, TargetGeneration: target.Generation, RefreshRequestID: target.RefreshRequestID,
		Status: domain.ObservationUnknown, Summary: detail, Checks: checks, ObservedAt: time.Now().UTC(),
	}
}

func New(root, dockerPath string) (*Provisioner, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve managed root: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create managed root: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed root must be a real directory")
	}
	recoveryBlocks, err := cleanupRecoveryStaging(absoluteRoot)
	if err != nil {
		return nil, err
	}
	if dockerPath == "" {
		dockerPath = "docker"
	}
	return &Provisioner{
		root: absoluteRoot, dockerPath: dockerPath,
		httpClient: &http.Client{Timeout: 5 * time.Second}, chatIdleTimeout: hermesChatIdleTimeout, portCheck: checkPort,
		recoveryBlocks: recoveryBlocks,
	}, nil
}

func cleanupRecoveryStaging(root string) (map[string]string, error) {
	recoveryBlocks := make(map[string]string)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery staging files: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		fullPath := filepath.Join(root, name)
		switch {
		case strings.HasPrefix(name, ".recovery-volume-"), strings.HasPrefix(name, ".recovery-point-"):
			if entry.IsDir() || !strings.HasSuffix(name, ".enc") {
				return nil, errors.New("unexpected recovery staging path")
			}
			if err := os.Remove(fullPath); err != nil {
				return nil, fmt.Errorf("remove stale encrypted recovery staging file: %w", err)
			}
		case strings.HasPrefix(name, ".restore-stage-"):
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil, errors.New("unexpected restore staging path")
			}
			if err := os.RemoveAll(fullPath); err != nil {
				return nil, fmt.Errorf("remove stale restore staging directory: %w", err)
			}
		case strings.HasPrefix(name, ".restore-volume-backup-"):
			info, statErr := os.Lstat(fullPath)
			if statErr != nil || !info.Mode().IsRegular() || !strings.HasSuffix(name, ".tar") {
				return nil, errors.New("unexpected restore volume rollback path")
			}
			if err := os.Remove(fullPath); err != nil {
				return nil, fmt.Errorf("remove stale restore volume rollback copy: %w", err)
			}
		case strings.HasPrefix(name, ".") && strings.Contains(name, ".restore-backup-"):
			separator := strings.LastIndex(name, ".restore-backup-")
			managedName := strings.TrimPrefix(name[:separator], ".")
			if managedName == "" || strings.ContainsRune(managedName, os.PathSeparator) {
				return nil, fmt.Errorf("unexpected interrupted restore workspace %q", name)
			}
			recoveryBlocks[managedName] = name
		}
	}
	return recoveryBlocks, nil
}

func (p *Provisioner) Execute(ctx context.Context, job domain.Job) domain.JobResult {
	return p.ExecuteWithProgress(ctx, job, nil)
}

func (p *Provisioner) ExecuteWithProgress(ctx context.Context, job domain.Job, report func(context.Context, domain.JobProgress) error) domain.JobResult {
	switch job.Type {
	case "instance.provision":
		var payload domain.ProvisionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid provision payload", err)
		}
		return p.provision(ctx, payload)
	case "instance.start", "instance.stop", "instance.delete":
		var payload domain.ActionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid lifecycle payload", err)
		}
		return p.lifecycle(ctx, job.Type, payload)
	case "instance.runtime.repair":
		var payload domain.RuntimeRepairPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid runtime repair payload", err)
		}
		return p.repairRuntime(ctx, payload)
	case "instance.image.reconcile":
		var payload domain.ImageReconcilePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid image reconciliation payload", err)
		}
		return p.reconcileImage(ctx, payload)
	case "instance.image.repair":
		var payload domain.ImageRepairPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid image repair payload", err)
		}
		return p.repairImage(ctx, payload)
	case "instance.runtime.sync", "instance.runtime.configure":
		var payload domain.RuntimeSyncPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid runtime synchronization payload", err)
		}
		return p.syncRuntimeConfiguration(ctx, payload)
	case "instance.messaging.configure":
		var payload domain.MessagingApplyPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid messaging configuration payload", err)
		}
		return p.configureMessaging(ctx, payload, job.InputSecret)
	case "instance.mcp.configure":
		var payload domain.MCPApplyPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid MCP configuration payload", err)
		}
		return p.configureMCP(ctx, payload, job.InputSecret, report)
	case "instance.hermes.upgrade":
		var payload domain.HermesUpgradePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return domain.JobResult{Success: false, Error: "invalid Hermes update payload: " + err.Error(), InstanceStatus: domain.InstanceStopped}
		}
		return p.upgradeHermes(ctx, payload, job.InputArtifact)
	case "instance.hermes.prepare":
		var payload domain.HermesUpgradePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid Hermes release preparation payload", err)
		}
		imageID, err := p.prepareHermesImage(ctx, payload)
		if err != nil {
			return failure("Hermes release preparation failed", err)
		}
		return domain.JobResult{Success: true, Message: "Hermes release image is ready", ImageID: imageID}
	case "instance.credentials.inspect":
		var payload domain.ActionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid credential inspection payload", err)
		}
		return p.inspectCredentials(payload)
	case "instance.recovery.create":
		var payload domain.RecoveryPointPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid recovery point payload", err)
		}
		return p.createRecoveryPoint(ctx, payload)
	case "instance.recovery.restore":
		var payload domain.RecoveryRestorePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return domain.JobResult{Success: false, Error: "invalid recovery restore payload: " + err.Error(), InstanceStatus: domain.InstanceStopped}
		}
		return p.restoreRecoveryPoint(ctx, payload, job.InputArtifact)
	case "instance.auth.codex":
		var payload domain.CodexAuthPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid Codex authentication payload", err)
		}
		return p.authenticateCodex(ctx, payload, report)
	case "instance.chat.send":
		var payload domain.ChatSendPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return failure("invalid chat payload", err)
		}
		return p.sendChatMessage(ctx, payload, job.InputSecret)
	default:
		return domain.JobResult{Success: false, Error: "job type is not allowlisted: " + job.Type}
	}
}

func (p *Provisioner) sendChatMessage(ctx context.Context, payload domain.ChatSendPayload, input []byte) domain.JobResult {
	if payload.InstanceID == "" || payload.SessionID == "" || payload.MessageID == "" || payload.APIPort < 1 || payload.APIPort > 65535 {
		return domain.JobResult{Success: false, Error: "chat payload is incomplete"}
	}
	if err := providers.ValidateRuntime(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier); err != nil {
		return domain.JobResult{Success: false, Error: "chat session configuration is invalid: " + err.Error()}
	}
	if len(input) == 0 || len(input) > maximumChatInputBytes || !utf8.Valid(input) {
		return domain.JobResult{Success: false, Error: "chat input is empty, too large, or invalid UTF-8"}
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	apiKey, err := readManagedEnvValue(filepath.Join(managedPath, ".env"), "API_SERVER_KEY")
	if err != nil {
		return failure("read Hermes API credential", err)
	}
	defer func() { apiKey = "" }()
	client := *p.httpClient
	client.Timeout = 10 * time.Minute
	hermesSessionID := "fleet-" + payload.SessionID
	if err := ensureHermesChatSession(ctx, &client, payload.APIPort, apiKey, hermesSessionID, payload); err != nil {
		return failure("ensure Hermes chat session", err)
	}
	requestBody, err := hermesChatRequestBody(payload, input)
	if err != nil {
		return failure("encode Hermes chat request", err)
	}
	defer clearSensitiveBytes(requestBody)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		hermesSessionChatURL(payload.APIPort, hermesSessionID, false), bytes.NewReader(requestBody))
	if err != nil {
		return failure("create Hermes chat request", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return domain.JobResult{Success: false, Error: "Hermes chat was canceled after the job lease ended"}
		}
		return domain.JobResult{Success: false, Error: "Hermes chat request failed: " + err.Error()}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumHermesChatResponseBytes+1))
	if err != nil {
		return failure("read Hermes chat response", err)
	}
	defer clearSensitiveBytes(responseBody)
	if len(responseBody) > maximumHermesChatResponseBytes {
		return domain.JobResult{Success: false, Error: "Hermes chat response exceeded 1 MiB"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return domain.JobResult{Success: false, Error: hermesChatHTTPError(response.StatusCode, responseBody)}
	}
	var completion struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes returned an invalid session chat completion"}
	}
	content := strings.TrimSpace(completion.Message.Content)
	if content == "" || !utf8.ValidString(content) {
		return domain.JobResult{Success: false, Error: "Hermes returned an empty or invalid chat message"}
	}
	content, artifacts := p.prepareChatArtifacts(ctx, payload, content, 0)
	return domain.JobResult{
		Success: true, Message: "Hermes chat completed", ChatMessage: content, ChatArtifacts: artifacts,
	}
}

func (p *Provisioner) ExecuteChatStream(
	ctx context.Context,
	job domain.Job,
	report func(context.Context, domain.ChatStreamEvent) error,
) domain.JobResult {
	if job.Type != "instance.chat.send" {
		return domain.JobResult{Success: false, Error: "job type does not support chat streaming"}
	}
	var payload domain.ChatSendPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return failure("invalid chat payload", err)
	}
	return p.sendChatMessageStream(ctx, payload, job.InputSecret, job.Attempts, report)
}

func (p *Provisioner) sendChatMessageStream(
	ctx context.Context,
	payload domain.ChatSendPayload,
	input []byte,
	attempts int,
	report func(context.Context, domain.ChatStreamEvent) error,
) domain.JobResult {
	if payload.InstanceID == "" || payload.SessionID == "" || payload.MessageID == "" || payload.APIPort < 1 || payload.APIPort > 65535 {
		return domain.JobResult{Success: false, Error: "chat payload is incomplete"}
	}
	if err := providers.ValidateRuntime(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier); err != nil {
		return domain.JobResult{Success: false, Error: "chat session configuration is invalid: " + err.Error()}
	}
	if len(input) == 0 || len(input) > maximumChatInputBytes || !utf8.Valid(input) {
		return domain.JobResult{Success: false, Error: "chat input is empty, too large, or invalid UTF-8"}
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	apiKey, err := readManagedEnvValue(filepath.Join(managedPath, ".env"), "API_SERVER_KEY")
	if err != nil {
		return failure("read Hermes API credential", err)
	}
	defer func() { apiKey = "" }()
	if report == nil {
		return domain.JobResult{Success: false, Error: "chat stream reporter is unavailable"}
	}
	client := *p.httpClient
	client.Timeout = 30 * time.Second
	hermesSessionID := "fleet-" + payload.SessionID
	if err := ensureHermesChatSession(ctx, &client, payload.APIPort, apiKey, hermesSessionID, payload); err != nil {
		return failure("ensure Hermes chat session", err)
	}
	// A streamed Hermes turn may legitimately run longer than a fixed request
	// timeout while tools continue making progress. The stream context is
	// bounded by the lease and the idle guard below, so an overall Client.Timeout
	// would incorrectly terminate healthy long-running turns.
	client.Timeout = 0
	requestBody, err := hermesChatRequestBody(payload, input)
	if err != nil {
		return failure("encode Hermes chat request", err)
	}
	defer clearSensitiveBytes(requestBody)
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	idleTimeout := p.chatIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = hermesChatIdleTimeout
	}
	idleGuard := newChatIdleGuard(idleTimeout, cancelStream)
	defer idleGuard.Stop()
	request, err := http.NewRequestWithContext(streamCtx, http.MethodPost,
		hermesSessionChatURL(payload.APIPort, hermesSessionID, true), bytes.NewReader(requestBody))
	if err != nil {
		return failure("create Hermes chat request", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		if idleGuard.Expired() {
			return domain.JobResult{Success: false, Error: fmt.Sprintf("Hermes chat produced no progress for %s", idleTimeout)}
		}
		if ctx.Err() != nil {
			return domain.JobResult{Success: false, Error: "Hermes chat was canceled after the job lease ended"}
		}
		return domain.JobResult{Success: false, Error: "Hermes chat request failed: " + err.Error()}
	}
	defer response.Body.Close()
	idleGuard.Touch()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maximumHermesChatResponseBytes+1))
		if readErr != nil {
			return failure("read Hermes chat response", readErr)
		}
		defer clearSensitiveBytes(responseBody)
		return domain.JobResult{Success: false, Error: hermesChatHTTPError(response.StatusCode, responseBody)}
	}
	sequence := int64(0)
	if attempts > 0 {
		sequence = int64(attempts) << 32
	}
	sequence++
	if err := report(ctx, domain.ChatStreamEvent{Sequence: sequence, Type: domain.ChatEventStarted}); err != nil {
		return domain.JobResult{Success: false, Error: "chat stream start could not be recorded: " + err.Error()}
	}
	content, sequence, err := consumeHermesChatStream(streamCtx,
		&chatProgressReader{reader: response.Body, progress: idleGuard.Touch}, sequence, report)
	if err != nil {
		if idleGuard.Expired() {
			return domain.JobResult{Success: false, Error: fmt.Sprintf("Hermes chat produced no progress for %s", idleTimeout)}
		}
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	content = strings.TrimSpace(content)
	if content == "" || !utf8.ValidString(content) {
		return domain.JobResult{Success: false, Error: "Hermes returned an empty or invalid chat message"}
	}
	content, artifacts := p.prepareChatArtifacts(ctx, payload, content, sequence)
	return domain.JobResult{
		Success: true, Message: "Hermes chat completed", ChatMessage: content, ChatArtifacts: artifacts,
	}
}

type chatArtifactCandidate struct {
	SourcePath string
	Name       string
	Kind       string
	MediaType  string
}

var chatArtifactMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".pdf":  "application/pdf",
	".csv":  "text/csv",
	".txt":  "text/plain",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".mp4":  "video/mp4",
	".webm": "video/webm",
}

const chatArtifactReadScript = `import os, stat, sys
path = os.path.normpath(sys.argv[1])
limit = int(sys.argv[2])
parts = [part for part in path.split('/') if part]
allowed = path.startswith('/data/cache/') or path.startswith('/workspace/') or os.path.dirname(path) == '/root'
if not allowed or any(part.startswith('.') for part in parts):
    raise SystemExit(20)
if os.path.realpath(path) != path:
    raise SystemExit(21)
fd = os.open(path, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
try:
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_size < 1 or info.st_size > limit:
        raise SystemExit(22)
    while True:
        chunk = os.read(fd, 65536)
        if not chunk:
            break
        sys.stdout.buffer.write(chunk)
finally:
    os.close(fd)
`

func (p *Provisioner) prepareChatArtifacts(
	ctx context.Context,
	payload domain.ChatSendPayload,
	content string,
	sequence int64,
) (string, []domain.ChatArtifactUpload) {
	cleaned, candidates := discoverChatArtifactCandidates(content)
	if len(candidates) == 0 {
		return content, nil
	}
	_, hermesContainer, targetErr := p.validateChatArtifactTarget(ctx, payload)
	artifacts := make([]domain.ChatArtifactUpload, 0, len(candidates))
	for index, candidate := range candidates {
		digest := sha256.Sum256([]byte(payload.SessionID + "\x00" + payload.MessageID + "\x00" + candidate.SourcePath))
		upload := domain.ChatArtifactUpload{
			Sequence:   sequence + int64(index*2) + 1,
			SourcePath: candidate.SourcePath,
			Artifact: domain.ChatArtifact{
				ID: "artifact-" + hex.EncodeToString(digest[:16]), Name: candidate.Name,
				Kind: candidate.Kind, MediaType: candidate.MediaType, Status: "preparing", SourceTool: "file_output",
			},
		}
		if targetErr != nil {
			upload.Error = "The output file is unavailable from the managed Hermes instance."
			upload.Artifact.Status = "missing"
			upload.Artifact.Error = upload.Error
			artifacts = append(artifacts, upload)
			continue
		}
		localPath, size, sha256Hex, err := p.stageChatArtifact(ctx, hermesContainer, candidate)
		if err != nil {
			upload.Error = "The output file is missing, unsafe, or could not be read."
			upload.Artifact.Status = "missing"
			upload.Artifact.Error = upload.Error
		} else {
			upload.LocalPath = localPath
			upload.Artifact.SizeBytes = size
			upload.Artifact.SHA256 = sha256Hex
		}
		artifacts = append(artifacts, upload)
	}
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		if len(artifacts) == 1 {
			cleaned = "Generated 1 output."
		} else {
			cleaned = fmt.Sprintf("Generated %d outputs.", len(artifacts))
		}
	}
	return cleaned, artifacts
}

func discoverChatArtifactCandidates(content string) (string, []chatArtifactCandidate) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	candidates := make([]chatArtifactCandidate, 0, 4)
	seen := make(map[string]bool)
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			kept = append(kept, line)
			continue
		}
		if inFence {
			kept = append(kept, line)
			continue
		}
		sourcePath, recognized := chatArtifactSourcePath(trimmed)
		if !recognized || seen[sourcePath] {
			kept = append(kept, line)
			continue
		}
		mediaType, allowed := chatArtifactMediaTypes[strings.ToLower(path.Ext(sourcePath))]
		if !allowed || !safeChatArtifactSourcePath(sourcePath) {
			kept = append(kept, line)
			continue
		}
		name := safeActivityName(path.Base(sourcePath))
		if name == "" {
			kept = append(kept, line)
			continue
		}
		seen[sourcePath] = true
		candidates = append(candidates, chatArtifactCandidate{
			SourcePath: sourcePath, Name: name, MediaType: mediaType,
			Kind: artifactKind("", mediaType, name),
		})
	}
	return strings.Join(kept, "\n"), candidates
}

func chatArtifactSourcePath(line string) (string, bool) {
	value := line
	if strings.HasPrefix(value, "MEDIA:") || strings.HasPrefix(value, "FILE:") {
		value = strings.TrimSpace(value[strings.IndexByte(value, ':')+1:])
	} else if strings.HasPrefix(value, "Lokasi file:") {
		// Compatibility for the natural-language path label emitted by older
		// Hermes turns. Keep this exact and line-scoped so prose and code paths
		// are never promoted to downloadable artifacts accidentally.
		value = strings.TrimSpace(strings.TrimPrefix(value, "Lokasi file:"))
	} else if strings.HasPrefix(value, "file://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "file" || (parsed.Host != "" && parsed.Host != "localhost") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", false
		}
		var unescapeErr error
		value, unescapeErr = url.PathUnescape(parsed.Path)
		if unescapeErr != nil {
			return "", false
		}
	} else if !strings.HasPrefix(value, "/") {
		return "", false
	}
	if strings.ContainsAny(value, "\x00\n\r") {
		return "", false
	}
	cleaned := path.Clean(value)
	return cleaned, cleaned == value && strings.HasPrefix(cleaned, "/")
}

func safeChatArtifactSourcePath(sourcePath string) bool {
	if sourcePath == "" || path.Clean(sourcePath) != sourcePath {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(sourcePath, "/"), "/")
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	switch {
	case strings.HasPrefix(sourcePath, "/data/cache/"):
		return true
	case strings.HasPrefix(sourcePath, "/workspace/"):
		return true
	case path.Dir(sourcePath) == "/root":
		return true
	default:
		return false
	}
}

func (p *Provisioner) validateChatArtifactTarget(ctx context.Context, payload domain.ChatSendPayload) (string, string, error) {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.InstanceName) {
		return "", "", errors.New("invalid Fleet instance identity")
	}
	expectedProject, _, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.InstanceName)
	if payload.ProjectName != expectedProject {
		return "", "", errors.New("Compose project does not match the Fleet identity")
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil || managedPath != filepath.Join(p.root, expectedDirectory) {
		return "", "", errors.New("managed path does not match the Fleet identity")
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return "", "", errors.New("required instance file is missing or unsafe")
		}
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil || len(containers) != 2 {
		return "", "", errors.New("the exact Fleet-owned runtime is unavailable")
	}
	hermesContainer := ""
	for _, container := range containers {
		service := container.Config.Labels["com.docker.compose.service"]
		if (service != "hermes" && service != "dashboard") || container.State.Status != "running" {
			return "", "", errors.New("the managed runtime is not running")
		}
		if service == "hermes" {
			hermesContainer = container.ID
		}
	}
	if !containerIDPattern.MatchString(hermesContainer) {
		return "", "", errors.New("Hermes container identity is invalid")
	}
	return managedPath, hermesContainer, nil
}

func (p *Provisioner) stageChatArtifact(
	ctx context.Context,
	hermesContainer string,
	candidate chatArtifactCandidate,
) (string, int64, string, error) {
	if !containerIDPattern.MatchString(hermesContainer) || !safeChatArtifactSourcePath(candidate.SourcePath) {
		return "", 0, "", errors.New("unsafe chat artifact source")
	}
	stage, err := os.CreateTemp("", "hermes-fleet-chat-artifact-*")
	if err != nil {
		return "", 0, "", err
	}
	stagePath := stage.Name()
	removeStage := true
	defer func() {
		_ = stage.Close()
		if removeStage {
			_ = os.Remove(stagePath)
		}
	}()
	reader, wait := p.dockerOutput(ctx, "exec", hermesContainer, "python", "-c", chatArtifactReadScript,
		candidate.SourcePath, strconv.FormatInt(25<<20, 10))
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(stage, hasher), io.LimitReader(reader, (25<<20)+1))
	_ = reader.Close()
	waitErr := wait()
	if copyErr != nil || waitErr != nil || written < 1 || written > 25<<20 {
		return "", 0, "", errors.New("container output file could not be read")
	}
	if err := stage.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := stage.Close(); err != nil {
		return "", 0, "", err
	}
	if err := validateChatArtifactContent(stagePath, candidate); err != nil {
		return "", 0, "", err
	}
	removeStage = false
	return stagePath, written, hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateChatArtifactContent(filename string, candidate chatArtifactCandidate) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 512)
	read, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	header = header[:read]
	extension := strings.ToLower(path.Ext(candidate.Name))
	detected := http.DetectContentType(header)
	valid := false
	switch extension {
	case ".png":
		valid = detected == "image/png"
	case ".jpg", ".jpeg":
		valid = detected == "image/jpeg"
	case ".gif":
		valid = detected == "image/gif"
	case ".webp":
		valid = len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP"
	case ".pdf":
		valid = len(header) >= 5 && string(header[:5]) == "%PDF-"
	case ".csv":
		valid = strings.HasPrefix(detected, "text/plain") && utf8.Valid(header) && !bytes.ContainsRune(header, '\x00')
	case ".txt":
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		content, err := io.ReadAll(io.LimitReader(file, (25<<20)+1))
		if err != nil {
			return err
		}
		valid = len(content) <= 25<<20 && utf8.Valid(content) && !bytes.ContainsRune(content, '\x00')
	case ".xlsx", ".docx", ".pptx":
		valid = validOfficeArtifact(filename, extension)
	case ".mp3":
		valid = len(header) >= 3 && (string(header[:3]) == "ID3" || header[0] == 0xff && header[1]&0xe0 == 0xe0)
	case ".wav":
		valid = len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WAVE"
	case ".mp4":
		valid = len(header) >= 12 && string(header[4:8]) == "ftyp"
	case ".webm":
		valid = len(header) >= 4 && bytes.Equal(header[:4], []byte{0x1a, 0x45, 0xdf, 0xa3})
	}
	if !valid {
		return errors.New("chat artifact content does not match its allowed file type")
	}
	return nil
}

func validOfficeArtifact(filename, extension string) bool {
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return false
	}
	defer archive.Close()
	hasContentTypes, hasPayload := false, false
	wantedPrefix := map[string]string{".xlsx": "xl/", ".docx": "word/", ".pptx": "ppt/"}[extension]
	for _, entry := range archive.File {
		if entry.Name == "[Content_Types].xml" {
			hasContentTypes = true
		}
		if strings.HasPrefix(entry.Name, wantedPrefix) {
			hasPayload = true
		}
	}
	return hasContentTypes && hasPayload
}

func hermesChatRequestBody(payload domain.ChatSendPayload, input []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"input":        string(input),
		"instructions": "When you create an output file for the user, include its absolute path on a standalone line as FILE:/absolute/path. For generated images, MEDIA:/absolute/path is also supported.",
		"model_options": map[string]string{
			"reasoning_effort": payload.Reasoning,
			"service_tier":     payload.ServiceTier,
		},
	})
}

func hermesSessionChatURL(apiPort int, sessionID string, stream bool) string {
	suffix := "/chat"
	if stream {
		suffix += "/stream"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/api/sessions/%s%s", apiPort, url.PathEscape(sessionID), suffix)
}

func ensureHermesChatSession(
	ctx context.Context,
	client *http.Client,
	apiPort int,
	apiKey, sessionID string,
	payload domain.ChatSendPayload,
) error {
	requestBody, err := json.Marshal(map[string]any{
		"id":                 sessionID,
		"source":             "api_server",
		"provider":           payload.Provider,
		"model":              payload.Model,
		"require_model_lock": true,
		"model_options": map[string]string{
			"reasoning_effort": payload.Reasoning,
			"service_tier":     payload.ServiceTier,
		},
	})
	if err != nil {
		return err
	}
	defer clearSensitiveBytes(requestBody)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/sessions", apiPort), bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumHermesChatResponseBytes+1))
	if err != nil {
		return err
	}
	defer clearSensitiveBytes(responseBody)
	if len(responseBody) > maximumHermesChatResponseBytes {
		return errors.New("Hermes session response exceeded 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode != http.StatusConflict || hermesErrorCode(responseBody) != "session_exists" {
			return errors.New(hermesChatHTTPError(response.StatusCode, responseBody))
		}
	}
	return lockHermesChatSessionRuntime(ctx, client, apiPort, apiKey, sessionID, payload)
}

func lockHermesChatSessionRuntime(
	ctx context.Context,
	client *http.Client,
	apiPort int,
	apiKey, sessionID string,
	payload domain.ChatSendPayload,
) error {
	requestBody, err := json.Marshal(map[string]any{
		"provider": payload.Provider,
		"model":    payload.Model,
		"model_options": map[string]string{
			"reasoning_effort": payload.Reasoning,
			"service_tier":     payload.ServiceTier,
		},
	})
	if err != nil {
		return err
	}
	defer clearSensitiveBytes(requestBody)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/sessions/%s/model", apiPort, url.PathEscape(sessionID)),
		bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumHermesChatResponseBytes+1))
	if err != nil {
		return err
	}
	defer clearSensitiveBytes(responseBody)
	if len(responseBody) > maximumHermesChatResponseBytes {
		return errors.New("Hermes session model-lock response exceeded 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New(hermesChatHTTPError(response.StatusCode, responseBody))
	}
	var acknowledgement struct {
		SessionID string `json:"session_id"`
		Runtime   struct {
			Provider  string `json:"provider"`
			Model     string `json:"model"`
			ModelLock string `json:"model_lock"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(responseBody, &acknowledgement); err != nil {
		return errors.New("Hermes returned an invalid session model-lock acknowledgement")
	}
	if acknowledgement.SessionID != sessionID ||
		acknowledgement.Runtime.Provider != payload.Provider ||
		acknowledgement.Runtime.Model != payload.Model ||
		acknowledgement.Runtime.ModelLock != "accepted" {
		return errors.New("Hermes did not acknowledge the requested session model lock")
	}
	return nil
}

func hermesErrorCode(responseBody []byte) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(responseBody, &envelope) != nil {
		return ""
	}
	return envelope.Error.Code
}

type chatProgressReader struct {
	reader   io.Reader
	progress func()
}

func (r *chatProgressReader) Read(target []byte) (int, error) {
	n, err := r.reader.Read(target)
	if n > 0 && r.progress != nil {
		r.progress()
	}
	return n, err
}

type chatIdleGuard struct {
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	cancel   context.CancelFunc
	expired  bool
	stopped  bool
}

func newChatIdleGuard(timeout time.Duration, cancel context.CancelFunc) *chatIdleGuard {
	guard := &chatIdleGuard{timeout: timeout, deadline: time.Now().Add(timeout), cancel: cancel}
	guard.timer = time.AfterFunc(timeout, guard.check)
	return guard
}

func (g *chatIdleGuard) check() {
	g.mu.Lock()
	if g.stopped || g.expired {
		g.mu.Unlock()
		return
	}
	if remaining := time.Until(g.deadline); remaining > 0 {
		g.timer.Reset(remaining)
		g.mu.Unlock()
		return
	}
	g.expired = true
	cancel := g.cancel
	g.mu.Unlock()
	cancel()
}

func (g *chatIdleGuard) Touch() {
	g.mu.Lock()
	if !g.stopped && !g.expired {
		g.deadline = time.Now().Add(g.timeout)
		g.timer.Reset(g.timeout)
	}
	g.mu.Unlock()
}

func (g *chatIdleGuard) Expired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.expired
}

func (g *chatIdleGuard) Stop() {
	g.mu.Lock()
	g.stopped = true
	if g.timer != nil {
		g.timer.Stop()
	}
	g.mu.Unlock()
}

func consumeHermesChatStream(
	ctx context.Context,
	reader io.Reader,
	sequence int64,
	report func(context.Context, domain.ChatStreamEvent) error,
) (string, int64, error) {
	limited := &io.LimitedReader{R: reader, N: maximumHermesChatResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 32<<10), maximumHermesChatResponseBytes)
	var complete strings.Builder
	var pending strings.Builder
	dataLines := []string{}
	eventName := ""
	unknownEvents := 0
	lastFlush := time.Now()
	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		sequence++
		delta := pending.String()
		pending.Reset()
		lastFlush = time.Now()
		return report(ctx, domain.ChatStreamEvent{Sequence: sequence, Type: domain.ChatEventDelta, Content: delta})
	}
	consume := func() (bool, error) {
		if len(dataLines) == 0 && eventName == "" {
			return false, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		name := eventName
		eventName = ""
		if name == "ping" || name == "heartbeat" || data == "" {
			return false, nil
		}
		if strings.TrimSpace(data) == "[DONE]" {
			return true, flush()
		}
		frame, err := decodeHermesChatEvent(name, []byte(data))
		if err != nil {
			return false, err
		}
		if !frame.Recognized {
			unknownEvents++
			return false, nil
		}
		if frame.Activity != "" {
			sequence++
			eventType := frame.EventType
			if eventType == "" {
				eventType = domain.ChatEventActivity
			}
			if err := report(ctx, domain.ChatStreamEvent{
				Sequence: sequence,
				Type:     eventType,
				Content:  frame.Activity,
			}); err != nil {
				return false, fmt.Errorf("chat stream activity could not be recorded: %w", err)
			}
		}
		if frame.Text == "" {
			if frame.Done {
				return true, flush()
			}
			return false, nil
		}
		// Completed frames may repeat text already emitted as deltas.
		if frame.Done && complete.Len() > 0 {
			return true, flush()
		}
		if !utf8.ValidString(frame.Text) || complete.Len()+len(frame.Text) > maximumHermesChatResponseBytes {
			return false, errors.New("Hermes chat response exceeded 1 MiB or contained invalid UTF-8")
		}
		complete.WriteString(frame.Text)
		pending.WriteString(frame.Text)
		if pending.Len() >= 256 || time.Since(lastFlush) >= 80*time.Millisecond {
			if err := flush(); err != nil {
				return false, fmt.Errorf("chat stream delta could not be recorded: %w", err)
			}
		}
		if frame.Done {
			return true, flush()
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := consume()
			if err != nil {
				return "", sequence, err
			}
			if done {
				return complete.String(), sequence, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			eventName = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", sequence, fmt.Errorf("read Hermes chat stream: %w", err)
	}
	if limited.N == 0 {
		return "", sequence, errors.New("Hermes chat stream exceeded 1 MiB")
	}
	if len(dataLines) > 0 {
		if _, err := consume(); err != nil {
			return "", sequence, err
		}
	}
	if err := flush(); err != nil {
		return "", sequence, fmt.Errorf("chat stream delta could not be recorded: %w", err)
	}
	if complete.Len() == 0 {
		if unknownEvents > 0 {
			return "", sequence, errors.New("Hermes chat stream used an unsupported event format")
		}
		return "", sequence, errors.New("Hermes chat stream ended without a message")
	}
	return complete.String(), sequence, nil
}

type hermesChatFrame struct {
	Text       string
	Activity   string
	EventType  string
	Done       bool
	Recognized bool
}

// decodeHermesChatEvent is the compatibility boundary between Hermes and the
// Fleet conversation protocol. Every non-error upstream frame is accepted.
// User-visible text is emitted as Text; the exact SSE data field for tool,
// reasoning, lifecycle, and future frames is retained as an encrypted activity
// event instead of being reconstructed or rejected.
func decodeHermesChatEvent(eventName string, data []byte) (hermesChatFrame, error) {
	trimmed := bytes.TrimSpace(data)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		if utf8.Valid(trimmed) && isHermesTextEvent(eventName) {
			return hermesChatFrame{Text: string(trimmed), Recognized: true}, nil
		}
		return newHermesActivityFrame(eventName, "", trimmed), nil
	}
	if eventName == "error" {
		return hermesChatFrame{}, errors.New("Hermes returned an error while streaming the response")
	}
	normalizedEventName := strings.ToLower(strings.TrimSpace(eventName))
	var eventType string
	if raw := envelope["type"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &eventType)
	}
	if eventType == "error" || strings.HasSuffix(eventType, ".error") || strings.HasSuffix(eventType, ".failed") {
		return hermesChatFrame{}, errors.New("Hermes returned an error while streaming the response")
	}
	switch normalizedEventName {
	case "assistant.delta":
		part, err := chatTextFromValue(envelope["delta"])
		if err != nil || part == "" {
			return newHermesActivityFrame(eventName, eventType, trimmed), nil
		}
		return hermesChatFrame{Text: part, Recognized: true}, nil
	case "assistant.completed":
		part, _ := chatTextFromValue(envelope["content"])
		return hermesChatFrame{Text: part, Done: true, Recognized: true}, nil
	case "done":
		return hermesChatFrame{Done: true, Recognized: true}, nil
	case "run.started", "message.started", "run.completed":
		return newHermesActivityFrame(eventName, eventType, trimmed), nil
	}
	if isHermesNonVisibleEvent(eventName, eventType) {
		return newHermesActivityFrame(eventName, eventType, trimmed), nil
	}
	if rawChoices, ok := envelope["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(rawChoices, &choices); err != nil {
			return newHermesActivityFrame(eventName, eventType, trimmed), nil
		}
		var text strings.Builder
		done := false
		for _, choice := range choices {
			part, err := chatTextFromValue(choice["delta"])
			if err != nil {
				return newHermesActivityFrame(eventName, eventType, trimmed), nil
			}
			if part == "" {
				_ = json.Unmarshal(choice["text"], &part)
			}
			if part == "" {
				part, err = chatTextFromValue(choice["message"])
				if err != nil {
					return newHermesActivityFrame(eventName, eventType, trimmed), nil
				}
			}
			text.WriteString(part)
			choiceDone, err := hermesFinishReason(choice["finish_reason"])
			if err != nil {
				return hermesChatFrame{}, err
			}
			done = done || choiceDone
		}
		if text.Len() > 0 {
			return hermesChatFrame{Text: text.String(), Done: done, Recognized: true}, nil
		}
		frame := newHermesActivityFrame(eventName, eventType, trimmed)
		frame.Done = done
		return frame, nil
	}
	switch eventType {
	case "response.output_text.delta", "content_block_delta", "message.delta", "message_delta", "text_delta", "token":
		part, err := chatTextFromValue(envelope["delta"])
		if err != nil {
			return newHermesActivityFrame(eventName, eventType, trimmed), nil
		}
		if part == "" {
			part, _ = chatTextFromValue(envelope["text"])
		}
		if part != "" {
			return hermesChatFrame{Text: part, Recognized: true}, nil
		}
		return newHermesActivityFrame(eventName, eventType, trimmed), nil
	case "response.completed", "response.done", "message.completed", "message_stop", "done":
		return hermesChatFrame{Text: firstHermesVisibleText(envelope), Done: true, Recognized: true}, nil
	case "response.created", "response.in_progress", "message_start", "content_block_start", "content_block_stop", "ping":
		return newHermesActivityFrame(eventName, eventType, trimmed), nil
	}
	if part := firstHermesVisibleText(envelope); part != "" {
		return hermesChatFrame{Text: part, Recognized: true}, nil
	}
	return newHermesActivityFrame(eventName, eventType, trimmed), nil
}

func hermesFinishReason(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var reason string
	if err := json.Unmarshal(raw, &reason); err != nil {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return false, nil
	case "error":
		return false, errors.New("Hermes returned an error while streaming the response")
	case "length":
		return false, errors.New("Hermes returned an incomplete response because the output limit was reached")
	case "content_filter":
		return false, errors.New("Hermes returned an incomplete response because the output was filtered")
	default:
		return true, nil
	}
}

func newHermesActivityFrame(eventName, eventType string, data []byte) hermesChatFrame {
	normalizedType := domain.ChatEventActivity
	kind := strings.ToLower(strings.TrimSpace(eventName + " " + eventType))
	if strings.Contains(kind, "artifact") || strings.Contains(kind, "attachment") ||
		strings.Contains(kind, "output_file") || strings.Contains(kind, "file_output") ||
		strings.Contains(kind, "image_output") || strings.Contains(kind, "audio_output") || strings.Contains(kind, "video_output") {
		normalizedType = domain.ChatEventArtifact
	}
	activity := hermesActivityPayload(eventName, data)
	var payload domain.ChatEventPayload
	if json.Unmarshal([]byte(activity), &payload) == nil && payload.Kind == "artifact" && payload.Artifact != nil {
		normalizedType = domain.ChatEventArtifact
	}
	return hermesChatFrame{
		Activity: activity, EventType: normalizedType, Recognized: true,
	}
}

func isHermesNonVisibleEvent(eventName, eventType string) bool {
	kind := strings.ToLower(strings.TrimSpace(eventName + " " + eventType))
	for _, marker := range []string{
		"reasoning", "analysis", "thinking", "tool.", "tool_", "tool-", "function_call",
		"artifact", "attachment", "output_file", "file_output", "image_output", "audio_output", "video_output",
	} {
		if strings.Contains(kind, marker) {
			return true
		}
	}
	return false
}

func isHermesTextEvent(eventName string) bool {
	name := strings.ToLower(strings.TrimSpace(eventName))
	return name == "message" || name == "delta" || name == "token" ||
		strings.Contains(name, "text.delta")
}

func hermesActivityPayload(eventName string, data []byte) string {
	var envelope map[string]any
	_ = json.Unmarshal(data, &envelope)
	eventType := stringValue(envelope["type"])
	item := objectValue(envelope["item"])
	itemType := stringValue(item["type"])
	event := strings.TrimSpace(eventName)
	if event == "" {
		event = eventType
	}
	if event == "" {
		event = "message"
	}
	kind := strings.ToLower(event + " " + eventType + " " + itemType)
	payload := domain.ChatEventPayload{Kind: "activity", Event: event, Data: string(data)}

	artifact := firstObjectValue(envelope, "artifact", "attachment", "file", "output_file")
	if len(artifact) == 0 {
		artifact = firstObjectValue(item, "artifact", "attachment", "file", "output_file")
	}
	isArtifact := strings.Contains(kind, "artifact") || strings.Contains(kind, "attachment") || len(artifact) > 0 ||
		strings.Contains(kind, "output_file") || strings.Contains(kind, "file_output") ||
		strings.Contains(kind, "image_output") || strings.Contains(kind, "audio_output") || strings.Contains(kind, "video_output")
	if !isArtifact {
		encoded, _ := json.Marshal(payload)
		return string(encoded)
	}
	// Downloadable artifacts keep the existing bounded capability envelope;
	// executable URLs and unrelated upstream fields must not become browser
	// capabilities. Ordinary Hermes process events never take this path.
	payload.Data = ""

	tool := objectValue(envelope["tool"])
	payload.Tool = safeActivityName(stringValue(envelope["tool"]))
	if payload.Tool == "" {
		payload.Tool = safeActivityName(stringValue(tool["name"]))
	}
	if payload.Tool == "" {
		payload.Tool = safeActivityName(firstNonEmpty(stringValue(envelope["name"]), stringValue(item["name"])))
	}
	payload.CallID = safeActivityIdentifier(firstNonEmpty(
		stringValue(envelope["toolCallId"]), stringValue(envelope["tool_call_id"]), stringValue(envelope["call_id"]),
		stringValue(tool["toolCallId"]), stringValue(tool["tool_call_id"]), stringValue(tool["call_id"]),
		stringValue(item["toolCallId"]), stringValue(item["tool_call_id"]), stringValue(item["call_id"]),
		stringValue(envelope["id"]), stringValue(item["id"]),
	), "")
	payload.Status = safeActivityIdentifier(firstNonEmpty(
		stringValue(envelope["status"]), stringValue(envelope["state"]), stringValue(tool["status"]), stringValue(item["status"]),
	), "")
	payload.Label = safeActivityLabel(firstNonEmpty(
		stringValue(envelope["label"]), stringValue(tool["label"]), stringValue(item["label"]),
		stringValue(envelope["preview"]), stringValue(tool["preview"]), stringValue(item["preview"]),
	))
	payload.DurationMS = safeActivityMilliseconds(firstNonNil(
		envelope["duration_ms"], tool["duration_ms"], item["duration_ms"],
	))
	if payload.DurationMS == 0 {
		payload.DurationMS = safeActivityDurationMS(firstNonNil(
			envelope["duration"], tool["duration"], item["duration"],
			envelope["duration_seconds"], tool["duration_seconds"], item["duration_seconds"],
		))
	}

	if strings.Contains(kind, "tool") || strings.Contains(kind, "function_call") {
		if payload.Label == "" {
			payload.Label = payload.Tool
		}
		if strings.Contains(kind, "result") || strings.Contains(kind, "completed") || strings.EqualFold(payload.Status, "completed") {
			payload.Status = "completed"
		}
	} else if strings.Contains(kind, "reasoning") || strings.Contains(kind, "thinking") || strings.Contains(kind, "analysis") {
		if payload.Label == "" {
			payload.Label = "Thinking"
		}
	}
	if payload.Label == "" {
		payload.Label = safeActivityLabel(firstNonEmpty(stringValue(envelope["message"]), event))
	}
	if payload.Label == "" {
		payload.Label = "Activity"
	}

	if isArtifact {
		name := firstNonEmpty(
			stringValue(artifact["name"]), stringValue(artifact["filename"]), stringValue(artifact["title"]),
			stringValue(envelope["name"]), stringValue(envelope["filename"]), stringValue(item["name"]),
		)
		artifactURL := safeArtifactURL(firstNonEmpty(
			stringValue(artifact["url"]), stringValue(artifact["download_url"]), stringValue(artifact["uri"]),
			stringValue(envelope["url"]), stringValue(envelope["download_url"]), stringValue(envelope["uri"]),
		))
		if name == "" {
			if parsed, err := url.Parse(artifactURL); err == nil {
				name = path.Base(parsed.Path)
			}
		}
		if name == "" {
			name = "Artifact"
		}
		name = safeActivityName(path.Base(strings.ReplaceAll(name, "\\", "/")))
		if name == "" {
			name = "Artifact"
		}
		mediaType := safeArtifactMediaType(firstNonEmpty(
			stringValue(artifact["media_type"]), stringValue(artifact["mime_type"]), stringValue(artifact["content_type"]),
			stringValue(envelope["media_type"]), stringValue(envelope["mime_type"]), stringValue(envelope["content_type"]),
		))
		payload.Kind = "artifact"
		payload.Label = "Created " + name
		payload.Artifact = &domain.ChatArtifact{
			ID: safeActivityIdentifier(firstNonEmpty(
				stringValue(artifact["id"]), stringValue(envelope["id"]), stringValue(item["id"]),
			), ""),
			Name:       name,
			Kind:       artifactKind(kind, mediaType, name),
			MediaType:  mediaType,
			SizeBytes:  safeArtifactSize(firstNonNil(artifact["size_bytes"], artifact["size"], envelope["size_bytes"], envelope["size"])),
			URL:        artifactURL,
			SourceTool: payload.Tool,
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maximumChatDeltaBytes {
		return `{"kind":"activity","event":"unknown","label":"Working"}`
	}
	return string(encoded)
}

func objectValue(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func firstObjectValue(object map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value := objectValue(object[key]); len(value) > 0 {
			return value
		}
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func safeArtifactURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func safeArtifactMediaType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || len(mediaType) > 127 {
		return ""
	}
	return strings.ToLower(mediaType)
}

func safeArtifactSize(value any) int64 {
	var size int64
	switch typed := value.(type) {
	case float64:
		if typed > 0 && typed <= float64(1<<40) && typed == float64(int64(typed)) {
			size = int64(typed)
		}
	case json.Number:
		size, _ = typed.Int64()
	case string:
		size, _ = strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	}
	if size < 1 || size > 1<<40 {
		return 0
	}
	return size
}

func artifactKind(eventKind, mediaType, name string) string {
	mediaType = strings.ToLower(mediaType)
	eventKind = strings.ToLower(eventKind)
	extension := strings.ToLower(path.Ext(name))
	switch {
	case strings.HasPrefix(mediaType, "image/") || strings.Contains(eventKind, "image_output") ||
		containsString([]string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"}, extension):
		return "image"
	case strings.HasPrefix(mediaType, "audio/") || strings.Contains(eventKind, "audio_output") ||
		containsString([]string{".mp3", ".wav", ".m4a", ".ogg", ".flac"}, extension):
		return "audio"
	case strings.HasPrefix(mediaType, "video/") || strings.Contains(eventKind, "video_output") ||
		containsString([]string{".mp4", ".webm", ".mov", ".m4v"}, extension):
		return "video"
	default:
		return "file"
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func safeActivityName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 {
		value = value[:80]
	}
	return strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune(" ._-()", character) {
			return character
		}
		return -1
	}, value)
}

func safeActivityLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512]
	}
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' {
			return ' '
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
}

func safeActivityDurationMS(value any) int64 {
	var seconds float64
	switch typed := value.(type) {
	case float64:
		seconds = typed
	case json.Number:
		seconds, _ = typed.Float64()
	case string:
		seconds, _ = strconv.ParseFloat(strings.TrimSpace(typed), 64)
	}
	if seconds <= 0 || seconds > 24*60*60 {
		return 0
	}
	return int64(seconds * 1000)
}

func safeActivityMilliseconds(value any) int64 {
	var milliseconds float64
	switch typed := value.(type) {
	case float64:
		milliseconds = typed
	case json.Number:
		milliseconds, _ = typed.Float64()
	case string:
		milliseconds, _ = strconv.ParseFloat(strings.TrimSpace(typed), 64)
	}
	if milliseconds <= 0 || milliseconds > float64((24*time.Hour)/time.Millisecond) {
		return 0
	}
	return int64(milliseconds)
}

func safeActivityIdentifier(value, fallback string) string {
	value = strings.TrimSpace(value)
	if len(value) > 96 {
		value = value[:96]
	}
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._:/-", character) {
			return character
		}
		return -1
	}, value)
	if value == "" {
		return fallback
	}
	return value
}

func firstHermesVisibleText(envelope map[string]json.RawMessage) string {
	for _, key := range []string{"delta", "content", "text", "output_text", "message", "response"} {
		if part, err := chatTextFromValue(envelope[key]); err == nil && part != "" {
			return part
		}
	}
	return ""
}

func chatTextFromValue(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var object struct {
		Content json.RawMessage `json:"content"`
		Text    json.RawMessage `json:"text"`
		Output  json.RawMessage `json:"output"`
		Delta   json.RawMessage `json:"delta"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		for _, value := range []json.RawMessage{
			object.Content, object.Text, object.Output, object.Delta, object.Message,
		} {
			if len(value) == 0 {
				continue
			}
			content, err := chatTextFromValue(value)
			if err != nil {
				return "", err
			}
			if content != "" {
				return content, nil
			}
		}
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var joined strings.Builder
		for _, part := range parts {
			value, err := chatTextFromValue(part)
			if err != nil {
				return "", err
			}
			joined.WriteString(value)
		}
		return joined.String(), nil
	}
	// Role, finish-reason, and tool metadata can be valid members of a delta
	// object without carrying user-visible text.
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err == nil {
		return "", nil
	}
	return "", errors.New("unsupported text value")
}

const (
	maximumChatInputBytes          = 64 << 10
	maximumChatDeltaBytes          = 64 << 10
	maximumHermesChatResponseBytes = 1 << 20
)

func readManagedEnvValue(filename, requestedKey string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 64<<10))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == requestedKey {
			value = trimEnvValue(value)
			if value == "" {
				return "", fmt.Errorf("%s is empty", requestedKey)
			}
			return value, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s is missing", requestedKey)
}

func hermesChatHTTPError(status int, body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	message := ""
	if json.Unmarshal(body, &payload) == nil {
		message = strings.TrimSpace(payload.Error.Message)
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return fmt.Sprintf("Hermes chat returned HTTP %d", status)
	}
	return fmt.Sprintf("Hermes chat returned HTTP %d: %s", status, message)
}

func (p *Provisioner) authenticateCodex(ctx context.Context, payload domain.CodexAuthPayload, report func(context.Context, domain.JobProgress) error) domain.JobResult {
	fail := func(message string, err error) domain.JobResult {
		return domain.JobResult{Success: false, Error: message + ": " + err.Error()}
	}
	if report == nil {
		return domain.JobResult{Success: false, Error: "Codex authentication requires a progress-capable Host Agent"}
	}
	managedPath, hermesContainer, err := p.validateCodexAuthTarget(ctx, payload)
	if err != nil {
		return fail("Codex authentication preflight failed", err)
	}
	if err := report(ctx, domain.JobProgress{Stage: "STARTING"}); err != nil {
		return fail("Codex authentication progress could not be recorded", err)
	}
	awaitingCode := false
	reportedCode := false
	authArgs := []string{
		"compose", "--env-file", filepath.Join(managedPath, ".env"), "-p", payload.ProjectName,
		"-f", filepath.Join(managedPath, "compose.yaml"), "exec", "-T", "hermes",
		"hermes", "auth", "add", "openai-codex", "--no-browser", "--timeout", "900",
	}
	runner := p.authRun
	if runner == nil {
		runner = p.runStreamingDocker
	}
	err = runner(ctx, authArgs, func(rawLine string) error {
		line := strings.TrimSpace(ansiEscapePattern.ReplaceAllString(rawLine, ""))
		if line == "" {
			return nil
		}
		if strings.Contains(line, "Enter this code:") {
			awaitingCode = true
			return nil
		}
		if !awaitingCode || reportedCode || !codexUserCode.MatchString(line) {
			return nil
		}
		reportedCode = true
		return report(ctx, domain.JobProgress{
			Stage: "AWAITING_USER", VerificationURI: codexDeviceURL, UserCode: line,
			ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
		})
	})
	if err != nil {
		return fail("Codex authentication failed", err)
	}
	if err := report(ctx, domain.JobProgress{Stage: "VERIFYING"}); err != nil {
		return fail("Codex authentication progress could not be recorded", err)
	}
	status, err := p.docker(ctx, "exec", hermesContainer, "hermes", "auth", "status", "openai-codex")
	if err != nil || !strings.Contains(status, "openai-codex: logged in") {
		if err == nil {
			err = errors.New("Hermes did not report a connected Codex session")
		}
		return fail("Codex authentication verification failed", err)
	}
	return domain.JobResult{Success: true, Message: "Codex authentication connected"}
}

func (p *Provisioner) validateCodexAuthTarget(ctx context.Context, payload domain.CodexAuthPayload) (string, string, error) {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return "", "", errors.New("invalid Fleet instance identity")
	}
	shortID := strings.ReplaceAll(payload.InstanceID, "-", "")[:8]
	if payload.ProjectName != "hermes-fleet-"+payload.Name+"-"+shortID {
		return "", "", errors.New("Compose project does not match the Fleet identity")
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return "", "", err
	}
	if managedPath != filepath.Join(p.root, payload.Name+"-"+shortID) {
		return "", "", errors.New("managed path does not match the Fleet identity")
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return "", "", errors.New("required instance file is missing or unsafe")
		}
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return "", "", err
	}
	if len(containers) != 2 {
		return "", "", errors.New("the exact two Fleet-owned containers are required")
	}
	hermesContainer := ""
	for _, container := range containers {
		service := container.Config.Labels["com.docker.compose.service"]
		if (service != "hermes" && service != "dashboard") || container.State.Status != "running" {
			return "", "", errors.New("Hermes and dashboard containers must both be running")
		}
		if service == "hermes" {
			hermesContainer = container.ID
		}
	}
	if !containerIDPattern.MatchString(hermesContainer) {
		return "", "", errors.New("Hermes container identity is invalid")
	}
	return managedPath, hermesContainer, nil
}

const runtimeStateSchemaVersion = compatibility.RuntimeSchemaCurrent

const runtimeStateProbe = `import json, os
from hermes_cli.config import load_config
loaded = load_config()
model = loaded.get("model", {}) if isinstance(loaded, dict) else {}
if not isinstance(model, dict):
    model = {}
agent = loaded.get("agent", {}) if isinstance(loaded, dict) else {}
if not isinstance(agent, dict):
    agent = {}
environment = {
    "provider": os.environ.get("HERMES_INFERENCE_PROVIDER", ""),
    "model": os.environ.get("HERMES_INFERENCE_MODEL", ""),
    "reasoning": os.environ.get("HERMES_REASONING_EFFORT", ""),
    "service_tier": os.environ.get("HERMES_SERVICE_TIER", ""),
}
state = None
try:
    with open(os.path.join(os.environ.get("HERMES_HOME", "/data"), ".fleet-runtime-ready.json"), encoding="utf-8") as handle:
        state = json.load(handle)
except (FileNotFoundError, json.JSONDecodeError, OSError):
    pass
print(json.dumps({"agent": agent, "environment": environment, "model": model, "state": state}, sort_keys=True))`

const codexModelCatalogProbe = `import json
from hermes_cli.models import get_default_model_for_provider, provider_model_ids
print(json.dumps({
    "models": provider_model_ids("openai-codex"),
    "recommended": get_default_model_for_provider("openai-codex"),
}, sort_keys=True))`

const runtimeStateApply = `import fcntl, hashlib, json, os, sys, tempfile
from hermes_cli import config as hermes_config
provider, model, reasoning, service_tier, schema_version_raw, build_id = sys.argv[1:7]
schema_version = int(schema_version_raw)
if schema_version not in (1, 2):
    raise RuntimeError("unsupported Fleet runtime configuration schema")
if not provider or not model or not reasoning or not service_tier:
    raise RuntimeError("incomplete Fleet runtime configuration")
if len(build_id) != 64 or any(character not in "0123456789abcdef" for character in build_id):
    raise RuntimeError("invalid Fleet runtime wrapper build identity")
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
lock_path = os.path.join(home, ".fleet-runtime-config.lock")
with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    raw = hermes_config.read_raw_config() or {}
    if not isinstance(raw, dict):
        raise RuntimeError("Hermes configuration root must be a mapping")
    model_config = raw.get("model", {})
    if not isinstance(model_config, dict):
        model_config = {}
    model_config["default"] = model
    model_config["provider"] = provider
    raw["model"] = model_config
    agent_config = raw.get("agent", {})
    if not isinstance(agent_config, dict):
        agent_config = {}
    agent_config["reasoning_effort"] = reasoning
    agent_config["service_tier"] = service_tier
    raw["agent"] = agent_config
    hermes_config.save_config(
        raw,
        strip_defaults=False,
        preserve_keys={
            ("model", "default"),
            ("model", "provider"),
            ("agent", "reasoning_effort"),
            ("agent", "service_tier"),
        },
    )
    loaded = hermes_config.load_config()
    effective_model = loaded.get("model", {}) if isinstance(loaded, dict) else {}
    effective_agent = loaded.get("agent", {}) if isinstance(loaded, dict) else {}
    if (
        not isinstance(effective_model, dict)
        or not isinstance(effective_agent, dict)
        or effective_model.get("default") != model
        or effective_model.get("provider") != provider
        or effective_agent.get("reasoning_effort") != reasoning
        or effective_agent.get("service_tier") != service_tier
    ):
        raise RuntimeError("Hermes did not persist the Fleet runtime configuration")
    revision_values = (provider, model)
    if schema_version == 2:
        revision_values = (provider, model, reasoning, service_tier)
    state = {
        "schema_version": schema_version,
        "configuration_revision": hashlib.sha256("\0".join(revision_values).encode()).hexdigest(),
        "provider": provider,
        "model": model,
        "runtime_build_id": build_id,
    }
    if schema_version == 2:
        state["reasoning"] = reasoning
        state["service_tier"] = service_tier
    descriptor, temporary = tempfile.mkstemp(prefix=".fleet-runtime-ready-", dir=home)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(state, handle, sort_keys=True, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, os.path.join(home, ".fleet-runtime-ready.json"))
        directory = os.open(home, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)`

const runtimeConfigSnapshotScript = `import base64, json, os
home = os.environ.get("HERMES_HOME", "/data")
result = {}
for key, filename in (
    ("config", "config.yaml"),
    ("marker", ".fleet-runtime-ready.json"),
):
    path = os.path.join(home, filename)
    try:
        with open(path, "rb") as handle:
            data = handle.read()
        result[key] = {"exists": True, "data": base64.b64encode(data).decode("ascii")}
    except FileNotFoundError:
        result[key] = {"exists": False, "data": ""}
print(json.dumps(result, sort_keys=True, separators=(",", ":")))`

const runtimeConfigRestoreScript = `import base64, fcntl, json, os, sys, tempfile
snapshot = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
lock_path = os.path.join(home, ".fleet-runtime-config.lock")
files = (
    ("config", "config.yaml"),
    ("marker", ".fleet-runtime-ready.json"),
)
with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    for key, filename in files:
        value = snapshot.get(key)
        if not isinstance(value, dict) or not isinstance(value.get("exists"), bool):
            raise RuntimeError("invalid Fleet runtime configuration snapshot")
        path = os.path.join(home, filename)
        if value["exists"]:
            data = base64.b64decode(value.get("data", ""), validate=True)
            descriptor, temporary = tempfile.mkstemp(prefix=".fleet-runtime-restore-", dir=home)
            try:
                with os.fdopen(descriptor, "wb") as handle:
                    handle.write(data)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.chmod(temporary, 0o600)
                os.replace(temporary, path)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        else:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass`

type runtimeStateObservation struct {
	Environment struct {
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		Reasoning   string `json:"reasoning"`
		ServiceTier string `json:"service_tier"`
	} `json:"environment"`
	Agent map[string]any `json:"agent"`
	Model map[string]any `json:"model"`
	State *struct {
		SchemaVersion         int    `json:"schema_version"`
		ConfigurationRevision string `json:"configuration_revision"`
		Provider              string `json:"provider"`
		Model                 string `json:"model"`
		Reasoning             string `json:"reasoning"`
		ServiceTier           string `json:"service_tier"`
		RuntimeBuildID        string `json:"runtime_build_id"`
	} `json:"state"`
}

type runtimeImageCapability struct {
	SchemaVersion int
	BuildID       string
}

type runtimeConfigFileSnapshot struct {
	Exists bool   `json:"exists"`
	Data   string `json:"data"`
}

type runtimeConfigSnapshot struct {
	Config runtimeConfigFileSnapshot `json:"config"`
	Marker runtimeConfigFileSnapshot `json:"marker"`
}

func (p *Provisioner) syncRuntimeConfiguration(ctx context.Context, payload domain.RuntimeSyncPayload) domain.JobResult {
	failed := func(message string, err error) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		status := ""
		if payload.DesiredStatus == domain.InstanceRunning || payload.DesiredStatus == domain.InstanceStopped {
			status = payload.DesiredStatus
		}
		return domain.JobResult{Success: false, Error: message, InstanceStatus: status}
	}
	managedPath, hermesContainer, err := p.validateRuntimeSyncTarget(ctx, payload)
	if err != nil {
		return failed("runtime synchronization refused", err)
	}
	capability, err := p.inspectRuntimeImageCapability(ctx, payload.ImageID)
	if err != nil {
		return failed("runtime synchronization refused", err)
	}
	envPath := filepath.Join(managedPath, ".env")
	originalEnv, err := os.ReadFile(envPath)
	if err != nil {
		return failed("managed runtime environment could not be read", err)
	}
	defer clearSensitiveBytes(originalEnv)
	configSnapshot, snapshotJSON, err := p.snapshotRuntimeConfiguration(ctx, payload)
	if err != nil {
		return failed("Hermes runtime configuration could not be snapshotted", err)
	}
	defer clearSensitiveBytes(snapshotJSON)
	updatedEnv, err := updateEnvContent(originalEnv, map[string]string{
		"HERMES_INFERENCE_PROVIDER": payload.Provider,
		"HERMES_INFERENCE_MODEL":    payload.Model,
		"HERMES_REASONING_EFFORT":   payload.Reasoning,
		"HERMES_SERVICE_TIER":       payload.ServiceTier,
	})
	if err != nil {
		return failed("managed runtime environment is invalid", err)
	}
	defer clearSensitiveBytes(updatedEnv)
	if err := writeAtomicReplaceContext(ctx, envPath, updatedEnv, 0o600); err != nil {
		return failed("managed runtime environment could not be updated", err)
	}
	rollback := func() error {
		rollbackContext, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		if err := rollbackContext.Err(); err != nil {
			return err
		}
		var rollbackErrors []string
		if err := writeAtomicReplaceContext(rollbackContext, envPath, originalEnv, 0o600); err != nil {
			rollbackErrors = append(rollbackErrors, "environment: "+err.Error())
		}
		if err := p.restoreRuntimeConfiguration(rollbackContext, payload, snapshotJSON); err != nil {
			rollbackErrors = append(rollbackErrors, "Hermes config: "+err.Error())
		}
		if payload.DesiredStatus == domain.InstanceRunning && len(rollbackErrors) == 0 {
			output, err := p.compose(
				rollbackContext, managedPath, payload.ProjectName,
				"up", "-d", "--remove-orphans", "--force-recreate", "hermes", "dashboard",
			)
			if err == nil {
				err = p.waitForDashboard(rollbackContext, payload.DashboardPort, provisionDashboardReadyTimeout)
			}
			if err != nil {
				rollbackErrors = append(rollbackErrors, "runtime: "+safeCommandError(err, output))
			}
		}
		if len(rollbackErrors) == 0 {
			restored, _, err := p.snapshotRuntimeConfiguration(rollbackContext, payload)
			if err != nil {
				rollbackErrors = append(rollbackErrors, "verification: "+err.Error())
			} else if restored != configSnapshot {
				rollbackErrors = append(rollbackErrors, "verification: restored Hermes config does not match the snapshot")
			}
		}
		if len(rollbackErrors) > 0 {
			return errors.New(strings.Join(rollbackErrors, "; "))
		}
		return nil
	}
	failedAfterChange := func(message string, err error) domain.JobResult {
		result := failed(message, err)
		if rollbackErr := rollback(); rollbackErr != nil {
			result.Error += "; automatic rollback failed; manual recovery is required: " + rollbackErr.Error()
			result.InstanceStatus = domain.InstanceFailed
		} else {
			result.Error += "; previous runtime configuration was restored"
		}
		return result
	}
	if payload.DesiredStatus == domain.InstanceRunning {
		if err := p.applyRuntimeConfig(ctx, []string{"exec", hermesContainer, "python"}, payload, capability); err != nil {
			return failedAfterChange("Hermes runtime configuration could not be written", err)
		}
		if output, recreateErr := p.compose(
			ctx, managedPath, payload.ProjectName,
			"up", "-d", "--remove-orphans", "--force-recreate", "hermes", "dashboard",
		); recreateErr != nil {
			return failedAfterChange(
				"Hermes services could not be recreated after synchronization",
				errors.New(safeCommandError(recreateErr, output)),
			)
		}
		if err := p.waitForDashboard(ctx, payload.DashboardPort, provisionDashboardReadyTimeout); err != nil {
			return failedAfterChange("Hermes Dashboard did not become ready after synchronization", err)
		}
		if err := p.verifyRuntimeConfig(
			ctx,
			[]string{"compose", "--env-file", envPath, "-p", payload.ProjectName, "-f", filepath.Join(managedPath, "compose.yaml"),
				"exec", "-T", "hermes", "python", "-c", runtimeStateProbe},
			payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
			capability,
		); err != nil {
			return failedAfterChange("Hermes runtime configuration verification failed", err)
		}
	} else {
		if err := p.applyRuntimeConfig(ctx, p.runtimeVolumeCommand(payload, "python"), payload, capability); err != nil {
			return failedAfterChange("stopped Hermes runtime configuration could not be written", err)
		}
		if err := p.verifyRuntimeConfig(
			ctx, p.runtimeVolumeCommand(payload, "python", "-c", runtimeStateProbe),
			payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
			capability,
		); err != nil {
			return failedAfterChange("stopped Hermes runtime configuration verification failed", err)
		}
	}
	return domain.JobResult{
		Success: true, Message: "Hermes runtime configuration synchronized",
		InstanceStatus: payload.DesiredStatus,
	}
}

const messagingConfigSnapshotScript = `import base64, json, os
home = os.environ.get("HERMES_HOME", "/data")
path = os.path.join(home, "config.yaml")
try:
    with open(path, "rb") as handle:
        data = handle.read()
    result = {"exists": True, "data": base64.b64encode(data).decode("ascii")}
except FileNotFoundError:
    result = {"exists": False, "data": ""}
print(json.dumps(result, sort_keys=True, separators=(",", ":")))`

const messagingConfigApplyScript = `import fcntl, json, os, sys, tempfile, yaml
settings = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
config_path = os.path.join(home, "config.yaml")
lock_path = os.path.join(home, ".fleet-messaging-config.lock")
with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    try:
        with open(config_path, encoding="utf-8") as handle:
            raw = yaml.safe_load(handle) or {}
    except FileNotFoundError:
        raw = {}
    if not isinstance(raw, dict):
        raise RuntimeError("Hermes configuration root must be a mapping")
    telegram = raw.get("telegram", {})
    if not isinstance(telegram, dict):
        telegram = {}
    telegram["require_mention"] = bool(settings["telegram_require_mention"])
    raw["telegram"] = telegram
    whatsapp = raw.get("whatsapp", {})
    if not isinstance(whatsapp, dict):
        whatsapp = {}
    whatsapp["unauthorized_dm_behavior"] = settings["whatsapp_unauthorized_dm_behavior"]
    whatsapp["reply_prefix"] = settings["whatsapp_reply_prefix"]
    raw["whatsapp"] = whatsapp
    descriptor, temporary = tempfile.mkstemp(prefix=".fleet-config-", dir=home)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            yaml.safe_dump(raw, handle, sort_keys=False, allow_unicode=True)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, config_path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    marker = {
        "schema_version": 1,
        "configuration_revision": settings["configuration_revision"],
    }
    descriptor, temporary = tempfile.mkstemp(prefix=".fleet-messaging-ready-", dir=home)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(marker, handle, sort_keys=True, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, os.path.join(home, ".fleet-messaging-ready.json"))
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)`

const messagingConfigVerifyScript = `import json, os, sys, yaml
settings = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
with open(os.path.join(home, "config.yaml"), encoding="utf-8") as handle:
    raw = yaml.safe_load(handle) or {}
telegram = raw.get("telegram", {}) if isinstance(raw, dict) else {}
whatsapp = raw.get("whatsapp", {}) if isinstance(raw, dict) else {}
try:
    with open(os.path.join(home, ".fleet-messaging-ready.json"), encoding="utf-8") as handle:
        marker = json.load(handle)
except (FileNotFoundError, json.JSONDecodeError, OSError):
    marker = {}
result = {
    "telegram_require_mention": telegram.get("require_mention") if isinstance(telegram, dict) else None,
    "whatsapp_unauthorized_dm_behavior": whatsapp.get("unauthorized_dm_behavior") if isinstance(whatsapp, dict) else None,
    "whatsapp_reply_prefix": whatsapp.get("reply_prefix") if isinstance(whatsapp, dict) else None,
    "schema_version": marker.get("schema_version"),
    "configuration_revision": marker.get("configuration_revision"),
}
print(json.dumps(result, sort_keys=True, separators=(",", ":")))`

const messagingConfigRestoreScript = `import base64, fcntl, json, os, sys, tempfile
snapshot = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
config_path = os.path.join(home, "config.yaml")
lock_path = os.path.join(home, ".fleet-messaging-config.lock")
with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    if snapshot.get("exists"):
        data = base64.b64decode(snapshot["data"], validate=True)
        descriptor, temporary = tempfile.mkstemp(prefix=".fleet-config-restore-", dir=home)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            os.chmod(temporary, 0o600)
            os.replace(temporary, config_path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    else:
        try:
            os.unlink(config_path)
        except FileNotFoundError:
            pass
    try:
        os.unlink(os.path.join(home, ".fleet-messaging-ready.json"))
    except FileNotFoundError:
        pass`

type messagingYAMLSettings struct {
	TelegramRequireMention         bool   `json:"telegram_require_mention"`
	WhatsAppUnauthorizedDMBehavior string `json:"whatsapp_unauthorized_dm_behavior"`
	WhatsAppReplyPrefix            string `json:"whatsapp_reply_prefix"`
	ConfigurationRevision          string `json:"configuration_revision"`
}

type messagingYAMLObservation struct {
	TelegramRequireMention         *bool  `json:"telegram_require_mention"`
	WhatsAppUnauthorizedDMBehavior string `json:"whatsapp_unauthorized_dm_behavior"`
	WhatsAppReplyPrefix            string `json:"whatsapp_reply_prefix"`
	SchemaVersion                  int    `json:"schema_version"`
	ConfigurationRevision          string `json:"configuration_revision"`
}

type messagingConfigSnapshot struct {
	Exists bool   `json:"exists"`
	Data   string `json:"data"`
}

var messagingEnvironmentKeys = []string{
	"TELEGRAM_BOT_TOKEN",
	"TELEGRAM_ALLOWED_USERS",
	"TELEGRAM_GROUP_ALLOWED_USERS",
	"TELEGRAM_GROUP_ALLOWED_CHATS",
	"TELEGRAM_PROXY",
	"WHATSAPP_ENABLED",
	"WHATSAPP_MODE",
	"WHATSAPP_ALLOWED_USERS",
}

func (p *Provisioner) configureMessaging(
	ctx context.Context,
	payload domain.MessagingApplyPayload,
	secret []byte,
) domain.JobResult {
	status := payload.DesiredStatus
	failureResult := func(message string, err error) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		return domain.JobResult{Success: false, Error: message, InstanceStatus: status}
	}
	managedPath, _, err := p.validateMessagingTarget(ctx, payload)
	if err != nil {
		return failureResult("messaging configuration refused", err)
	}
	if len(secret) == 0 {
		return failureResult("messaging configuration is unavailable", nil)
	}
	var config domain.MessagingConfiguration
	if err := json.Unmarshal(secret, &config); err != nil {
		return failureResult("messaging configuration is invalid", err)
	}
	config, err = messaging.NormalizeAndValidate(config)
	if err != nil {
		return failureResult("messaging configuration is invalid", err)
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return failureResult("messaging configuration could not be encoded", err)
	}
	defer clearSensitiveBytes(canonical)
	digest := sha256.Sum256(canonical)
	if payload.Revision != hex.EncodeToString(digest[:]) {
		return failureResult("messaging configuration revision does not match the leased job", nil)
	}

	envPath := filepath.Join(managedPath, ".env")
	manifestPath := filepath.Join(managedPath, "compose.yaml")
	originalEnv, err := os.ReadFile(envPath)
	if err != nil {
		return failureResult("managed messaging environment could not be read", err)
	}
	defer clearSensitiveBytes(originalEnv)
	originalManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return failureResult("managed Compose manifest could not be read", err)
	}
	snapshotCommand := p.messagingPythonCommand(payload, "", messagingConfigSnapshotScript)
	snapshotOutput, err := p.docker(ctx, snapshotCommand...)
	if err != nil {
		return failureResult("Hermes messaging configuration could not be snapshotted", errors.New(safeCommandError(err, snapshotOutput)))
	}
	var configSnapshot messagingConfigSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(snapshotOutput)), &configSnapshot); err != nil {
		return failureResult("Hermes messaging configuration snapshot is invalid", err)
	}
	snapshotJSON, err := json.Marshal(configSnapshot)
	if err != nil {
		return failureResult("Hermes messaging configuration snapshot could not be encoded", err)
	}
	defer clearSensitiveBytes(snapshotJSON)

	environment := messagingEnvironment(config)
	updatedEnv, err := updateEnvContentWithKeys(originalEnv, environment, messagingEnvironmentKeys)
	if err != nil {
		return failureResult("managed messaging environment is invalid", err)
	}
	defer clearSensitiveBytes(updatedEnv)
	updatedManifest := []byte(renderCompose(domain.ProvisionPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: payload.Image,
		Provider: payload.Provider, Model: payload.Model, Reasoning: payload.Reasoning,
		ServiceTier: payload.ServiceTier, APIPort: payload.APIPort, DashboardPort: payload.DashboardPort,
	}, payload.ProjectName, payload.DataVolume))
	yamlSettings := messagingYAMLSettings{
		TelegramRequireMention:         config.Telegram.RequireMention,
		WhatsAppUnauthorizedDMBehavior: config.WhatsApp.UnauthorizedDMBehavior,
		WhatsAppReplyPrefix:            config.WhatsApp.ReplyPrefix,
		ConfigurationRevision:          payload.Revision,
	}
	settingsJSON, err := json.Marshal(yamlSettings)
	if err != nil {
		return failureResult("Hermes messaging settings could not be encoded", err)
	}
	defer clearSensitiveBytes(settingsJSON)

	changed := false
	rollback := func() error {
		if !changed {
			return nil
		}
		rollbackContext, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		var rollbackErrors []string
		if err := writeAtomicReplaceContext(rollbackContext, envPath, originalEnv, 0o600); err != nil {
			rollbackErrors = append(rollbackErrors, "environment: "+err.Error())
		}
		if err := writeAtomicReplaceContext(rollbackContext, manifestPath, originalManifest, 0o600); err != nil {
			rollbackErrors = append(rollbackErrors, "manifest: "+err.Error())
		}
		restoreCommand := p.messagingPythonCommand(payload, "", messagingConfigRestoreScript)
		if output, err := p.dockerInput(rollbackContext, bytes.NewReader(snapshotJSON), restoreCommand...); err != nil {
			rollbackErrors = append(rollbackErrors, "Hermes config: "+safeCommandError(err, output))
		}
		if status == domain.InstanceRunning {
			output, err := p.compose(rollbackContext, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
			if err == nil {
				err = p.waitForHealth(rollbackContext, payload.APIPort, 120*time.Second)
			}
			if err == nil {
				err = p.waitForDashboard(rollbackContext, payload.DashboardPort, 60*time.Second)
			}
			if err != nil {
				rollbackErrors = append(rollbackErrors, "runtime: "+safeCommandError(err, output))
			}
		}
		if len(rollbackErrors) > 0 {
			return errors.New(strings.Join(rollbackErrors, "; "))
		}
		return nil
	}
	failedAfterChange := func(message string, err error) domain.JobResult {
		result := failureResult(message, err)
		if rollbackErr := rollback(); rollbackErr != nil {
			result.Error += "; automatic rollback failed: " + rollbackErr.Error()
			result.InstanceStatus = domain.InstanceFailed
		} else {
			result.Error += "; previous messaging configuration was restored"
		}
		return result
	}

	if err := writeAtomicReplaceContext(ctx, envPath, updatedEnv, 0o600); err != nil {
		return failureResult("managed messaging environment could not be written", err)
	}
	changed = true
	if err := writeAtomicReplaceContext(ctx, manifestPath, updatedManifest, 0o600); err != nil {
		return failedAfterChange("managed Compose manifest could not be written", err)
	}
	if output, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--quiet"); err != nil {
		return failedAfterChange("managed messaging environment does not produce a valid Compose manifest", errors.New(safeCommandError(err, output)))
	}
	applyCommand := p.messagingPythonCommand(payload, "", messagingConfigApplyScript)
	if output, err := p.dockerInput(ctx, bytes.NewReader(settingsJSON), applyCommand...); err != nil {
		return failedAfterChange("Hermes messaging settings could not be written", errors.New(safeCommandError(err, output)))
	}
	verifyCommand := p.messagingPythonCommand(payload, "", messagingConfigVerifyScript)
	verifyOutput, err := p.dockerInput(ctx, bytes.NewReader(settingsJSON), verifyCommand...)
	if err != nil {
		return failedAfterChange("Hermes messaging settings could not be verified", errors.New(safeCommandError(err, verifyOutput)))
	}
	if err := verifyMessagingYAMLObservation(verifyOutput, yamlSettings); err != nil {
		return failedAfterChange("Hermes messaging settings could not be verified", err)
	}

	if status == domain.InstanceRunning {
		output, err := p.compose(ctx, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
		if err != nil {
			return failedAfterChange("Hermes could not be restarted with the messaging configuration", errors.New(safeCommandError(err, output)))
		}
		if err := p.waitForHealth(ctx, payload.APIPort, 120*time.Second); err != nil {
			return failedAfterChange("Hermes health check failed after applying messaging configuration", err)
		}
		if err := p.waitForDashboard(ctx, payload.DashboardPort, 60*time.Second); err != nil {
			return failedAfterChange("Hermes Dashboard did not recover after applying messaging configuration", err)
		}
		verifyOutput, err = p.composeInput(
			ctx, bytes.NewReader(settingsJSON), managedPath, payload.ProjectName,
			"exec", "-T", "hermes", "python", "-c", messagingConfigVerifyScript,
		)
		if err != nil {
			return failedAfterChange("restarted Hermes messaging settings could not be verified", errors.New(safeCommandError(err, verifyOutput)))
		}
		if err := verifyMessagingYAMLObservation(verifyOutput, yamlSettings); err != nil {
			return failedAfterChange("restarted Hermes messaging settings could not be verified", err)
		}
	}
	changed = false
	return domain.JobResult{
		Success: true, Message: "Hermes messaging configuration applied",
		InstanceStatus: status,
	}
}

const mcpConfigSnapshotScript = `import base64, json, os
home = os.environ.get("HERMES_HOME", "/data")
result = {}
for name in ("config.yaml", ".env"):
    path = os.path.join(home, name)
    try:
        with open(path, "rb") as handle:
            result[name] = {"exists": True, "data": base64.b64encode(handle.read()).decode("ascii")}
    except FileNotFoundError:
        result[name] = {"exists": False, "data": ""}
print(json.dumps(result, sort_keys=True, separators=(",", ":")))`

const mcpConfigApplyScript = `import fcntl, hashlib, json, os, tempfile, yaml
settings = json.load(__import__("sys").stdin)
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
config_path = os.path.join(home, "config.yaml")
env_path = os.path.join(home, ".env")
lock_path = os.path.join(home, ".fleet-mcp-config.lock")
begin = "# BEGIN HERMES FLEET MCP"
end = "# END HERMES FLEET MCP"

def atomic_write(path, data):
    descriptor, temporary = tempfile.mkstemp(prefix=".fleet-mcp-", dir=home)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)

with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    try:
        with open(config_path, encoding="utf-8") as handle:
            raw = yaml.safe_load(handle) or {}
    except FileNotFoundError:
        raw = {}
    if not isinstance(raw, dict):
        raise RuntimeError("Hermes configuration root must be a mapping")
    try:
        with open(env_path, encoding="utf-8") as handle:
            existing_env = handle.read()
    except FileNotFoundError:
        existing_env = ""
    before, separator, remainder = existing_env.partition(begin)
    if separator:
        _, closing, after = remainder.partition(end)
        if not closing:
            raise RuntimeError("Fleet MCP environment block is incomplete")
        existing_env = before.rstrip("\n") + ("\n" if before.strip() else "") + after.lstrip("\n")
    managed_env = []
    mcp_servers = {}
    for server in settings["servers"]:
        entry = {
            "url": server["url"],
            "enabled": bool(server["enabled"]),
            "supports_parallel_tool_calls": False,
            "trust": "untrusted",
            "sampling": {"enabled": False},
            "elicitation": {"enabled": False},
            "tools": {"include": server["tools"], "resources": False, "prompts": False},
        }
        if server["auth_type"] == "bearer":
            variable = "HERMES_FLEET_MCP_" + hashlib.sha256(server["name"].encode()).hexdigest()[:16].upper() + "_TOKEN"
            managed_env.append(variable + "=" + json.dumps(server["bearer_token"]))
            entry["headers"] = {"Authorization": "Bearer ${" + variable + "}"}
        mcp_servers[server["name"]] = entry
    raw["mcp_servers"] = mcp_servers
    block = begin + "\n" + "\n".join(managed_env) + ("\n" if managed_env else "") + end + "\n"
    env_data = existing_env.rstrip("\n")
    if env_data:
        env_data += "\n"
    env_data += block
    atomic_write(env_path, env_data)
    atomic_write(config_path, yaml.safe_dump(raw, sort_keys=False, allow_unicode=True))
    marker = {"schema_version": 1, "configuration_revision": settings["configuration_revision"], "server_count": len(mcp_servers)}
    atomic_write(os.path.join(home, ".fleet-mcp-ready.json"), json.dumps(marker, sort_keys=True, separators=(",", ":")) + "\n")`

const mcpConfigVerifyScript = `import json, os, sys, yaml
settings = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
with open(os.path.join(home, "config.yaml"), encoding="utf-8") as handle:
    raw = yaml.safe_load(handle) or {}
servers = raw.get("mcp_servers", {}) if isinstance(raw, dict) else {}
if not isinstance(servers, dict):
    raise RuntimeError("mcp_servers is not a mapping")
for expected in settings["servers"]:
    actual = servers.get(expected["name"])
    if not isinstance(actual, dict) or actual.get("url") != expected["url"] or bool(actual.get("enabled")) != bool(expected["enabled"]):
        raise RuntimeError("MCP server configuration does not match")
    tools = actual.get("tools", {})
    if not isinstance(tools, dict) or tools.get("include") != expected["tools"] or tools.get("resources") is not False or tools.get("prompts") is not False:
        raise RuntimeError("MCP tool allowlist does not match")
    if actual.get("trust") != "untrusted" or actual.get("sampling", {}).get("enabled") is not False or actual.get("elicitation", {}).get("enabled") is not False:
        raise RuntimeError("MCP server-initiated capabilities are not disabled")
    headers = actual.get("headers", {})
    if expected["auth_type"] == "bearer" and (not isinstance(headers, dict) or not str(headers.get("Authorization", "")).startswith("Bearer ${HERMES_FLEET_MCP_")):
        raise RuntimeError("MCP bearer token reference is missing")
try:
    with open(os.path.join(home, ".fleet-mcp-ready.json"), encoding="utf-8") as handle:
        marker = json.load(handle)
except (FileNotFoundError, json.JSONDecodeError, OSError):
    marker = {}
print(json.dumps({"schema_version": marker.get("schema_version"), "configuration_revision": marker.get("configuration_revision"), "server_count": len(servers)}, sort_keys=True, separators=(",", ":")))`

const mcpConfigRestoreScript = `import base64, fcntl, json, os, sys, tempfile
snapshot = json.load(sys.stdin)
home = os.environ.get("HERMES_HOME", "/data")
os.makedirs(home, exist_ok=True)
lock_path = os.path.join(home, ".fleet-mcp-config.lock")
with open(lock_path, "a+", encoding="utf-8") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    for name in ("config.yaml", ".env"):
        path = os.path.join(home, name)
        item = snapshot.get(name, {})
        if item.get("exists"):
            data = base64.b64decode(item["data"], validate=True)
            descriptor, temporary = tempfile.mkstemp(prefix=".fleet-mcp-restore-", dir=home)
            try:
                with os.fdopen(descriptor, "wb") as handle:
                    handle.write(data)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.chmod(temporary, 0o600)
                os.replace(temporary, path)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        else:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass
    try:
        os.unlink(os.path.join(home, ".fleet-mcp-ready.json"))
    except FileNotFoundError:
        pass`

type mcpRuntimeSettings struct {
	Servers               []domain.MCPServerConfiguration `json:"servers"`
	ConfigurationRevision string                          `json:"configuration_revision"`
}

type mcpRuntimeObservation struct {
	SchemaVersion         int    `json:"schema_version"`
	ConfigurationRevision string `json:"configuration_revision"`
	ServerCount           int    `json:"server_count"`
}

func (p *Provisioner) configureMCP(
	ctx context.Context,
	payload domain.MCPApplyPayload,
	secret []byte,
	report func(context.Context, domain.JobProgress) error,
) domain.JobResult {
	status := payload.DesiredStatus
	fail := func(message string, err error) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		return domain.JobResult{Success: false, Error: message, InstanceStatus: status}
	}
	if report == nil {
		return fail("MCP configuration requires a progress-capable Host Agent", nil)
	}
	if err := report(ctx, domain.JobProgress{Stage: "VALIDATING", Detail: "Validating the Fleet-owned MCP definition"}); err != nil {
		return fail("MCP progress could not be recorded", err)
	}
	managedPath, hermesContainer, err := p.validateMessagingTarget(ctx, domain.MessagingApplyPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: payload.Image, ImageID: payload.ImageID,
		Provider: payload.Provider, Model: payload.Model, Reasoning: payload.Reasoning, ServiceTier: payload.ServiceTier,
		ProjectName: payload.ProjectName, DataVolume: payload.DataVolume, ManagedPath: payload.ManagedPath,
		DesiredStatus: payload.DesiredStatus, APIPort: payload.APIPort, DashboardPort: payload.DashboardPort, Revision: payload.Revision,
	})
	if err != nil {
		return fail("MCP configuration refused", err)
	}
	if len(secret) == 0 {
		return fail("MCP configuration is unavailable", nil)
	}
	var config domain.MCPConfiguration
	if err := json.Unmarshal(secret, &config); err != nil {
		return fail("MCP configuration is invalid", err)
	}
	config, err = mcpconfig.NormalizeAndValidate(config)
	if err != nil {
		return fail("MCP configuration is invalid", err)
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return fail("MCP configuration could not be encoded", err)
	}
	defer clearSensitiveBytes(canonical)
	digest := sha256.Sum256(canonical)
	if payload.Revision != hex.EncodeToString(digest[:]) {
		return fail("MCP configuration revision does not match the leased job", nil)
	}

	snapshotCommand := p.mcpPythonCommand(payload, hermesContainer, mcpConfigSnapshotScript)
	snapshotOutput, err := p.docker(ctx, snapshotCommand...)
	if err != nil {
		return fail("Hermes MCP configuration could not be snapshotted", errors.New(safeCommandError(err, snapshotOutput)))
	}
	if !json.Valid([]byte(strings.TrimSpace(snapshotOutput))) {
		return fail("Hermes MCP configuration snapshot is invalid", nil)
	}
	snapshotJSON := []byte(strings.TrimSpace(snapshotOutput))
	defer clearSensitiveBytes(snapshotJSON)
	settings := mcpRuntimeSettings{Servers: config.Servers, ConfigurationRevision: payload.Revision}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return fail("MCP runtime settings could not be encoded", err)
	}
	defer clearSensitiveBytes(settingsJSON)

	changed := false
	rollback := func() error {
		if !changed {
			return nil
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 180*time.Second)
		defer cancel()
		restoreCommand := p.mcpPythonCommand(payload, "", mcpConfigRestoreScript)
		if output, restoreErr := p.dockerInput(rollbackContext, bytes.NewReader(snapshotJSON), restoreCommand...); restoreErr != nil {
			return errors.New(safeCommandError(restoreErr, output))
		}
		if status == domain.InstanceRunning {
			if output, restartErr := p.compose(rollbackContext, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate"); restartErr != nil {
				return errors.New(safeCommandError(restartErr, output))
			}
			if healthErr := p.waitForHealth(rollbackContext, payload.APIPort, 120*time.Second); healthErr != nil {
				return healthErr
			}
			if dashboardErr := p.waitForDashboard(rollbackContext, payload.DashboardPort, 60*time.Second); dashboardErr != nil {
				return dashboardErr
			}
		}
		return nil
	}
	failedAfterChange := func(message string, failureErr error) domain.JobResult {
		result := fail(message, failureErr)
		if rollbackErr := rollback(); rollbackErr != nil {
			result.Error += "; automatic rollback failed: " + rollbackErr.Error()
			result.InstanceStatus = domain.InstanceFailed
		} else {
			result.Error += "; previous MCP configuration was restored"
		}
		return result
	}

	if err := report(ctx, domain.JobProgress{Stage: "WRITING_CONFIGURATION", Detail: "Writing encrypted references and the MCP tool allowlist"}); err != nil {
		return fail("MCP progress could not be recorded", err)
	}
	applyCommand := p.mcpPythonCommand(payload, hermesContainer, mcpConfigApplyScript)
	changed = true
	if output, applyErr := p.dockerInput(ctx, bytes.NewReader(settingsJSON), applyCommand...); applyErr != nil {
		return failedAfterChange("Hermes MCP configuration could not be written", errors.New(safeCommandError(applyErr, output)))
	}
	verifyCommand := p.mcpPythonCommand(payload, hermesContainer, mcpConfigVerifyScript)
	verifyOutput, verifyErr := p.dockerInput(ctx, bytes.NewReader(settingsJSON), verifyCommand...)
	if verifyErr != nil {
		return failedAfterChange("Hermes MCP configuration could not be verified", errors.New(safeCommandError(verifyErr, verifyOutput)))
	}
	var observation mcpRuntimeObservation
	if json.Unmarshal([]byte(strings.TrimSpace(verifyOutput)), &observation) != nil || observation.SchemaVersion != 1 ||
		observation.ConfigurationRevision != payload.Revision || observation.ServerCount != len(config.Servers) {
		return failedAfterChange("Hermes MCP configuration could not be verified", errors.New("readiness marker or server count does not match"))
	}

	if status == domain.InstanceRunning {
		if err := report(ctx, domain.JobProgress{Stage: "RESTARTING_RUNTIME", Detail: "Restarting Hermes with the updated MCP configuration"}); err != nil {
			return failedAfterChange("MCP progress could not be recorded", err)
		}
		if output, restartErr := p.compose(ctx, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate"); restartErr != nil {
			return failedAfterChange("Hermes could not be restarted with the MCP configuration", errors.New(safeCommandError(restartErr, output)))
		}
		if healthErr := p.waitForHealth(ctx, payload.APIPort, 120*time.Second); healthErr != nil {
			return failedAfterChange("Hermes health check failed after applying MCP configuration", healthErr)
		}
		if dashboardErr := p.waitForDashboard(ctx, payload.DashboardPort, 60*time.Second); dashboardErr != nil {
			return failedAfterChange("Hermes Dashboard did not recover after applying MCP configuration", dashboardErr)
		}
	}
	if err := report(ctx, domain.JobProgress{Stage: "TESTING_CONNECTIONS", Detail: "Testing every enabled MCP server"}); err != nil {
		return failedAfterChange("MCP progress could not be recorded", err)
	}
	for _, server := range config.Servers {
		if !server.Enabled {
			continue
		}
		if output, testErr := p.testMCPServer(ctx, payload, managedPath, server.Name); testErr != nil {
			_ = output
			return failedAfterChange("MCP server "+server.Name+" connection test failed", nil)
		}
	}
	if err := report(ctx, domain.JobProgress{Stage: "VERIFYING_TOOLS", Detail: "Confirming the applied MCP tool allowlists"}); err != nil {
		return failedAfterChange("MCP progress could not be recorded", err)
	}
	changed = false
	return domain.JobResult{Success: true, Message: "Hermes MCP configuration applied and verified", InstanceStatus: status}
}

func (p *Provisioner) mcpPythonCommand(payload domain.MCPApplyPayload, hermesContainer, script string) []string {
	if hermesContainer != "" {
		return []string{"exec", "-i", hermesContainer, "python", "-c", script}
	}
	return []string{
		"run", "--rm", "-i", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "0:0", "-e", "HERMES_HOME=/data", "-v", payload.DataVolume + ":/data",
		"--entrypoint", "python", payload.ImageID, "-c", script,
	}
}

func (p *Provisioner) testMCPServer(ctx context.Context, payload domain.MCPApplyPayload, managedPath, serverName string) (string, error) {
	if payload.DesiredStatus == domain.InstanceRunning {
		return p.compose(ctx, managedPath, payload.ProjectName, "exec", "-T", "hermes", "hermes", "mcp", "test", serverName)
	}
	return p.docker(ctx,
		"run", "--rm", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "0:0",
		"-e", "HERMES_HOME=/data", "-v", payload.DataVolume+":/data", "--entrypoint", "hermes", payload.ImageID,
		"mcp", "test", serverName,
	)
}

func messagingEnvironment(config domain.MessagingConfiguration) map[string]string {
	values := map[string]string{
		"TELEGRAM_BOT_TOKEN":           config.Telegram.BotToken,
		"TELEGRAM_ALLOWED_USERS":       strings.Join(config.Telegram.AllowedUsers, ","),
		"TELEGRAM_GROUP_ALLOWED_USERS": strings.Join(config.Telegram.GroupAllowedUsers, ","),
		"TELEGRAM_GROUP_ALLOWED_CHATS": strings.Join(config.Telegram.GroupAllowedChats, ","),
		"TELEGRAM_PROXY":               config.Telegram.ProxyURL,
		"WHATSAPP_ENABLED":             strconv.FormatBool(config.WhatsApp.Enabled),
		"WHATSAPP_MODE":                config.WhatsApp.Mode,
		"WHATSAPP_ALLOWED_USERS":       strings.Join(config.WhatsApp.AllowedUsers, ","),
	}
	if !config.Telegram.Enabled {
		values["TELEGRAM_BOT_TOKEN"] = ""
		values["TELEGRAM_ALLOWED_USERS"] = ""
		values["TELEGRAM_GROUP_ALLOWED_USERS"] = ""
		values["TELEGRAM_GROUP_ALLOWED_CHATS"] = ""
		values["TELEGRAM_PROXY"] = ""
	}
	if !config.WhatsApp.Enabled {
		values["WHATSAPP_ALLOWED_USERS"] = ""
	}
	return values
}

func verifyMessagingYAMLObservation(output string, expected messagingYAMLSettings) error {
	var observation messagingYAMLObservation
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &observation); err != nil {
		return errors.New("Hermes returned invalid messaging readiness data")
	}
	if observation.TelegramRequireMention == nil ||
		*observation.TelegramRequireMention != expected.TelegramRequireMention ||
		observation.WhatsAppUnauthorizedDMBehavior != expected.WhatsAppUnauthorizedDMBehavior ||
		observation.WhatsAppReplyPrefix != expected.WhatsAppReplyPrefix ||
		observation.SchemaVersion != 1 ||
		observation.ConfigurationRevision != expected.ConfigurationRevision {
		return errors.New("Hermes messaging readiness marker or effective settings do not match")
	}
	return nil
}

func (p *Provisioner) messagingPythonCommand(
	payload domain.MessagingApplyPayload,
	hermesContainer string,
	script string,
) []string {
	if hermesContainer != "" {
		return []string{"exec", "-i", hermesContainer, "python", "-c", script}
	}
	return []string{
		"run", "--rm", "-i", "--network", "none", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0",
		"-e", "HERMES_HOME=/data", "-v", payload.DataVolume + ":/data",
		"--entrypoint", "python", payload.ImageID, "-c", script,
	}
}

func (p *Provisioner) validateMessagingTarget(
	ctx context.Context,
	payload domain.MessagingApplyPayload,
) (string, string, error) {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return "", "", errors.New("invalid Fleet instance identity")
	}
	if payload.DesiredStatus != domain.InstanceRunning && payload.DesiredStatus != domain.InstanceStopped {
		return "", "", errors.New("runtime state does not accept messaging configuration")
	}
	if err := providers.ValidateImageReference(payload.Image); err != nil {
		return "", "", err
	}
	if !imageIDPattern.MatchString(payload.ImageID) || !sha256HexPattern.MatchString(payload.Revision) {
		return "", "", errors.New("runtime image or messaging revision identity is invalid")
	}
	if payload.APIPort < 1024 || payload.APIPort > 65535 ||
		payload.DashboardPort < 1024 || payload.DashboardPort > 65535 ||
		payload.APIPort == payload.DashboardPort {
		return "", "", errors.New("runtime ports are invalid")
	}
	expectedProject, expectedVolume, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	if payload.ProjectName != expectedProject || payload.DataVolume != expectedVolume {
		return "", "", errors.New("managed resource names do not match the Fleet identity")
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return "", "", err
	}
	if managedPath != filepath.Join(p.root, expectedDirectory) {
		return "", "", errors.New("managed path does not match the Fleet identity")
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return "", "", errors.New("required instance file is missing or unsafe")
		}
	}
	composeImages, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--images")
	if err != nil {
		return "", "", errors.New("managed Compose image references could not be verified")
	}
	imageReferences := nonEmptyLines(composeImages)
	if len(imageReferences) != 2 || imageReferences[0] != payload.Image || imageReferences[1] != payload.Image {
		return "", "", errors.New("managed Compose services do not use the desired image reference")
	}
	volumeProject, err := p.docker(ctx, "volume", "inspect", "--format", `{{ index .Labels "com.docker.compose.project" }}`, payload.DataVolume)
	if err != nil || strings.TrimSpace(volumeProject) != payload.ProjectName {
		return "", "", errors.New("data volume is missing or is not owned by the exact Fleet project")
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return "", "", err
	}
	if len(containers) != 2 {
		return "", "", errors.New("the exact two Fleet-owned containers are required")
	}
	hermesContainer := ""
	for _, container := range containers {
		if container.Image != payload.ImageID {
			return "", "", errors.New("container image differs from the provisioned immutable image")
		}
		state := container.State.Status
		if payload.DesiredStatus == domain.InstanceRunning && state != "running" {
			return "", "", errors.New("both Fleet containers must be running")
		}
		if payload.DesiredStatus == domain.InstanceStopped && state != "exited" && state != "created" {
			return "", "", errors.New("both Fleet containers must remain stopped")
		}
		service := container.Config.Labels["com.docker.compose.service"]
		if service != "hermes" && service != "dashboard" {
			return "", "", errors.New("unexpected Fleet-owned Compose service")
		}
		if service == "hermes" {
			hermesContainer = container.ID
		}
	}
	if payload.DesiredStatus == domain.InstanceRunning && !containerIDPattern.MatchString(hermesContainer) {
		return "", "", errors.New("Hermes container identity is invalid")
	}
	if immutableID, inspectErr := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.ImageID); inspectErr != nil || strings.TrimSpace(immutableID) != payload.ImageID {
		return "", "", errors.New("provisioned immutable image is unavailable")
	}
	return managedPath, hermesContainer, nil
}

func clearSensitiveBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (p *Provisioner) validateRuntimeSyncTarget(ctx context.Context, payload domain.RuntimeSyncPayload) (string, string, error) {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return "", "", errors.New("invalid Fleet instance identity")
	}
	if payload.DesiredStatus != domain.InstanceRunning && payload.DesiredStatus != domain.InstanceStopped {
		return "", "", errors.New("runtime state is not synchronizable")
	}
	if err := providers.ValidateImageReference(payload.Image); err != nil {
		return "", "", err
	}
	if !imageIDPattern.MatchString(payload.ImageID) {
		return "", "", errors.New("runtime image identity is invalid")
	}
	if err := providers.ValidateRuntime(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier); err != nil {
		return "", "", err
	}
	if payload.DashboardPort < 1024 || payload.DashboardPort > 65535 {
		return "", "", errors.New("dashboard port is invalid")
	}
	expectedProject, expectedVolume, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	if payload.ProjectName != expectedProject || payload.DataVolume != expectedVolume {
		return "", "", errors.New("managed resource names do not match the Fleet identity")
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return "", "", err
	}
	if managedPath != filepath.Join(p.root, expectedDirectory) {
		return "", "", errors.New("managed path does not match the Fleet identity")
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return "", "", errors.New("required instance file is missing or unsafe")
		}
	}
	composeImages, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--images")
	if err != nil {
		return "", "", errors.New("managed Compose image references could not be verified")
	}
	imageReferences := nonEmptyLines(composeImages)
	if len(imageReferences) != 2 || imageReferences[0] != payload.Image || imageReferences[1] != payload.Image {
		return "", "", errors.New("managed Compose services do not use the desired image reference")
	}
	volumeProject, err := p.docker(ctx, "volume", "inspect", "--format", `{{ index .Labels "com.docker.compose.project" }}`, payload.DataVolume)
	if err != nil || strings.TrimSpace(volumeProject) != payload.ProjectName {
		return "", "", errors.New("data volume is missing or is not owned by the exact Fleet project")
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return "", "", err
	}
	hermesContainer := ""
	for _, container := range containers {
		if container.Image != payload.ImageID {
			return "", "", errors.New("container image differs from the provisioned immutable image")
		}
		if payload.DesiredStatus == domain.InstanceRunning && container.State.Status != "running" {
			return "", "", errors.New("both Fleet containers must be running")
		}
		if payload.DesiredStatus == domain.InstanceStopped && container.State.Status != "exited" && container.State.Status != "created" {
			return "", "", errors.New("both Fleet containers must remain stopped")
		}
		if container.Config.Labels["com.docker.compose.service"] == "hermes" {
			hermesContainer = container.ID
		}
	}
	if payload.DesiredStatus == domain.InstanceRunning && !containerIDPattern.MatchString(hermesContainer) {
		return "", "", errors.New("Hermes container identity is invalid")
	}
	if immutableID, inspectErr := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.ImageID); inspectErr != nil || strings.TrimSpace(immutableID) != payload.ImageID {
		return "", "", errors.New("provisioned immutable image is unavailable")
	}
	return managedPath, hermesContainer, nil
}

func (p *Provisioner) inspectRuntimeImageCapability(
	ctx context.Context,
	imageID string,
) (runtimeImageCapability, error) {
	if !imageIDPattern.MatchString(imageID) {
		return runtimeImageCapability{}, errors.New("runtime image identity is invalid")
	}
	output, err := p.docker(ctx, "image", "inspect", "--format", "{{json .Config.Labels}}", imageID)
	if err != nil {
		return runtimeImageCapability{}, errors.New(safeCommandError(err, output))
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &labels); err != nil {
		return runtimeImageCapability{}, errors.New("runtime image returned invalid Fleet metadata")
	}
	schemaVersion, err := strconv.Atoi(strings.TrimSpace(labels["io.hermes-fleet.runtime-config-schema"]))
	if err != nil || !compatibility.SupportsRuntimeSchema(schemaVersion) {
		return runtimeImageCapability{}, errors.New("runtime image uses an unsupported Fleet configuration schema")
	}
	buildID := strings.TrimSpace(labels["io.hermes-fleet.runtime-build-id"])
	if !sha256HexPattern.MatchString(buildID) {
		return runtimeImageCapability{}, errors.New("runtime image does not expose a valid Fleet wrapper build identity")
	}
	return runtimeImageCapability{SchemaVersion: schemaVersion, BuildID: buildID}, nil
}

func (p *Provisioner) runtimeVolumeCommand(payload domain.RuntimeSyncPayload, executable string, args ...string) []string {
	return p.runtimeVolumeCommandWithInput(payload, false, executable, args...)
}

func (p *Provisioner) runtimeVolumeInputCommand(payload domain.RuntimeSyncPayload, executable string, args ...string) []string {
	return p.runtimeVolumeCommandWithInput(payload, true, executable, args...)
}

func (p *Provisioner) runtimeVolumeCommandWithInput(
	payload domain.RuntimeSyncPayload,
	attachInput bool,
	executable string,
	args ...string,
) []string {
	command := []string{
		"run", "--rm",
	}
	if attachInput {
		command = append(command, "-i")
	}
	command = append(command,
		"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "0:0",
		"-e", "HERMES_HOME=/data",
		"-e", "HERMES_INFERENCE_PROVIDER="+payload.Provider,
		"-e", "HERMES_INFERENCE_MODEL="+payload.Model,
		"-e", "HERMES_REASONING_EFFORT="+payload.Reasoning,
		"-e", "HERMES_SERVICE_TIER="+payload.ServiceTier,
		"-v", payload.DataVolume+":/data",
		"--entrypoint", executable, payload.ImageID,
	)
	return append(command, args...)
}

func (p *Provisioner) snapshotRuntimeConfiguration(
	ctx context.Context,
	payload domain.RuntimeSyncPayload,
) (runtimeConfigSnapshot, []byte, error) {
	output, err := p.docker(ctx, p.runtimeVolumeCommand(payload, "python", "-c", runtimeConfigSnapshotScript)...)
	if err != nil {
		return runtimeConfigSnapshot{}, nil, errors.New(safeCommandError(err, output))
	}
	var snapshot runtimeConfigSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &snapshot); err != nil {
		return runtimeConfigSnapshot{}, nil, errors.New("Hermes returned an invalid runtime configuration snapshot")
	}
	for _, file := range []runtimeConfigFileSnapshot{snapshot.Config, snapshot.Marker} {
		if !file.Exists && file.Data != "" {
			return runtimeConfigSnapshot{}, nil, errors.New("Hermes returned an inconsistent runtime configuration snapshot")
		}
		if file.Exists {
			if _, err := base64.StdEncoding.DecodeString(file.Data); err != nil {
				return runtimeConfigSnapshot{}, nil, errors.New("Hermes returned an invalid runtime configuration snapshot")
			}
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return runtimeConfigSnapshot{}, nil, errors.New("Hermes runtime configuration snapshot could not be encoded")
	}
	return snapshot, encoded, nil
}

func (p *Provisioner) restoreRuntimeConfiguration(
	ctx context.Context,
	payload domain.RuntimeSyncPayload,
	snapshot []byte,
) error {
	output, err := p.dockerInput(
		ctx, bytes.NewReader(snapshot),
		p.runtimeVolumeInputCommand(payload, "python", "-c", runtimeConfigRestoreScript)...,
	)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	return nil
}

func (p *Provisioner) applyRuntimeConfig(
	ctx context.Context,
	prefix []string,
	payload domain.RuntimeSyncPayload,
	capability runtimeImageCapability,
) error {
	args := append(
		append([]string(nil), prefix...),
		"-c", runtimeStateApply,
		payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
		strconv.Itoa(capability.SchemaVersion), capability.BuildID,
	)
	output, err := p.docker(ctx, args...)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	return nil
}

func runtimeConfigurationRevision(provider, model, reasoning, serviceTier string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{provider, model, reasoning, serviceTier}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func legacyRuntimeConfigurationRevision(provider, model string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{provider, model}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func verifyRuntimeState(
	output, provider, model, reasoning, serviceTier string,
	capability *runtimeImageCapability,
) error {
	var observation runtimeStateObservation
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &observation); err != nil {
		return errors.New("Hermes returned invalid runtime readiness data")
	}
	if observation.Model["default"] != model || observation.Model["provider"] != provider {
		return errors.New("Hermes did not persist the Fleet-owned provider and model")
	}
	if observation.Environment.Provider != provider || observation.Environment.Model != model {
		return errors.New("Hermes container environment does not match the Fleet-owned provider and model")
	}
	expectedReasoning, expectedServiceTier := reasoning, serviceTier
	if expectedReasoning == "" && expectedServiceTier == "" {
		expectedReasoning = observation.Environment.Reasoning
		expectedServiceTier = observation.Environment.ServiceTier
	}
	if expectedReasoning == "" || expectedServiceTier == "" {
		return errors.New("Hermes reasoning and service tier are not completely configured")
	}
	if observation.Environment.Reasoning != expectedReasoning ||
		observation.Environment.ServiceTier != expectedServiceTier {
		return errors.New("Hermes container environment does not match the Fleet reasoning and service tier")
	}
	if observation.Agent["reasoning_effort"] != expectedReasoning ||
		observation.Agent["service_tier"] != expectedServiceTier {
		return errors.New("Hermes did not activate the Fleet reasoning and service tier")
	}
	if observation.State == nil ||
		observation.State.Provider != provider ||
		observation.State.Model != model ||
		!sha256HexPattern.MatchString(strings.TrimSpace(observation.State.RuntimeBuildID)) {
		return errors.New("Hermes Fleet runtime readiness marker is missing or stale")
	}
	schemaVersion := observation.State.SchemaVersion
	if capability != nil {
		if !compatibility.SupportsRuntimeSchema(capability.SchemaVersion) {
			return errors.New("runtime image uses an unsupported Fleet configuration schema")
		}
		if schemaVersion != capability.SchemaVersion ||
			observation.State.RuntimeBuildID != capability.BuildID {
			return errors.New("Hermes Fleet runtime readiness marker does not match the runtime image")
		}
	} else if !compatibility.SupportsRuntimeSchema(schemaVersion) {
		return errors.New("Hermes Fleet runtime readiness marker uses an unsupported schema")
	}
	expectedRevision := legacyRuntimeConfigurationRevision(provider, model)
	if schemaVersion == runtimeStateSchemaVersion {
		expectedRevision = runtimeConfigurationRevision(provider, model, expectedReasoning, expectedServiceTier)
		if observation.State.Reasoning != expectedReasoning ||
			observation.State.ServiceTier != expectedServiceTier {
			return errors.New("Hermes Fleet runtime readiness marker has stale reasoning or service tier")
		}
	}
	if observation.State.ConfigurationRevision != expectedRevision {
		return errors.New("Hermes Fleet runtime readiness marker has a stale configuration revision")
	}
	return nil
}

func (p *Provisioner) verifyRuntimeConfig(
	ctx context.Context,
	args []string,
	provider, model, reasoning, serviceTier string,
	capability runtimeImageCapability,
) error {
	output, err := p.docker(ctx, args...)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	return verifyRuntimeState(output, provider, model, reasoning, serviceTier, &capability)
}

func (p *Provisioner) verifyManagedRuntimeReady(
	ctx context.Context,
	managedPath, projectName, provider, model, reasoning, serviceTier string,
	capability runtimeImageCapability,
) error {
	output, err := p.compose(ctx, managedPath, projectName, "exec", "-T", "hermes", "python", "-c", runtimeStateProbe)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	return verifyRuntimeState(output, provider, model, reasoning, serviceTier, &capability)
}

func (p *Provisioner) ensureManagedRuntimeReady(
	ctx context.Context,
	managedPath, projectName, provider, model, reasoning, serviceTier, imageID string,
	dashboardPort int,
) error {
	return p.ensureManagedRuntimeReadyWithTimeout(
		ctx, managedPath, projectName, provider, model, reasoning, serviceTier,
		imageID, dashboardPort, 60*time.Second,
	)
}

func (p *Provisioner) ensureManagedRuntimeReadyWithTimeout(
	ctx context.Context,
	managedPath, projectName, provider, model, reasoning, serviceTier, imageID string,
	dashboardPort int,
	dashboardTimeout time.Duration,
) error {
	capability, err := p.inspectRuntimeImageCapability(ctx, imageID)
	if err != nil {
		return fmt.Errorf("inspect Fleet runtime wrapper: %w", err)
	}
	if err := p.verifyManagedRuntimeReady(
		ctx, managedPath, projectName, provider, model, reasoning, serviceTier, capability,
	); err == nil {
		if err := p.waitForDashboard(ctx, dashboardPort, dashboardTimeout); err != nil {
			return fmt.Errorf("verify Hermes Dashboard readiness: %w", err)
		}
		return nil
	}
	output, applyErr := p.compose(
		ctx, managedPath, projectName, "exec", "-T", "hermes", "python", "-c",
		runtimeStateApply, provider, model, reasoning, serviceTier,
		strconv.Itoa(capability.SchemaVersion), capability.BuildID,
	)
	if applyErr != nil {
		return fmt.Errorf("apply Fleet runtime migration: %s", safeCommandError(applyErr, output))
	}
	if output, recreateErr := p.compose(
		ctx, managedPath, projectName,
		"up", "-d", "--remove-orphans", "--force-recreate", "hermes", "dashboard",
	); recreateErr != nil {
		return fmt.Errorf("recreate Hermes services after migration: %s", safeCommandError(recreateErr, output))
	}
	if err := p.waitForDashboard(ctx, dashboardPort, dashboardTimeout); err != nil {
		return fmt.Errorf("verify Hermes Dashboard readiness: %w", err)
	}
	if err := p.verifyManagedRuntimeReady(
		ctx, managedPath, projectName, provider, model, reasoning, serviceTier, capability,
	); err != nil {
		return fmt.Errorf("verify Fleet runtime migration: %w", err)
	}
	return nil
}

func (p *Provisioner) runStreamingDocker(ctx context.Context, args []string, onLine func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, p.dockerPath, args...)
	reader, writer := io.Pipe()
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return err
	}
	waited := make(chan error, 1)
	go func() {
		err := command.Wait()
		_ = writer.Close()
		waited <- err
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		if err := onLine(scanner.Text()); err != nil {
			_ = command.Process.Kill()
			_ = reader.Close()
			<-waited
			return err
		}
	}
	scanErr := scanner.Err()
	waitErr := <-waited
	_ = reader.Close()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

func (p *Provisioner) reconcileImage(ctx context.Context, payload domain.ImageReconcilePayload) domain.JobResult {
	actualImageID, _, err := p.verifyImageCandidate(ctx, payload, false)
	if err != nil {
		return domain.JobResult{Success: false, Error: "image reconciliation refused: " + err.Error(), InstanceStatus: domain.InstanceStopped}
	}
	return domain.JobResult{
		Success: true, Message: "Stopped runtime image metadata reconciled",
		ImageID: actualImageID, InstanceStatus: domain.InstanceStopped,
	}
}

func (p *Provisioner) repairImage(ctx context.Context, payload domain.ImageRepairPayload) domain.JobResult {
	originalStatus := domain.InstanceStopped
	if payload.Restart {
		originalStatus = domain.InstanceRunning
	}
	reconcilePayload := domain.ImageReconcilePayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: payload.Image,
		PreviousImageID: payload.PreviousImageID, ProjectName: payload.ProjectName,
		DataVolume: payload.DataVolume, ManagedPath: payload.ManagedPath,
	}
	actualImageID, managedPath, err := p.verifyImageCandidate(ctx, reconcilePayload, payload.Restart)
	if err != nil {
		return domain.JobResult{Success: false, Error: "image repair preflight failed: " + err.Error(), InstanceStatus: originalStatus}
	}
	if !payload.Restart {
		return domain.JobResult{
			Success: true, Message: "Stopped runtime image drift fixed",
			ImageID: actualImageID, InstanceStatus: domain.InstanceStopped,
		}
	}

	stopOutput, err := p.compose(ctx, managedPath, payload.ProjectName, "stop")
	if err != nil {
		return domain.JobResult{
			Success: false, Error: "image repair could not stop the runtime: " + safeCommandError(err, stopOutput),
			InstanceStatus: domain.InstanceFailed,
		}
	}

	actualImageID, _, err = p.verifyImageCandidate(ctx, reconcilePayload, false)
	if err != nil {
		return p.failedImageRepairWithRestore(ctx, payload, managedPath, "image repair verification failed after stop: "+err.Error())
	}
	startOutput, err := p.compose(ctx, managedPath, payload.ProjectName, "start")
	if err != nil {
		return domain.JobResult{
			Success: false, Error: "image repair verified the image but could not restart the runtime: " + safeCommandError(err, startOutput),
			InstanceStatus: domain.InstanceFailed,
		}
	}
	if err := p.waitForHealth(ctx, payload.APIPort, 120*time.Second); err != nil {
		return domain.JobResult{
			Success: false, Error: "image repair restarted the runtime but health verification failed: " + err.Error(),
			InstanceStatus: domain.InstanceFailed,
		}
	}
	return domain.JobResult{
		Success: true, Message: "Image drift fixed and runtime restored",
		ImageID: actualImageID, InstanceStatus: domain.InstanceRunning,
	}
}

func (p *Provisioner) upgradeHermes(ctx context.Context, payload domain.HermesUpgradePayload, artifactPath string) domain.JobResult {
	failureResult := func(message string, err error, status string) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		return domain.JobResult{Success: false, Error: message, InstanceStatus: status}
	}
	if err := p.validateHermesUpgradePayload(payload); err != nil {
		return failureResult("Hermes update refused", err, domain.InstanceStopped)
	}
	if err := verifyRestoreArtifact(artifactPath, payload.Rollback.RecoverySizeBytes, payload.Rollback.RecoverySHA256); err != nil {
		return failureResult("Hermes update backup is unavailable", err, domain.InstanceStopped)
	}
	if targetImageID, err := p.verifyHermesUpdateTarget(ctx, payload); err == nil {
		if p.verifyAlreadyUpdated(ctx, payload, targetImageID) == nil {
			return domain.JobResult{
				Success: true, Message: "Hermes update was already installed and verified",
				ImageID: targetImageID, InstanceStatus: domain.InstanceStopped,
			}
		}
	}
	if err := p.ensureCurrentImageReference(ctx, payload); err != nil {
		return failureResult("Hermes update preflight failed", err, domain.InstanceStopped)
	}
	managedPath, err := p.verifyStoppedUpgradeSource(ctx, payload)
	if err != nil {
		return failureResult("Hermes update preflight failed", err, domain.InstanceStopped)
	}
	targetImageID, err := p.verifyHermesUpdateTarget(ctx, payload)
	if err != nil {
		return failureResult("Hermes update target failed verification", err, domain.InstanceStopped)
	}
	if targetImageID == payload.CurrentImageID {
		return failureResult("Hermes update target resolves to the current immutable image", nil, domain.InstanceStopped)
	}

	manifest := renderCompose(domain.ProvisionPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: payload.TargetImage,
		Provider: payload.Provider, Model: payload.Model, Reasoning: payload.Reasoning,
		ServiceTier: payload.ServiceTier, APIPort: payload.APIPort, DashboardPort: payload.DashboardPort,
	}, payload.ProjectName, payload.DataVolume)
	if err := writeAtomicReplaceContext(ctx, filepath.Join(managedPath, "compose.yaml"), []byte(manifest), 0o600); err != nil {
		return failureResult("Hermes update manifest could not be written", err, domain.InstanceStopped)
	}

	updateErr := func() error {
		output, err := p.compose(ctx, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
		if err != nil {
			return errors.New(safeCommandError(err, output))
		}
		if err := p.waitForHealth(ctx, payload.APIPort, 150*time.Second); err != nil {
			return err
		}
		if payload.CodexConfigured {
			if err := p.ensureManagedRuntimeReady(
				ctx, managedPath, payload.ProjectName,
				payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
				targetImageID, payload.DashboardPort,
			); err != nil {
				return err
			}
		} else if err := p.waitForDashboard(ctx, payload.DashboardPort, 60*time.Second); err != nil {
			return err
		}
		output, err = p.compose(ctx, managedPath, payload.ProjectName, "stop")
		if err != nil {
			return errors.New(safeCommandError(err, output))
		}
		return p.verifyStoppedUpgradeTarget(ctx, payload, targetImageID)
	}()
	if updateErr == nil {
		return domain.JobResult{
			Success: true, Message: "Hermes updated to " + payload.TargetVersion + "; instance remains stopped",
			ImageID: targetImageID, InstanceStatus: domain.InstanceStopped,
		}
	}
	return p.rollbackHermesUpgrade(ctx, payload, artifactPath, updateErr)
}

func (p *Provisioner) prepareHermesImage(ctx context.Context, payload domain.HermesUpgradePayload) (string, error) {
	p.imageBuildMu.Lock()
	defer p.imageBuildMu.Unlock()

	if !hermesVersionRef.MatchString(payload.TargetVersion) || !hermesCommitRef.MatchString(payload.TargetSource) ||
		providers.ValidateImageReference(payload.TargetImage) != nil {
		return "", errors.New("target Hermes release identity is invalid")
	}
	imageID, verificationErr := p.verifyHermesUpdateTarget(ctx, payload)
	if verificationErr == nil {
		return imageID, nil
	}
	existingOutput, err := p.docker(ctx, "image", "ls", "--quiet", "--no-trunc", payload.TargetImage)
	if err != nil {
		return "", fmt.Errorf("inspect target Hermes image reference: %s", safeCommandError(err, existingOutput))
	}
	if len(nonEmptyLines(existingOutput)) > 0 {
		return "", fmt.Errorf(
			"target Hermes image reference already exists but failed immutable release verification: %w",
			verificationErr,
		)
	}
	buildContext, err := os.MkdirTemp(p.root, ".runtime-build-")
	if err != nil {
		return "", fmt.Errorf("create runtime build context: %w", err)
	}
	defer os.RemoveAll(buildContext)
	if err := os.Chmod(buildContext, 0o700); err != nil {
		return "", fmt.Errorf("secure runtime build context: %w", err)
	}
	if err := runtimeassets.WriteBuildContext(buildContext); err != nil {
		return "", err
	}
	output, err := p.docker(
		ctx, "build", "--pull",
		"--build-arg", "HERMES_VERSION="+payload.TargetVersion,
		"--build-arg", "HERMES_REF="+payload.TargetSource,
		"--build-arg", "RUNTIME_BUILD_ID="+runtimeassets.BuildID(),
		"-t", payload.TargetImage,
		"-f", filepath.Join(buildContext, "runtime", "Dockerfile"),
		buildContext,
	)
	if err != nil {
		return "", errors.New(safeCommandError(err, output))
	}
	return p.verifyHermesUpdateTarget(ctx, payload)
}

func (p *Provisioner) ensureCurrentImageReference(ctx context.Context, payload domain.HermesUpgradePayload) error {
	output, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.CurrentImageID)
	if err != nil || strings.TrimSpace(output) != payload.CurrentImageID {
		return errors.New("current immutable image is unavailable for rollback")
	}
	output, err = p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.CurrentImage)
	if err == nil && strings.TrimSpace(output) == payload.CurrentImageID {
		return nil
	}
	output, err = p.docker(ctx, "image", "tag", payload.CurrentImageID, payload.CurrentImage)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	return nil
}

func (p *Provisioner) verifyAlreadyUpdated(ctx context.Context, payload domain.HermesUpgradePayload, targetImageID string) error {
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return err
	}
	images, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--images")
	if err != nil {
		return err
	}
	refs := nonEmptyLines(images)
	if len(refs) != 2 || refs[0] != payload.TargetImage || refs[1] != payload.TargetImage {
		return errors.New("managed Compose services do not use the target image")
	}
	return p.verifyStoppedUpgradeTarget(ctx, payload, targetImageID)
}

func (p *Provisioner) validateHermesUpgradePayload(payload domain.HermesUpgradePayload) error {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) ||
		!imageIDPattern.MatchString(payload.CurrentImageID) || !recoveryIDPattern.MatchString(payload.RecoveryPointID) {
		return errors.New("invalid Fleet instance or backup identity")
	}
	if providers.ValidateImageReference(payload.CurrentImage) != nil || providers.ValidateImageReference(payload.TargetImage) != nil ||
		payload.TargetImage == payload.CurrentImage || !hermesVersionRef.MatchString(payload.TargetVersion) ||
		!hermesCommitRef.MatchString(payload.TargetSource) {
		return errors.New("invalid current or target Hermes release")
	}
	if providers.ValidateRuntimeOrPending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier) != nil {
		return errors.New("invalid instance runtime configuration")
	}
	if payload.APIPort < 1024 || payload.APIPort > 65535 || payload.DashboardPort < 1024 || payload.DashboardPort > 65535 ||
		payload.APIPort == payload.DashboardPort {
		return errors.New("invalid instance ports")
	}
	expectedProject, expectedVolume, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	if payload.ProjectName != expectedProject || payload.DataVolume != expectedVolume || filepath.Base(payload.ManagedPath) != expectedDirectory {
		return errors.New("managed resource identity does not match the Fleet instance")
	}
	rollback := payload.Rollback
	if payload.RecoveryPointID != rollback.RecoveryPointID || rollback.InstanceID != payload.InstanceID || rollback.Name != payload.Name ||
		rollback.Image != payload.CurrentImage || rollback.ImageID != payload.CurrentImageID || !rollback.RequireImageID || rollback.Provider != payload.Provider ||
		rollback.Model != payload.Model || rollback.Reasoning != payload.Reasoning || rollback.ServiceTier != payload.ServiceTier ||
		rollback.CodexConfigured != payload.CodexConfigured || rollback.ProjectName != payload.ProjectName ||
		rollback.DataVolume != payload.DataVolume || rollback.ManagedPath != payload.ManagedPath {
		return errors.New("rollback backup does not match the exact pre-update instance")
	}
	return nil
}

func (p *Provisioner) verifyStoppedUpgradeSource(ctx context.Context, payload domain.HermesUpgradePayload) (string, error) {
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return "", err
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return "", errors.New("required instance file is missing or unsafe")
		}
	}
	images, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--images")
	if err != nil {
		return "", errors.New("managed Compose image references could not be verified")
	}
	refs := nonEmptyLines(images)
	if len(refs) != 2 || refs[0] != payload.CurrentImage || refs[1] != payload.CurrentImage {
		return "", errors.New("managed Compose services do not use the recorded current image")
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return "", err
	}
	for _, container := range containers {
		if container.State.Status != "exited" && container.State.Status != "created" {
			return "", errors.New("both Fleet containers must remain stopped")
		}
		if container.Image != payload.CurrentImageID {
			return "", errors.New("stopped containers do not use the recorded immutable image")
		}
	}
	volumeProject, err := p.docker(ctx, "volume", "inspect", "--format", `{{ index .Labels "com.docker.compose.project" }}`, payload.DataVolume)
	if err != nil || strings.TrimSpace(volumeProject) != payload.ProjectName {
		return "", errors.New("data volume is missing or is not owned by the exact Fleet project")
	}
	imageID, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.CurrentImageID)
	if err != nil || strings.TrimSpace(imageID) != payload.CurrentImageID {
		return "", errors.New("current immutable image is unavailable for rollback")
	}
	resolvedCurrentImageID, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.CurrentImage)
	if err != nil || strings.TrimSpace(resolvedCurrentImageID) != payload.CurrentImageID {
		return "", errors.New("current versioned image reference no longer resolves to the recorded immutable image")
	}
	return managedPath, nil
}

func (p *Provisioner) verifyHermesUpdateTarget(ctx context.Context, payload domain.HermesUpgradePayload) (string, error) {
	output, err := p.docker(
		ctx, "image", "inspect", "--format",
		`{{.Id}}{{"\n"}}{{ index .Config.Labels "io.hermes-fleet.hermes-version" }}{{"\n"}}{{ index .Config.Labels "io.hermes-fleet.hermes-ref" }}{{"\n"}}{{ index .Config.Labels "io.hermes-fleet.runtime-build-id" }}`,
		payload.TargetImage,
	)
	if err != nil {
		return "", errors.New(safeCommandError(err, output))
	}
	parts := nonEmptyLines(output)
	if len(parts) != 4 || !imageIDPattern.MatchString(parts[0]) {
		return "", errors.New("target image does not expose immutable Fleet release metadata")
	}
	if parts[1] != payload.TargetVersion || parts[2] != payload.TargetSource {
		return "", errors.New("target image labels do not match the pinned Fleet release")
	}
	if parts[3] != runtimeassets.BuildID() {
		return "", errors.New("target image runtime wrapper does not match this Fleet build")
	}
	return parts[0], nil
}

func (p *Provisioner) verifyStoppedUpgradeTarget(ctx context.Context, payload domain.HermesUpgradePayload, targetImageID string) error {
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.State.Status != "exited" && container.State.Status != "created" {
			return errors.New("updated Fleet containers did not return to the stopped state")
		}
		if container.Image != targetImageID {
			return errors.New("updated Fleet containers do not use the verified target image")
		}
	}
	return nil
}

func (p *Provisioner) rollbackHermesUpgrade(
	ctx context.Context,
	payload domain.HermesUpgradePayload,
	artifactPath string,
	updateErr error,
) domain.JobResult {
	rollbackCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	managedPath, pathErr := p.safeManagedPath(payload.ManagedPath)
	if pathErr == nil {
		_, _ = p.compose(rollbackCtx, managedPath, payload.ProjectName, "stop")
	}
	restored := p.restoreRecoveryPoint(rollbackCtx, payload.Rollback, artifactPath)
	if !restored.Success {
		return domain.JobResult{
			Success: false, Error: "Hermes update failed: " + updateErr.Error() + "; automatic backup restore failed: " + restored.Error,
			InstanceStatus: domain.InstanceFailed,
		}
	}
	managedPath, pathErr = p.safeManagedPath(payload.ManagedPath)
	if pathErr != nil {
		return domain.JobResult{Success: false, Error: "Hermes update failed and restored data, but the managed path is invalid", InstanceStatus: domain.InstanceFailed}
	}
	output, err := p.compose(rollbackCtx, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
	if err == nil {
		err = p.waitForHealth(rollbackCtx, payload.APIPort, 150*time.Second)
	}
	if err == nil {
		if payload.CodexConfigured {
			err = p.ensureManagedRuntimeReady(
				rollbackCtx, managedPath, payload.ProjectName,
				payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
				payload.CurrentImageID, payload.DashboardPort,
			)
		} else {
			err = p.waitForDashboard(rollbackCtx, payload.DashboardPort, 60*time.Second)
		}
	}
	if err == nil {
		output, err = p.compose(rollbackCtx, managedPath, payload.ProjectName, "stop")
	}
	if err == nil {
		_, err = p.verifyStoppedUpgradeSource(rollbackCtx, payload)
	}
	if err != nil {
		return domain.JobResult{
			Success: false, Error: "Hermes update failed: " + updateErr.Error() + "; backup was restored but original runtime verification failed: " + safeCommandError(err, output),
			InstanceStatus: domain.InstanceFailed,
		}
	}
	return domain.JobResult{
		Success: false, Error: "Hermes update failed: " + updateErr.Error() + "; verified backup and original runtime were restored",
		InstanceStatus: domain.InstanceStopped,
	}
}

func (p *Provisioner) failedImageRepairWithRestore(
	ctx context.Context,
	payload domain.ImageRepairPayload,
	managedPath, message string,
) domain.JobResult {
	rollbackCtx, cancelRollback := context.WithTimeout(ctx, 150*time.Second)
	defer cancelRollback()
	output, err := p.compose(rollbackCtx, managedPath, payload.ProjectName, "start")
	if err == nil {
		err = p.waitForHealth(rollbackCtx, payload.APIPort, 120*time.Second)
	}
	if err != nil {
		return domain.JobResult{
			Success: false, Error: message + "; restoring the original running state failed: " + safeCommandError(err, output),
			InstanceStatus: domain.InstanceFailed,
		}
	}
	return domain.JobResult{Success: false, Error: message + "; original running state was restored", InstanceStatus: domain.InstanceRunning}
}

func (p *Provisioner) verifyImageCandidate(ctx context.Context, payload domain.ImageReconcilePayload, requireRunning bool) (string, string, error) {
	failed := func(message string, err error) (string, string, error) {
		if err != nil {
			message += ": " + err.Error()
		}
		return "", "", errors.New(message)
	}
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return failed("invalid Fleet instance identity", nil)
	}
	if providers.ValidateImageReference(payload.Image) != nil || !imageIDPattern.MatchString(payload.PreviousImageID) {
		return failed("invalid immutable image metadata", nil)
	}
	shortID := strings.ReplaceAll(payload.InstanceID, "-", "")[:8]
	expectedProject := "hermes-fleet-" + payload.Name + "-" + shortID
	if payload.ProjectName != expectedProject || payload.DataVolume != expectedProject+"-data" {
		return failed("managed resource names do not match the Fleet identity", nil)
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failed("unsafe managed path", err)
	}
	if managedPath != filepath.Join(p.root, payload.Name+"-"+shortID) {
		return failed("managed path does not match the Fleet identity", nil)
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return failed("required instance file is missing or unsafe", err)
		}
	}

	composeImages, err := p.compose(ctx, managedPath, payload.ProjectName, "config", "--images")
	if err != nil {
		return failed("managed Compose image references could not be verified", errors.New(safeCommandError(err, composeImages)))
	}
	imageReferences := nonEmptyLines(composeImages)
	if len(imageReferences) != 2 || imageReferences[0] != payload.Image || imageReferences[1] != payload.Image {
		return failed("managed Compose services do not use the desired image reference", nil)
	}

	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return failed("Fleet containers are not safe to reconcile", err)
	}
	actualImageID := ""
	for _, container := range containers {
		if requireRunning && container.State.Status != "running" {
			return failed("all Fleet containers must be running before automatic image repair", nil)
		}
		if !requireRunning && container.State.Status != "exited" && container.State.Status != "created" {
			return failed("all Fleet containers must remain stopped during image reconciliation", nil)
		}
		if !imageIDPattern.MatchString(container.Image) {
			return failed("container does not expose a valid immutable image ID", nil)
		}
		if actualImageID == "" {
			actualImageID = container.Image
		} else if container.Image != actualImageID {
			return failed("Fleet containers do not use the same immutable image", nil)
		}
	}
	volumeProject, err := p.docker(ctx, "volume", "inspect", "--format", `{{ index .Labels "com.docker.compose.project" }}`, payload.DataVolume)
	if err != nil || strings.TrimSpace(volumeProject) != payload.ProjectName {
		return failed("data volume is missing or is not owned by the exact Fleet project", err)
	}
	resolvedImageID, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.Image)
	if err != nil || strings.TrimSpace(resolvedImageID) != actualImageID {
		return failed("Fleet containers do not match the desired image reference", err)
	}
	immutableImageID, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", actualImageID)
	if err != nil || strings.TrimSpace(immutableImageID) != actualImageID {
		return failed("verified immutable image is unavailable", err)
	}
	return actualImageID, managedPath, nil
}

func (p *Provisioner) inspectOwnedFleetContainers(ctx context.Context, instanceID, projectName string) ([]observedContainer, error) {
	containerOutput, err := p.docker(
		ctx, "ps", "-a",
		"--filter", "label=io.hermes-fleet.managed=true",
		"--filter", "label=io.hermes-fleet.instance-id="+instanceID,
		"--format", "{{.ID}}",
	)
	if err != nil {
		return nil, errors.New(safeCommandError(err, containerOutput))
	}
	containerIDs := nonEmptyLines(containerOutput)
	if len(containerIDs) != 2 {
		return nil, errors.New("exactly two Fleet containers are required")
	}
	for _, containerID := range containerIDs {
		if !containerIDPattern.MatchString(containerID) {
			return nil, errors.New("Docker returned an invalid container identity")
		}
	}
	inspectOutput, err := p.docker(ctx, append([]string{"inspect"}, containerIDs...)...)
	if err != nil {
		return nil, errors.New(safeCommandError(err, inspectOutput))
	}
	var containers []observedContainer
	if err := json.Unmarshal([]byte(inspectOutput), &containers); err != nil || len(containers) != 2 {
		if err == nil {
			err = errors.New("Docker returned an unexpected number of containers")
		}
		return nil, fmt.Errorf("invalid container inspection data: %w", err)
	}
	services := make(map[string]bool, 2)
	for _, container := range containers {
		labels := container.Config.Labels
		service := labels["com.docker.compose.service"]
		if labels["io.hermes-fleet.managed"] != "true" ||
			labels["io.hermes-fleet.instance-id"] != instanceID ||
			labels["com.docker.compose.project"] != projectName ||
			(service != "hermes" && service != "dashboard") || services[service] {
			return nil, errors.New("container ownership does not match the exact Fleet instance")
		}
		services[service] = true
	}
	if len(services) != 2 {
		return nil, errors.New("container ownership does not include both expected Fleet services")
	}
	return containers, nil
}

func (p *Provisioner) provisionRetryOwnsPorts(ctx context.Context, payload domain.ProvisionPayload, projectName string) (bool, error) {
	containerOutput, err := p.docker(
		ctx, "ps", "-a",
		"--filter", "label=io.hermes-fleet.managed=true",
		"--filter", "label=io.hermes-fleet.instance-id="+payload.InstanceID,
		"--format", "{{.ID}}",
	)
	if err != nil {
		return false, errors.New(safeCommandError(err, containerOutput))
	}
	containerIDs := nonEmptyLines(containerOutput)
	if len(containerIDs) == 0 {
		return false, nil
	}
	if len(containerIDs) != 2 {
		return false, errors.New("retry found an incomplete Fleet container set")
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, projectName)
	if err != nil {
		return false, err
	}
	expected := map[string]struct {
		containerPort string
		hostPort      int
	}{
		"hermes": {
			containerPort: "8642/tcp",
			hostPort:      payload.APIPort,
		},
		"dashboard": {
			containerPort: "9119/tcp",
			hostPort:      payload.DashboardPort,
		},
	}
	for _, container := range containers {
		service := container.Config.Labels["com.docker.compose.service"]
		serviceExpected, ok := expected[service]
		if !ok {
			return false, errors.New("retry found an unexpected Fleet service")
		}
		bindings := container.HostConfig.PortBindings[serviceExpected.containerPort]
		if len(bindings) != 1 ||
			bindings[0].HostIP != "127.0.0.1" ||
			bindings[0].HostPort != strconv.Itoa(serviceExpected.hostPort) {
			return false, fmt.Errorf("retry port ownership does not match the %s service", service)
		}
	}
	return true, nil
}

func (p *Provisioner) createRecoveryPoint(ctx context.Context, payload domain.RecoveryPointPayload) domain.JobResult {
	failed := func(message string, err error) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		return domain.JobResult{Success: false, Error: message, RecoveryPointID: payload.RecoveryPointID}
	}
	if !recoveryIDPattern.MatchString(payload.RecoveryPointID) || !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return failed("invalid recovery point identity", nil)
	}
	if !imageIDPattern.MatchString(payload.ImageID) || providers.ValidateImageReference(payload.Image) != nil {
		return failed("invalid immutable image metadata", nil)
	}
	if payload.ProjectName == "" || !strings.HasPrefix(payload.ProjectName, "hermes-fleet-") || payload.DataVolume != payload.ProjectName+"-data" {
		return failed("invalid Fleet-owned resource names", nil)
	}
	if payload.AgentVersion == "" || payload.CreatedAt.IsZero() || payload.MaxBytes < 1 {
		return failed("incomplete recovery point metadata", nil)
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failed("unsafe managed path", err)
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			return failed("required instance file is missing or unsafe", err)
		}
	}
	running, err := p.compose(ctx, managedPath, payload.ProjectName, "ps", "--status", "running", "-q")
	if err != nil {
		return failed("could not verify stopped instance state", errors.New(safeCommandError(err, running)))
	}
	if strings.TrimSpace(running) != "" {
		return failed("instance containers are still running; recovery point was refused", nil)
	}
	containerOutput, err := p.compose(ctx, managedPath, payload.ProjectName, "ps", "--all", "-q")
	if err != nil {
		return failed("could not inspect stopped instance containers", errors.New(safeCommandError(err, containerOutput)))
	}
	containerIDs := nonEmptyLines(containerOutput)
	if len(containerIDs) == 0 {
		return failed("stopped instance containers are missing", nil)
	}
	for _, containerID := range containerIDs {
		if !containerIDPattern.MatchString(containerID) {
			return failed("Docker returned an invalid container identity", nil)
		}
		containerImageID, err := p.docker(ctx, "inspect", "--format", "{{.Image}}", containerID)
		if err != nil || strings.TrimSpace(containerImageID) != payload.ImageID {
			return failed("stopped container image differs from the provisioned immutable image", err)
		}
	}
	volumeProject, err := p.docker(ctx, "volume", "inspect", "--format", `{{ index .Labels "com.docker.compose.project" }}`, payload.DataVolume)
	if err != nil || strings.TrimSpace(volumeProject) != payload.ProjectName {
		return failed("data volume is missing or is not owned by the exact Fleet project", err)
	}
	imageID, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.ImageID)
	if err != nil || strings.TrimSpace(imageID) != payload.ImageID {
		return failed("immutable recovery image is unavailable", err)
	}
	volumeEstimate, err := p.estimateVolumeArchiveSize(ctx, payload.DataVolume, payload.ImageID, payload.MaxBytes)
	if err != nil {
		return failed("estimate recovery storage", err)
	}
	workspaceEstimate, err := estimateWorkspaceArchiveSize(ctx, managedPath, payload.MaxBytes)
	if err != nil {
		return failed("estimate recovery storage", err)
	}
	requiredBytes, err := recoveryCreationDiskRequirement(volumeEstimate, workspaceEstimate)
	if err != nil {
		return failed("estimate recovery storage", err)
	}
	if err := p.ensureDiskAvailable(requiredBytes); err != nil {
		return failed("recovery point preflight failed", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return failed("generate temporary recovery encryption key", err)
	}
	volumeFile, err := os.CreateTemp(p.root, ".recovery-volume-*.enc")
	if err != nil {
		return failed("create encrypted volume staging file", err)
	}
	volumePath := volumeFile.Name()
	defer os.Remove(volumePath)
	if err := volumeFile.Chmod(0o600); err != nil {
		volumeFile.Close()
		return failed("secure volume staging file", err)
	}
	volumeReader, volumeWait := p.dockerOutput(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "tar",
		"-v", payload.DataVolume+":/source:ro", payload.ImageID, "-C", "/source", "-cf", "-", ".",
	)
	volumeSize, volumeEncryptErr := recoverycodec.Encrypt(
		ctx, volumeFile, io.LimitReader(volumeReader, payload.MaxBytes+1), key, payload.RecoveryPointID+":volume",
	)
	_ = volumeReader.Close()
	volumeCommandErr := volumeWait()
	if syncErr := volumeFile.Sync(); volumeEncryptErr == nil && syncErr != nil {
		volumeEncryptErr = syncErr
	}
	if closeErr := volumeFile.Close(); volumeEncryptErr == nil && closeErr != nil {
		volumeEncryptErr = closeErr
	}
	if volumeEncryptErr != nil {
		return failed("encrypt data volume archive", volumeEncryptErr)
	}
	if volumeCommandErr != nil {
		return failed("export data volume", volumeCommandErr)
	}
	if volumeSize > payload.MaxBytes {
		return failed("data volume export exceeds the configured recovery point size limit", nil)
	}
	if volumeSize < 1 {
		return failed("data volume export was empty", nil)
	}

	artifact, err := os.CreateTemp(p.root, ".recovery-point-*.enc")
	if err != nil {
		return failed("create encrypted recovery artifact", err)
	}
	artifactPath := artifact.Name()
	cleanupArtifact := true
	defer func() {
		if cleanupArtifact {
			_ = os.Remove(artifactPath)
		}
	}()
	if err := artifact.Chmod(0o600); err != nil {
		artifact.Close()
		return failed("secure recovery artifact", err)
	}
	archiveReader, archiveWriter := io.Pipe()
	archiveDone := make(chan error, 1)
	go func() {
		err := p.writeRecoveryArchive(ctx, archiveWriter, payload, managedPath, volumePath, volumeSize, key)
		_ = archiveWriter.CloseWithError(err)
		archiveDone <- err
	}()
	hash := sha256.New()
	archiveSize, encryptErr := recoverycodec.Encrypt(
		ctx, artifact, io.LimitReader(io.TeeReader(archiveReader, hash), payload.MaxBytes+1), key, payload.RecoveryPointID+":artifact",
	)
	_ = archiveReader.Close()
	archiveErr := <-archiveDone
	if syncErr := artifact.Sync(); encryptErr == nil && syncErr != nil {
		encryptErr = syncErr
	}
	if closeErr := artifact.Close(); encryptErr == nil && closeErr != nil {
		encryptErr = closeErr
	}
	if encryptErr != nil {
		return failed("encrypt recovery point archive", encryptErr)
	}
	if archiveSize > payload.MaxBytes {
		return failed("recovery point archive exceeds the configured size limit", nil)
	}
	if archiveErr != nil {
		return failed("create recovery point archive", archiveErr)
	}
	if archiveSize < 1 {
		return failed("recovery point archive was empty", nil)
	}
	cleanupArtifact = false
	return domain.JobResult{
		Success: true, Message: "Encrypted recovery point is ready for upload", RecoveryPointID: payload.RecoveryPointID,
		RecoverySHA256: hex.EncodeToString(hash.Sum(nil)), RecoverySizeBytes: archiveSize,
		RecoveryArtifact: artifactPath, RecoveryKey: key,
	}
}

func (p *Provisioner) resolveRecoveryRuntimeImage(ctx context.Context, payload domain.RecoveryRestorePayload) (string, error) {
	output, err := p.docker(ctx, "image", "inspect", "--format", "{{json .}}", payload.Image)
	if err != nil {
		return "", errors.New(safeCommandError(err, output))
	}
	var image struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &image); err != nil {
		return "", errors.New("runtime image returned invalid Docker metadata")
	}
	if !imageIDPattern.MatchString(image.ID) {
		return "", errors.New("runtime image does not expose a valid local identity")
	}
	if payload.RequireImageID && image.ID != payload.ImageID {
		return "", errors.New("same-host rollback image no longer matches the recorded immutable identity")
	}
	version := strings.TrimSpace(image.Config.Labels["io.hermes-fleet.hermes-version"])
	source := strings.ToLower(strings.TrimSpace(image.Config.Labels["io.hermes-fleet.hermes-ref"]))
	buildID := strings.ToLower(strings.TrimSpace(image.Config.Labels["io.hermes-fleet.runtime-build-id"]))
	schemaVersion, schemaErr := strconv.Atoi(strings.TrimSpace(image.Config.Labels["io.hermes-fleet.runtime-config-schema"]))
	if !hermesVersionRef.MatchString(version) || !hermesCommitRef.MatchString(source) ||
		!sha256HexPattern.MatchString(buildID) || schemaErr != nil || !compatibility.SupportsRuntimeSchema(schemaVersion) {
		return "", errors.New("runtime image does not expose compatible Fleet release metadata")
	}
	if match := runtimeImageRef.FindStringSubmatch(payload.Image); len(match) == 4 {
		if version != match[1] || !strings.HasPrefix(source, match[2]) ||
			(match[3] != "" && !strings.HasPrefix(buildID, match[3])) {
			return "", errors.New("runtime image labels do not match the backup release identity")
		}
	}
	return image.ID, nil
}

func (p *Provisioner) restoreRecoveryPoint(ctx context.Context, payload domain.RecoveryRestorePayload, artifactPath string) domain.JobResult {
	failed := func(message string, err error, status string) domain.JobResult {
		if err != nil {
			message += ": " + err.Error()
		}
		return domain.JobResult{Success: false, Error: message, InstanceStatus: status}
	}
	if !recoveryIDPattern.MatchString(payload.RecoveryPointID) || !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return failed("invalid recovery restore identity", nil, domain.InstanceStopped)
	}
	if providers.ValidateImageReference(payload.Image) != nil || providers.ValidateRuntimeOrPending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier) != nil ||
		!imageIDPattern.MatchString(payload.ImageID) ||
		payload.RecoverySizeBytes < 1 || payload.MaxBytes < payload.RecoverySizeBytes || payload.CreatedAt.IsZero() {
		return failed("invalid recovery restore metadata", nil, domain.InstanceStopped)
	}
	expectedProject, expectedVolume, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	if payload.ProjectName != expectedProject || payload.DataVolume != expectedVolume || filepath.Base(payload.ManagedPath) != expectedDirectory {
		return failed("recovery point does not match the managed instance identity", nil, domain.InstanceStopped)
	}
	if err := verifyRestoreArtifact(artifactPath, payload.RecoverySizeBytes, payload.RecoverySHA256); err != nil {
		return failed("recovery point artifact is missing or unsafe", err, domain.InstanceStopped)
	}
	managedPath, workspaceExists, err := p.restoreManagedPath(payload.ManagedPath)
	if err != nil {
		return failed("unsafe managed path", err, domain.InstanceStopped)
	}
	volumeExists, err := p.restoreVolumeExists(ctx, payload)
	if err != nil {
		return failed("inspect restore data volume", err, domain.InstanceStopped)
	}
	if workspaceExists != volumeExists {
		return failed("clean-host restore requires both workspace and data volume to be present or both absent", nil, domain.InstanceStopped)
	}
	if workspaceExists {
		running, err := p.compose(ctx, managedPath, payload.ProjectName, "ps", "--status", "running", "-q")
		if err != nil || strings.TrimSpace(running) != "" {
			return failed("instance must remain stopped during restore", err, domain.InstanceStopped)
		}
	}
	runtimeImageID, err := p.resolveRecoveryRuntimeImage(ctx, payload)
	if err != nil {
		return failed("compatible recovery runtime is unavailable", err, domain.InstanceStopped)
	}
	var volumeEstimate int64
	if volumeExists {
		volumeEstimate, err = p.estimateVolumeArchiveSize(ctx, payload.DataVolume, runtimeImageID, payload.MaxBytes)
		if err != nil {
			return failed("estimate restore storage", err, domain.InstanceStopped)
		}
	} else {
		// A clean host has no rollback copy to measure. Reserve another full
		// recovery-artifact size for the extracted workspace and data volume.
		volumeEstimate = payload.RecoverySizeBytes
	}
	requiredBytes, err := restoreDiskRequirement(payload.RecoverySizeBytes, volumeEstimate)
	if err != nil {
		return failed("estimate restore storage", err, domain.InstanceStopped)
	}
	if err := p.ensureDiskAvailable(requiredBytes); err != nil {
		return failed("restore preflight failed", err, domain.InstanceStopped)
	}

	stagingRoot, err := os.MkdirTemp(p.root, ".restore-stage-")
	if err != nil {
		return failed("create restore staging directory", err, domain.InstanceStopped)
	}
	defer os.RemoveAll(stagingRoot)
	if err := os.Chmod(stagingRoot, 0o700); err != nil {
		return failed("secure restore staging directory", err, domain.InstanceStopped)
	}
	workspaceStage, volumeArchive, err := extractRestoreArchive(ctx, artifactPath, stagingRoot, payload)
	if err != nil {
		return failed("validate recovery point archive", err, domain.InstanceStopped)
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(workspaceStage, filename)); err != nil {
			return failed("restored instance files are incomplete", err, domain.InstanceStopped)
		}
	}

	if !workspaceExists {
		return p.restoreRecoveryPointToAbsentState(ctx, payload, runtimeImageID, managedPath, workspaceStage, volumeArchive, failed)
	}
	volumeBackup, err := p.exportVolume(ctx, payload.DataVolume, runtimeImageID, payload.MaxBytes)
	if err != nil {
		return failed("create pre-restore volume rollback copy", err, domain.InstanceStopped)
	}
	defer os.Remove(volumeBackup)
	suffix, err := randomSecret(8)
	if err != nil {
		return failed("create restore rollback identity", err, domain.InstanceStopped)
	}
	workspaceBackup := filepath.Join(p.root, "."+filepath.Base(managedPath)+".restore-backup-"+suffix)
	if err := ctx.Err(); err != nil {
		return failed("restore canceled before mutation", err, domain.InstanceStopped)
	}
	if err := os.Rename(managedPath, workspaceBackup); err != nil {
		return failed("stage current workspace for rollback", err, domain.InstanceStopped)
	}
	workspaceSwapped := true
	if err := os.Rename(workspaceStage, managedPath); err != nil {
		_ = os.Rename(workspaceBackup, managedPath)
		return failed("publish restored workspace", err, domain.InstanceStopped)
	}
	volumeMutated := false
	mutationErr := p.clearVolume(ctx, payload.DataVolume, runtimeImageID)
	if mutationErr == nil {
		volumeMutated = true
		mutationErr = p.importVolume(ctx, payload.DataVolume, runtimeImageID, volumeArchive)
	}
	if mutationErr == nil {
		if output, configErr := p.compose(ctx, managedPath, payload.ProjectName, "config", "--quiet"); configErr != nil {
			mutationErr = fmt.Errorf("restored Compose configuration is invalid: %s", safeCommandError(configErr, output))
		}
	}
	if mutationErr == nil {
		if output, stateErr := p.compose(ctx, managedPath, payload.ProjectName, "ps", "--status", "running", "-q"); stateErr != nil || strings.TrimSpace(output) != "" {
			mutationErr = errors.New("restore unexpectedly started instance containers")
		}
	}
	if mutationErr == nil {
		message := "Recovery point restored; instance remains stopped"
		if err := os.RemoveAll(workspaceBackup); err != nil {
			message += "; workspace rollback copy requires cleanup"
		}
		return domain.JobResult{Success: true, Message: message, ImageID: runtimeImageID, InstanceStatus: domain.InstanceStopped}
	}

	rollbackCtx, cancelRollback := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelRollback()
	rollbackErrors := make([]string, 0, 2)
	if volumeMutated {
		if err := p.replaceVolume(rollbackCtx, payload.DataVolume, runtimeImageID, volumeBackup); err != nil {
			rollbackErrors = append(rollbackErrors, "volume rollback failed: "+err.Error())
		}
	}
	if workspaceSwapped {
		if err := rollbackCtx.Err(); err != nil {
			rollbackErrors = append(rollbackErrors, "workspace rollback stopped: "+err.Error())
		} else if err := os.RemoveAll(managedPath); err != nil {
			rollbackErrors = append(rollbackErrors, "remove failed restored workspace: "+err.Error())
		} else if err := rollbackCtx.Err(); err != nil {
			rollbackErrors = append(rollbackErrors, "workspace rollback stopped: "+err.Error())
		} else if err := os.Rename(workspaceBackup, managedPath); err != nil {
			rollbackErrors = append(rollbackErrors, "workspace rollback failed: "+err.Error())
		}
	}
	if len(rollbackErrors) != 0 {
		return failed("restore failed and automatic rollback was incomplete", errors.New(strings.Join(rollbackErrors, "; ")), domain.InstanceFailed)
	}
	return failed("restore failed; original stopped state was restored", mutationErr, domain.InstanceStopped)
}

func (p *Provisioner) restoreRecoveryPointToAbsentState(
	ctx context.Context,
	payload domain.RecoveryRestorePayload,
	runtimeImageID string,
	managedPath, workspaceStage, volumeArchive string,
	failed func(string, error, string) domain.JobResult,
) domain.JobResult {
	if err := ctx.Err(); err != nil {
		return failed("restore canceled before clean-host mutation", err, domain.InstanceStopped)
	}
	output, err := p.docker(ctx, "volume", "create",
		"--label", "io.hermes-fleet.managed=true",
		"--label", "io.hermes-fleet.instance-id="+payload.InstanceID,
		"--label", "com.docker.compose.project="+payload.ProjectName,
		"--label", "com.docker.compose.volume=hermes-data",
		payload.DataVolume,
	)
	if err != nil || strings.TrimSpace(output) != payload.DataVolume {
		message := strings.TrimSpace(output)
		if message == "" && err != nil {
			message = err.Error()
		}
		if message == "" {
			message = "Docker did not return the expected volume identity"
		}
		return failed("create clean-host data volume", errors.New(message), domain.InstanceStopped)
	}
	volumeCreated := true
	workspacePublished := false
	mutationErr := os.Rename(workspaceStage, managedPath)
	if mutationErr == nil {
		workspacePublished = true
		mutationErr = p.importVolume(ctx, payload.DataVolume, runtimeImageID, volumeArchive)
	}
	if mutationErr == nil {
		if output, configErr := p.compose(ctx, managedPath, payload.ProjectName, "config", "--quiet"); configErr != nil {
			mutationErr = fmt.Errorf("restored Compose configuration is invalid: %s", safeCommandError(configErr, output))
		}
	}
	if mutationErr == nil {
		if output, stateErr := p.compose(ctx, managedPath, payload.ProjectName, "ps", "--status", "running", "-q"); stateErr != nil || strings.TrimSpace(output) != "" {
			mutationErr = errors.New("restore unexpectedly started instance containers")
		}
	}
	if mutationErr == nil {
		return domain.JobResult{Success: true, Message: "Recovery point restored onto clean host; instance remains stopped", ImageID: runtimeImageID, InstanceStatus: domain.InstanceStopped}
	}
	rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelRollback()
	rollbackErrors := make([]string, 0, 2)
	if workspacePublished {
		if err := os.RemoveAll(managedPath); err != nil {
			rollbackErrors = append(rollbackErrors, "workspace cleanup failed: "+err.Error())
		}
	}
	if volumeCreated {
		if output, err := p.docker(rollbackCtx, "volume", "rm", payload.DataVolume); err != nil {
			rollbackErrors = append(rollbackErrors, "data volume cleanup failed: "+safeCommandError(err, output))
		}
	}
	if len(rollbackErrors) != 0 {
		return failed("clean-host restore failed and rollback was incomplete", errors.New(strings.Join(rollbackErrors, "; ")), domain.InstanceFailed)
	}
	return failed("clean-host restore failed; absent state was restored", mutationErr, domain.InstanceStopped)
}

func verifyRestoreArtifact(filename string, expectedSize int64, expectedDigest string) error {
	if !sha256HexPattern.MatchString(expectedDigest) {
		return errors.New("recovery point checksum is invalid")
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != expectedSize {
		return errors.New("recovery point file metadata does not match")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		file.Close()
		return errors.New("recovery point file changed during validation")
	}
	digest := sha256.New()
	written, copyErr := io.Copy(digest, io.LimitReader(file, expectedSize+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != expectedSize || hex.EncodeToString(digest.Sum(nil)) != expectedDigest {
		return errors.New("recovery point size or checksum does not match")
	}
	return nil
}

func (p *Provisioner) verifyRestoreVolumeOwnership(ctx context.Context, payload domain.RecoveryRestorePayload) error {
	output, err := p.docker(ctx, "volume", "inspect", "--format", "{{json .Labels}}", payload.DataVolume)
	if err != nil {
		return errors.New(safeCommandError(err, output))
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &labels); err != nil {
		return errors.New("Docker returned invalid data volume ownership")
	}
	instanceOwned := labels["io.hermes-fleet.managed"] == "true" && labels["io.hermes-fleet.instance-id"] == payload.InstanceID
	legacyComposeOwned := labels["io.hermes-fleet.instance-id"] == "" &&
		labels["com.docker.compose.project"] == payload.ProjectName && labels["com.docker.compose.volume"] == "hermes-data"
	if !instanceOwned && !legacyComposeOwned {
		return errors.New("Fleet data volume ownership does not match the restore job")
	}
	return nil
}

func (p *Provisioner) restoreVolumeExists(ctx context.Context, payload domain.RecoveryRestorePayload) (bool, error) {
	if err := p.verifyRestoreVolumeOwnership(ctx, payload); err == nil {
		return true, nil
	} else {
		output, listErr := p.docker(ctx, "volume", "ls", "--quiet", "--filter", "name=^"+payload.DataVolume+"$")
		if listErr != nil {
			return false, fmt.Errorf("inspect Docker volume list: %s", safeCommandError(listErr, output))
		}
		for _, name := range strings.Fields(output) {
			if name == payload.DataVolume {
				return false, err
			}
		}
		return false, nil
	}
}

func extractRestoreArchive(ctx context.Context, artifactPath, stagingRoot string, payload domain.RecoveryRestorePayload) (string, string, error) {
	artifact, err := os.Open(artifactPath)
	if err != nil {
		return "", "", err
	}
	defer artifact.Close()
	workspaceRoot := filepath.Join(stagingRoot, "workspace")
	volumePath := filepath.Join(stagingRoot, "data-volume.tar")
	archive := tar.NewReader(io.LimitReader(artifact, payload.MaxBytes+1))
	seen := make(map[string]bool)
	manifestFound, volumeFound, workspaceFound := false, false, false
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "", err
		}
		entries++
		if entries > 100000 || header.Size < 0 || header.Size > payload.MaxBytes || !safeArchivePath(header.Name) || seen[header.Name] {
			return "", "", errors.New("recovery archive contains an unsafe or duplicate entry")
		}
		seen[header.Name] = true
		switch {
		case header.Name == "manifest.json":
			if header.Typeflag != tar.TypeReg || header.Size > 1<<20 {
				return "", "", errors.New("recovery manifest is invalid")
			}
			data, err := io.ReadAll(io.LimitReader(archive, (1<<20)+1))
			if err != nil || int64(len(data)) != header.Size {
				return "", "", errors.New("recovery manifest could not be read")
			}
			var manifest recovery.Manifest
			if json.Unmarshal(data, &manifest) != nil || !restoreManifestMatches(manifest, payload) {
				return "", "", errors.New("recovery manifest does not match the restore job")
			}
			manifestFound = true
		case header.Name == "data-volume.tar":
			if header.Typeflag != tar.TypeReg || volumeFound {
				return "", "", errors.New("data volume archive is invalid")
			}
			file, err := os.OpenFile(volumePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return "", "", err
			}
			written, copyErr := io.CopyN(file, archive, header.Size)
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil || written != header.Size || syncErr != nil || closeErr != nil {
				return "", "", errors.New("data volume archive could not be staged")
			}
			volumeFound = true
		case header.Name == "workspace":
			if header.Typeflag != tar.TypeDir || workspaceFound {
				return "", "", errors.New("workspace root is invalid")
			}
			if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
				return "", "", err
			}
			workspaceFound = true
		case strings.HasPrefix(header.Name, "workspace/"):
			if !workspaceFound {
				return "", "", errors.New("workspace entry appears before its root")
			}
			relative := strings.TrimPrefix(header.Name, "workspace/")
			target := filepath.Join(workspaceRoot, filepath.FromSlash(relative))
			if filepath.Clean(target) == workspaceRoot || !strings.HasPrefix(filepath.Clean(target), workspaceRoot+string(os.PathSeparator)) {
				return "", "", errors.New("workspace entry escapes the staging root")
			}
			switch header.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(target, 0o700); err != nil {
					return "", "", err
				}
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					return "", "", err
				}
				mode := os.FileMode(header.Mode) & 0o700
				if mode&0o400 == 0 {
					mode |= 0o600
				}
				file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
				if err != nil {
					return "", "", err
				}
				written, copyErr := io.CopyN(file, archive, header.Size)
				closeErr := file.Close()
				if copyErr != nil || written != header.Size || closeErr != nil {
					return "", "", errors.New("workspace file could not be staged")
				}
			default:
				return "", "", errors.New("workspace archive contains an unsupported file type")
			}
		default:
			return "", "", errors.New("recovery archive contains an unexpected entry")
		}
	}
	if !manifestFound || !volumeFound || !workspaceFound {
		return "", "", errors.New("recovery archive is incomplete")
	}
	if err := validateVolumeArchive(volumePath, payload.MaxBytes); err != nil {
		return "", "", err
	}
	return workspaceRoot, volumePath, nil
}

func restoreManifestMatches(manifest recovery.Manifest, payload domain.RecoveryRestorePayload) bool {
	return manifest.FormatVersion == 1 && manifest.RecoveryPointID == payload.RecoveryPointID &&
		manifest.InstanceID == payload.InstanceID && manifest.InstanceName == payload.Name && manifest.Image == payload.Image &&
		manifest.ImageID == payload.ImageID && manifest.Provider == payload.Provider && manifest.Model == payload.Model &&
		manifest.Reasoning == payload.Reasoning && manifest.ServiceTier == payload.ServiceTier &&
		manifest.CodexConfigured == payload.CodexConfigured && manifest.ProjectName == payload.ProjectName &&
		manifest.DataVolume == payload.DataVolume && manifest.ManagedPath == payload.ManagedPath && manifest.AgentVersion == payload.AgentVersion &&
		manifest.CreatedAt.Equal(payload.CreatedAt)
}

func validateVolumeArchive(filename string, maxBytes int64) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	archive := tar.NewReader(io.LimitReader(file, maxBytes+1))
	entries := 0
	seen := make(map[string]bool)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read data volume archive: %w", err)
		}
		entries++
		archiveName := header.Name
		if header.Typeflag == tar.TypeDir {
			// GNU tar emits directory entries with a trailing slash. Normalize
			// that representation only for directories so archives created by
			// the recovery-point writer pass the same path-safety checks used
			// during restore. A trailing slash on any other entry remains unsafe.
			archiveName = strings.TrimSuffix(archiveName, "/")
		}
		normalizedName, safe := canonicalArchivePath(archiveName)
		if entries > 100000 || header.Size < 0 || header.Size > maxBytes || !safe || seen[normalizedName] {
			return errors.New("data volume archive contains an unsafe entry")
		}
		seen[normalizedName] = true
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink:
			if header.Linkname == "" || path.IsAbs(header.Linkname) || strings.Contains(header.Linkname, "\\") {
				return errors.New("data volume archive contains an unsafe link")
			}
			resolved := path.Clean(path.Join(path.Dir(normalizedName), header.Linkname))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return errors.New("data volume archive link escapes its root")
			}
		case tar.TypeLink:
			if _, safe := canonicalArchivePath(header.Linkname); !safe {
				return errors.New("data volume archive contains an unsafe hard link")
			}
		default:
			return errors.New("data volume archive contains an unsupported file type")
		}
	}
}

func safeArchivePath(name string) bool {
	_, safe := canonicalArchivePath(name)
	return safe
}

func canonicalArchivePath(name string) (string, bool) {
	if name == "." || name == "./" {
		return ".", true
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	safe := name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." &&
		!strings.HasPrefix(name, "../") && !strings.Contains(name, "\\")
	return name, safe
}

func (p *Provisioner) exportVolume(ctx context.Context, volume, imageID string, maxBytes int64) (string, error) {
	file, err := os.CreateTemp(p.root, ".restore-volume-backup-*.tar")
	if err != nil {
		return "", err
	}
	filename := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(filename)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	reader, wait := p.dockerOutput(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "tar",
		"-v", volume+":/source:ro", imageID, "-C", "/source", "-cf", "-", ".",
	)
	written, copyErr := io.Copy(file, io.LimitReader(reader, maxBytes+1))
	_ = reader.Close()
	commandErr := wait()
	if copyErr != nil || commandErr != nil || written < 1 || written > maxBytes {
		return "", errors.New("data volume rollback export failed or exceeded the size limit")
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := validateVolumeArchive(filename, maxBytes); err != nil {
		return "", err
	}
	keep = true
	return filename, nil
}

const recoveryDiskSafetyMargin = uint64(128 << 20)

func (p *Provisioner) estimateVolumeArchiveSize(ctx context.Context, volume, imageID string, maxBytes int64) (int64, error) {
	if p.volumeSize != nil {
		return p.volumeSize(ctx, volume, imageID, maxBytes)
	}
	reader, wait := p.dockerOutput(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "tar",
		"-v", volume+":/source:ro", imageID, "-C", "/source", "-cf", "-", ".",
	)
	written, copyErr := io.Copy(io.Discard, io.LimitReader(reader, maxBytes+1))
	_ = reader.Close()
	commandErr := wait()
	if copyErr != nil {
		return 0, copyErr
	}
	if commandErr != nil {
		return 0, commandErr
	}
	if written < 1 || written > maxBytes {
		return 0, errors.New("data volume estimate is empty or exceeds the recovery size limit")
	}
	return written, nil
}

func estimateWorkspaceArchiveSize(ctx context.Context, root string, maxBytes int64) (int64, error) {
	if maxBytes < 2048 {
		return 0, errors.New("recovery point size limit is too small for a workspace archive")
	}
	var total int64
	entries := 0
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filename == root {
			return nil
		}
		entries++
		if entries > 100000 {
			return errors.New("workspace contains too many entries for one recovery point")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return fmt.Errorf("unsupported workspace file type: %s", filename)
		}
		entrySize := int64(512)
		if info.Mode().IsRegular() {
			if info.Size() > maxBytes {
				return fmt.Errorf("workspace file exceeds the recovery point size limit: %s", filename)
			}
			entrySize += ((info.Size() + 511) / 512) * 512
		}
		if entrySize > maxBytes-total {
			return errors.New("workspace estimate exceeds the recovery point size limit")
		}
		total += entrySize
		return nil
	})
	if err != nil {
		return 0, err
	}
	if total > maxBytes-2048 {
		return 0, errors.New("workspace estimate exceeds the recovery point size limit")
	}
	return total + 2048, nil
}

func recoveryCreationDiskRequirement(volumeBytes, workspaceBytes int64) (uint64, error) {
	if volumeBytes < 1 || workspaceBytes < 0 {
		return 0, errors.New("invalid recovery storage estimate")
	}
	volume := uint64(volumeBytes)
	workspace := uint64(workspaceBytes)
	if volume > (^uint64(0)-workspace-recoveryDiskSafetyMargin)/2 {
		return 0, errors.New("recovery storage estimate overflow")
	}
	return 2*volume + workspace + recoveryDiskSafetyMargin, nil
}

func restoreDiskRequirement(recoveryBytes, currentVolumeBytes int64) (uint64, error) {
	if recoveryBytes < 1 || currentVolumeBytes < 1 {
		return 0, errors.New("invalid restore storage estimate")
	}
	recoverySize := uint64(recoveryBytes)
	volumeSize := uint64(currentVolumeBytes)
	if recoverySize > ^uint64(0)-volumeSize-recoveryDiskSafetyMargin {
		return 0, errors.New("restore storage estimate overflow")
	}
	return recoverySize + volumeSize + recoveryDiskSafetyMargin, nil
}

func (p *Provisioner) ensureDiskAvailable(required uint64) error {
	var available uint64
	var err error
	if p.diskAvailable != nil {
		available, err = p.diskAvailable(p.root)
	} else {
		available, err = availableDiskBytes(p.root)
	}
	if err != nil {
		return fmt.Errorf("inspect available disk space: %w", err)
	}
	if available < required {
		return fmt.Errorf("insufficient disk space: %d bytes required, %d bytes available", required, available)
	}
	return nil
}

func availableDiskBytes(path string) (uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	blockSize := uint64(stats.Bsize)
	availableBlocks := uint64(stats.Bavail)
	if blockSize != 0 && availableBlocks > ^uint64(0)/blockSize {
		return ^uint64(0), nil
	}
	return availableBlocks * blockSize, nil
}

func (p *Provisioner) replaceVolume(ctx context.Context, volume, imageID, archivePath string) error {
	if err := p.clearVolume(ctx, volume, imageID); err != nil {
		return err
	}
	return p.importVolume(ctx, volume, imageID, archivePath)
}

func (p *Provisioner) clearVolume(ctx context.Context, volume, imageID string) error {
	clearOutput, err := p.docker(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "sh",
		"-v", volume+":/target", imageID, "-c", "find /target -mindepth 1 -delete",
	)
	if err != nil {
		return fmt.Errorf("clear target data volume: %s", safeCommandError(err, clearOutput))
	}
	return nil
}

func (p *Provisioner) importVolume(ctx context.Context, volume, imageID, archivePath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	extractOutput, err := p.dockerInput(ctx, archive,
		"run", "--rm", "-i", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "tar",
		"-v", volume+":/target", imageID, "-C", "/target", "-xf", "-",
	)
	if err != nil {
		return fmt.Errorf("extract target data volume: %s", safeCommandError(err, extractOutput))
	}
	return nil
}

func (p *Provisioner) writeRecoveryArchive(ctx context.Context, destination io.Writer, payload domain.RecoveryPointPayload, managedPath, volumePath string, volumeSize int64, key []byte) error {
	archive := tar.NewWriter(destination)
	manifest := recovery.Manifest{
		FormatVersion: 1, RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID,
		InstanceName: payload.Name, Image: payload.Image, ImageID: payload.ImageID, Provider: payload.Provider,
		Model: payload.Model, Reasoning: payload.Reasoning, ServiceTier: payload.ServiceTier,
		CodexConfigured: payload.CodexConfigured,
		ProjectName:     payload.ProjectName, DataVolume: payload.DataVolume, ManagedPath: payload.ManagedPath,
		AgentVersion: payload.AgentVersion, CreatedAt: payload.CreatedAt,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')
	if err := archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifestData)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(manifestData); err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: "workspace", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	entries := 0
	if err := filepath.WalkDir(managedPath, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(managedPath, filename)
		if err != nil || relative == "." {
			return err
		}
		entries++
		if entries > 100000 {
			return errors.New("workspace contains too many entries for one recovery point")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return fmt.Errorf("unsupported workspace file type: %s", relative)
		}
		if info.Mode().IsRegular() && info.Size() > payload.MaxBytes {
			return fmt.Errorf("workspace file exceeds the recovery point size limit: %s", relative)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(filepath.Join("workspace", relative))
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
			file.Close()
			return errors.New("workspace file changed during recovery point creation")
		}
		_, copyErr := io.Copy(archive, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: "data-volume.tar", Mode: 0o600, Size: volumeSize, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	volume, err := os.Open(volumePath)
	if err != nil {
		return err
	}
	decryptedSize, decryptErr := recoverycodec.Decrypt(ctx, archive, volume, key, payload.RecoveryPointID+":volume")
	closeErr := volume.Close()
	if decryptErr != nil {
		return decryptErr
	}
	if closeErr != nil {
		return closeErr
	}
	if decryptedSize != volumeSize {
		return errors.New("data volume archive size changed during assembly")
	}
	return archive.Close()
}

func (p *Provisioner) dockerOutput(ctx context.Context, args ...string) (*io.PipeReader, func() error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var err error
		if p.dockerRun != nil {
			var output string
			output, err = p.dockerRun(ctx, args...)
			if err == nil {
				_, err = io.WriteString(writer, output)
			}
		} else if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
		} else {
			command := exec.CommandContext(ctx, p.dockerPath, args...)
			var stderr bytes.Buffer
			command.Stdout = writer
			command.Stderr = &limitedWriter{destination: &stderr, remaining: 4096}
			if runErr := command.Run(); runErr != nil {
				err = fmt.Errorf("%s", safeCommandError(runErr, stderr.String()))
			}
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	return reader, func() error { return <-done }
}

type limitedWriter struct {
	destination io.Writer
	remaining   int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	original := len(data)
	if writer.remaining > 0 {
		write := len(data)
		if write > writer.remaining {
			write = writer.remaining
		}
		_, _ = writer.destination.Write(data[:write])
		writer.remaining -= write
	}
	return original, nil
}

// Observe inspects only the exact Fleet-owned resources named by the control
// plane. It never creates files or invokes a mutating Docker command.
func (p *Provisioner) Observe(ctx context.Context, target domain.ObservationTarget) domain.InstanceObservation {
	managedPath, err := p.validateObservationTarget(target)
	if err != nil {
		return unknownObservation(target, "Observation target metadata is invalid", nil)
	}
	builder := &observationBuilder{}
	p.observeManagedFiles(managedPath, builder)

	if _, err := p.docker(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		checks := append(builder.checks, domain.ObservationCheck{Name: "docker_daemon", Status: domain.ObservationCheckUnknown, Detail: "Docker daemon could not be queried"})
		return unknownObservation(target, "Docker daemon is unavailable", checks)
	}
	builder.add("docker_daemon", domain.ObservationCheckOK, "Docker daemon responded")

	if _, err := p.docker(ctx, "volume", "inspect", target.DataVolume); err != nil {
		builder.add("data_volume", domain.ObservationCheckMissing, "Expected Fleet data volume was not found")
	} else {
		builder.add("data_volume", domain.ObservationCheckOK, "Expected Fleet data volume exists")
	}

	containerOutput, err := p.docker(
		ctx, "ps", "-a",
		"--filter", "label=io.hermes-fleet.managed=true",
		"--filter", "label=io.hermes-fleet.instance-id="+target.InstanceID,
		"--format", "{{.ID}}",
	)
	if err != nil {
		checks := append(builder.checks, domain.ObservationCheck{Name: "containers", Status: domain.ObservationCheckUnknown, Detail: "Fleet container list could not be queried"})
		return unknownObservation(target, "Container state could not be observed", checks)
	}
	containerIDs := nonEmptyLines(containerOutput)
	for _, containerID := range containerIDs {
		if !containerIDPattern.MatchString(containerID) {
			checks := append(builder.checks, domain.ObservationCheck{Name: "containers", Status: domain.ObservationCheckUnknown, Detail: "Docker returned an invalid container identity"})
			return unknownObservation(target, "Container identity could not be validated", checks)
		}
	}
	if len(containerIDs) == 0 {
		builder.add("containers", domain.ObservationCheckMissing, "No containers exist for the exact Fleet instance label")
		if target.DesiredStatus == domain.InstanceRunning {
			builder.add("runtime", domain.ObservationCheckDrift, "Expected Hermes and dashboard containers are missing")
		} else if target.DesiredStatus == domain.InstanceStopped {
			builder.add("runtime", domain.ObservationCheckOK, "No runtime containers are running")
		}
		return builder.finish(target)
	}

	inspectArgs := append([]string{"inspect"}, containerIDs...)
	inspectOutput, err := p.docker(ctx, inspectArgs...)
	if err != nil {
		checks := append(builder.checks, domain.ObservationCheck{Name: "containers", Status: domain.ObservationCheckUnknown, Detail: "Labeled containers disappeared during inspection"})
		return unknownObservation(target, "Container inspection did not complete", checks)
	}
	var containers []observedContainer
	if err := json.Unmarshal([]byte(inspectOutput), &containers); err != nil {
		checks := append(builder.checks, domain.ObservationCheck{Name: "containers", Status: domain.ObservationCheckUnknown, Detail: "Docker returned invalid inspection data"})
		return unknownObservation(target, "Container inspection data is invalid", checks)
	}
	if len(containers) < 2 {
		builder.add("containers", domain.ObservationCheckMissing, "Hermes or dashboard container is missing")
	} else if len(containers) > 2 {
		builder.add("containers", domain.ObservationCheckDrift, "Unexpected extra containers use this Fleet instance label")
	} else {
		builder.add("containers", domain.ObservationCheckOK, "Hermes and dashboard containers exist")
	}

	services := make(map[string]observedContainer, len(containers))
	ownershipOK := true
	imagesOK := true
	for _, container := range containers {
		labels := container.Config.Labels
		service := labels["com.docker.compose.service"]
		if labels["io.hermes-fleet.managed"] != "true" ||
			labels["io.hermes-fleet.instance-id"] != target.InstanceID ||
			labels["com.docker.compose.project"] != target.ProjectName ||
			(service != "hermes" && service != "dashboard") {
			ownershipOK = false
			continue
		}
		if _, duplicate := services[service]; duplicate {
			ownershipOK = false
		}
		services[service] = container
		if target.ImageID != "" && container.Image != target.ImageID {
			imagesOK = false
		}
	}
	if ownershipOK && len(services) == 2 {
		builder.add("ownership", domain.ObservationCheckOK, "Container labels match the exact Fleet project and instance")
		if version := services["hermes"].Config.Labels["io.hermes-fleet.hermes-version"]; hermesVersionRef.MatchString(version) {
			builder.hermesVersion = version
		}
		if source := services["hermes"].Config.Labels["io.hermes-fleet.hermes-ref"]; hermesSourceRef.MatchString(source) {
			builder.hermesSource = source
		}
		if builder.hermesVersion == "" && services["hermes"].State.Status == "running" {
			p.observeInstalledHermesVersion(ctx, services["hermes"], builder)
		}
	} else {
		builder.add("ownership", domain.ObservationCheckDrift, "Container project, service, or Fleet ownership labels do not match")
	}
	if target.ImageID == "" {
		builder.add("image", domain.ObservationCheckUnknown, "No expected image ID was recorded")
	} else if imagesOK && len(containers) == 2 {
		builder.add("image", domain.ObservationCheckOK, "Container image IDs match the provisioned image")
	} else {
		builder.add("image", domain.ObservationCheckDrift, "One or more container image IDs differ from the provisioned image")
	}

	runtimeOK := p.observeRuntimeState(target.DesiredStatus, services, builder)
	if target.DesiredStatus == domain.InstanceRunning && runtimeOK {
		if err := p.checkHealth(ctx, target.APIPort); err != nil {
			builder.add("health_endpoint", domain.ObservationCheckDrift, "Hermes /health did not return a successful response")
		} else {
			builder.add("health_endpoint", domain.ObservationCheckOK, "Hermes /health returned a successful response")
		}
		if target.Provider == "openai-codex" {
			p.observeCodexModelCatalog(ctx, services["hermes"], builder)
			p.observeCodexAuth(ctx, services["hermes"], builder)
		}
		if ownershipOK && target.Provider == "openai-codex" {
			switch {
			case target.CodexConfigured && target.Model != "":
				p.observeRuntimeConfiguration(ctx, target, services["hermes"], builder)
			case !target.CodexConfigured:
				builder.add("runtime_configuration", domain.ObservationCheckDrift, "Codex configuration has not been saved in Hermes Fleet")
			default:
				builder.add("runtime_configuration", domain.ObservationCheckDrift, "Saved Codex configuration is incomplete")
			}
		} else if ownershipOK && target.Model != "" {
			p.observeRuntimeConfiguration(ctx, target, services["hermes"], builder)
		}
	}
	return builder.finish(target)
}

func (p *Provisioner) observeCodexModelCatalog(ctx context.Context, hermes observedContainer, builder *observationBuilder) {
	if !containerIDPattern.MatchString(hermes.ID) {
		return
	}
	output, err := p.docker(ctx, "exec", hermes.ID, "/opt/hermes-agent/.venv/bin/python", "-c", codexModelCatalogProbe)
	if err != nil {
		return
	}
	var catalog struct {
		Models      []string `json:"models"`
		Recommended string   `json:"recommended"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &catalog); err != nil {
		return
	}
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		model = strings.TrimSpace(model)
		if seen[model] || providers.ValidateRuntime("openai-codex", model, "medium", "normal") != nil {
			continue
		}
		builder.modelCatalog = append(builder.modelCatalog, model)
		seen[model] = true
		if len(builder.modelCatalog) == 64 {
			break
		}
	}
	catalog.Recommended = strings.TrimSpace(catalog.Recommended)
	if seen[catalog.Recommended] {
		builder.recommendedModel = catalog.Recommended
	}
}

func (p *Provisioner) observeInstalledHermesVersion(ctx context.Context, hermes observedContainer, builder *observationBuilder) {
	if !containerIDPattern.MatchString(hermes.ID) {
		return
	}
	version, err := p.docker(
		ctx, "exec", hermes.ID, "/opt/hermes-agent/.venv/bin/python", "-c",
		`import importlib.metadata as m; print(m.version("hermes-agent"))`,
	)
	version = strings.TrimSpace(version)
	if err == nil && hermesVersionRef.MatchString(version) {
		builder.hermesVersion = version
	}
}

func (p *Provisioner) observeRuntimeConfiguration(ctx context.Context, target domain.ObservationTarget, hermes observedContainer, builder *observationBuilder) {
	if !containerIDPattern.MatchString(hermes.ID) {
		builder.add("runtime_configuration", domain.ObservationCheckUnknown, "Hermes runtime configuration could not be checked")
		return
	}
	output, err := p.docker(ctx, "exec", hermes.ID, "python", "-c", runtimeStateProbe)
	if err != nil {
		builder.add("runtime_configuration", domain.ObservationCheckUnknown, "Hermes runtime configuration is unavailable")
		return
	}
	var decoded runtimeStateObservation
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &decoded); err != nil {
		builder.add("runtime_configuration", domain.ObservationCheckUnknown, "Hermes returned invalid runtime configuration")
		return
	}
	if verifyRuntimeState(output, target.Provider, target.Model, "", "", nil) == nil {
		builder.add("runtime_configuration", domain.ObservationCheckOK, "Hermes configuration and Fleet readiness marker match")
		return
	}
	builder.add("runtime_configuration", domain.ObservationCheckDrift, "Hermes configuration or Fleet readiness marker is stale")
}

func (p *Provisioner) observeCodexAuth(ctx context.Context, hermes observedContainer, builder *observationBuilder) bool {
	if !containerIDPattern.MatchString(hermes.ID) {
		builder.add("codex_auth", domain.ObservationCheckUnknown, "Codex authentication could not be checked")
		return false
	}
	output, err := p.docker(ctx, "exec", hermes.ID, "hermes", "auth", "status", "openai-codex")
	if err != nil {
		builder.add("codex_auth", domain.ObservationCheckUnknown, "Codex authentication status is unavailable")
		return false
	}
	switch {
	case strings.Contains(output, "openai-codex: logged in"):
		builder.add("codex_auth", domain.ObservationCheckOK, "Codex authentication is connected")
		return true
	case strings.Contains(output, "openai-codex: logged out"):
		builder.add("codex_auth", domain.ObservationCheckDrift, "Codex authentication is required")
	default:
		builder.add("codex_auth", domain.ObservationCheckUnknown, "Codex authentication returned an unknown status")
	}
	return false
}

func (p *Provisioner) validateObservationTarget(target domain.ObservationTarget) (string, error) {
	if !instanceIDPattern.MatchString(target.InstanceID) || !safeNamePattern.MatchString(target.Name) {
		return "", errors.New("invalid Fleet instance identity")
	}
	shortID := strings.ReplaceAll(target.InstanceID, "-", "")[:8]
	expectedProject := "hermes-fleet-" + target.Name + "-" + shortID
	if target.ProjectName != expectedProject || target.DataVolume != expectedProject+"-data" {
		return "", errors.New("managed resource names do not match the Fleet identity")
	}
	reasoning, serviceTier := "", ""
	if target.Model != "" {
		reasoning, serviceTier = "medium", "normal"
	}
	if err := providers.ValidateRuntimeOrPending(target.Provider, target.Model, reasoning, serviceTier); err != nil {
		return "", errors.New("invalid desired runtime configuration")
	}
	if target.DesiredStatus != domain.InstanceRunning && target.DesiredStatus != domain.InstanceStopped && target.DesiredStatus != domain.InstanceFailed {
		return "", errors.New("instance state is not observable")
	}
	absolute, err := filepath.Abs(target.ManagedPath)
	if err != nil {
		return "", err
	}
	expectedPath := filepath.Join(p.root, target.Name+"-"+shortID)
	if absolute != expectedPath {
		return "", errors.New("managed path does not match the Fleet identity")
	}
	return absolute, nil
}

func (p *Provisioner) observeManagedFiles(managedPath string, builder *observationBuilder) {
	info, err := os.Lstat(managedPath)
	if errors.Is(err, os.ErrNotExist) {
		builder.add("managed_path", domain.ObservationCheckMissing, "Managed instance directory is missing")
		builder.add("manifest", domain.ObservationCheckMissing, "Managed Compose manifest is missing")
		builder.add("environment", domain.ObservationCheckMissing, "Managed environment file is missing")
		builder.add("workspace", domain.ObservationCheckMissing, "Managed workspace directory is missing")
		return
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		builder.add("managed_path", domain.ObservationCheckDrift, "Managed path is not a regular directory")
		return
	}
	builder.add("managed_path", domain.ObservationCheckOK, "Managed instance directory exists")
	observeRegularFile(filepath.Join(managedPath, "compose.yaml"), "manifest", "Managed Compose manifest", builder)
	observeRegularFile(filepath.Join(managedPath, ".env"), "environment", "Managed environment file", builder)
	workspace, err := os.Lstat(filepath.Join(managedPath, "workspace"))
	if errors.Is(err, os.ErrNotExist) {
		builder.add("workspace", domain.ObservationCheckMissing, "Managed workspace directory is missing")
	} else if err != nil || !workspace.IsDir() || workspace.Mode()&os.ModeSymlink != 0 {
		builder.add("workspace", domain.ObservationCheckDrift, "Managed workspace is not a regular directory")
	} else {
		builder.add("workspace", domain.ObservationCheckOK, "Managed workspace directory exists")
	}
}

func observeRegularFile(path, name, label string, builder *observationBuilder) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		builder.add(name, domain.ObservationCheckMissing, label+" is missing")
	} else if err != nil || !info.Mode().IsRegular() {
		builder.add(name, domain.ObservationCheckDrift, label+" is not a regular file")
	} else if info.Mode().Perm() != 0o600 {
		builder.add(name, domain.ObservationCheckDrift, label+" permissions differ from 0600")
	} else {
		builder.add(name, domain.ObservationCheckOK, label+" exists")
	}
}

func (p *Provisioner) observeRuntimeState(desiredStatus string, services map[string]observedContainer, builder *observationBuilder) bool {
	hermes, hasHermes := services["hermes"]
	dashboard, hasDashboard := services["dashboard"]
	if !hasHermes || !hasDashboard {
		builder.add("runtime", domain.ObservationCheckDrift, "Container state could not be matched to both expected Compose services")
		return false
	}
	switch desiredStatus {
	case domain.InstanceRunning:
		hermesHealthy := hermes.State.Health != nil && hermes.State.Health.Status == "healthy"
		dashboardHealthy := dashboard.State.Health != nil && dashboard.State.Health.Status == "healthy"
		if hermes.State.Status == "running" && dashboard.State.Status == "running" && hermesHealthy && dashboardHealthy {
			builder.add("runtime", domain.ObservationCheckOK, "Hermes and dashboard are running and healthy")
			return true
		}
		builder.add("runtime", domain.ObservationCheckDrift, "Desired RUNNING state does not match container state or health")
	case domain.InstanceStopped:
		hermesStopped := hermes.State.Status == "exited" || hermes.State.Status == "created"
		dashboardStopped := dashboard.State.Status == "exited" || dashboard.State.Status == "created"
		if hermesStopped && dashboardStopped {
			builder.add("runtime", domain.ObservationCheckOK, "Both services are stopped")
			return true
		}
		builder.add("runtime", domain.ObservationCheckDrift, "Desired STOPPED state does not match container state")
	default:
		builder.add("runtime", domain.ObservationCheckDrift, "Lifecycle state is FAILED; retained runtime artifacts require operator review")
	}
	return false
}

func (p *Provisioner) checkHealth(ctx context.Context, port int) error {
	if port < 1024 || port > 65535 {
		return errors.New("invalid health port")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return err
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health returned %d", response.StatusCode)
	}
	return nil
}

func nonEmptyLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (p *Provisioner) verifyFleetContainerImage(ctx context.Context, instanceID, projectName, imageID string, requireStopped bool) error {
	containers, err := p.inspectOwnedFleetContainers(ctx, instanceID, projectName)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.Image != imageID {
			return errors.New("container image differs from the provisioned immutable image")
		}
		if requireStopped && container.State.Status != "created" && container.State.Status != "exited" {
			return errors.New("provider binding started a stopped instance")
		}
	}
	return nil
}

func (p *Provisioner) inspectCredentials(payload domain.ActionPayload) domain.JobResult {
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	file, err := os.Open(filepath.Join(managedPath, ".env"))
	if err != nil {
		return failure("open instance credential file", err)
	}
	defer file.Close()

	allowed := map[string]*string{}
	credentials := domain.Credentials{}
	allowed["HERMES_DASHBOARD_BASIC_AUTH_USERNAME"] = &credentials.DashboardUsername
	allowed["HERMES_DASHBOARD_BASIC_AUTH_PASSWORD"] = &credentials.DashboardPassword
	allowed["API_SERVER_KEY"] = &credentials.APIServerKey

	scanner := bufio.NewScanner(io.LimitReader(file, 64<<10))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if target, ok := allowed[strings.TrimSpace(key)]; ok {
			*target = trimEnvValue(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return failure("read instance credential file", err)
	}
	if credentials.DashboardUsername == "" || credentials.DashboardPassword == "" || credentials.APIServerKey == "" {
		return domain.JobResult{Success: false, Error: "required instance credentials are missing"}
	}
	return domain.JobResult{Success: true, Message: "Credentials inspected", Credentials: &credentials}
}

func trimEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func (p *Provisioner) provision(ctx context.Context, payload domain.ProvisionPayload) domain.JobResult {
	if !safeNamePattern.MatchString(payload.Name) {
		return domain.JobResult{Success: false, Error: "unsafe instance name"}
	}
	if !instanceIDPattern.MatchString(payload.InstanceID) {
		return domain.JobResult{Success: false, Error: "unsafe instance identity"}
	}
	if err := providers.ValidateImageReference(payload.Image); err != nil {
		return failure("unsafe Hermes image", err)
	}
	if err := providers.ValidateRuntimeOrPending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier); err != nil {
		return failure("invalid runtime configuration", err)
	}
	if payload.APIPort < 1024 || payload.APIPort > 65535 || payload.DashboardPort < 1024 || payload.DashboardPort > 65535 {
		return domain.JobResult{Success: false, Error: "ports must be between 1024 and 65535"}
	}
	if payload.APIPort == payload.DashboardPort {
		return domain.JobResult{Success: false, Error: "API and dashboard ports must differ"}
	}

	projectName, dataVolume, directoryName := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	managedPath := filepath.Join(p.root, directoryName)
	failureWithMetadata := func(prefix string, err error) domain.JobResult {
		return domain.JobResult{Success: false, Error: prefix + ": " + err.Error(), ProjectName: projectName, DataVolume: dataVolume, ManagedPath: managedPath}
	}
	failureWithMetadataOutput := func(prefix string, err error, output string) domain.JobResult {
		message := strings.TrimSpace(output)
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		if message == "" {
			message = err.Error()
		}
		return domain.JobResult{Success: false, Error: prefix + ": " + message, ProjectName: projectName, DataVolume: dataVolume, ManagedPath: managedPath}
	}
	envPath := filepath.Join(managedPath, ".env")
	composePath := filepath.Join(managedPath, "compose.yaml")
	pathInfo, pathErr := os.Lstat(managedPath)
	switch {
	case errors.Is(pathErr, os.ErrNotExist):
	case pathErr != nil:
		return failureWithMetadata("inspect instance directory", pathErr)
	case !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0:
		return failureWithMetadata("inspect instance directory", errors.New("managed path must be a real directory"))
	}
	envExists := fileExists(envPath)
	composeExists := fileExists(composePath)
	if envExists != composeExists {
		return domain.JobResult{Success: false, Error: "incomplete instance files; refusing an unsafe retry", ProjectName: projectName, DataVolume: dataVolume, ManagedPath: managedPath}
	}
	retryOwnsPorts := false
	if envExists {
		if err := requireRegularFile(envPath); err != nil {
			return failureWithMetadata("unsafe managed environment on retry", err)
		}
		manifest, err := os.ReadFile(composePath)
		if err != nil {
			return failureWithMetadata("read managed manifest on retry", err)
		}
		if !bytes.Equal(manifest, []byte(renderCompose(payload, projectName, dataVolume))) {
			return failureWithMetadata("validate managed manifest on retry", errors.New("existing manifest does not match the exact Fleet instance"))
		}
		retryOwnsPorts, err = p.provisionRetryOwnsPorts(ctx, payload, projectName)
		if err != nil {
			return failureWithMetadata("validate managed runtime on retry", err)
		}
	}
	if !retryOwnsPorts {
		if err := p.portCheck(payload.APIPort); err != nil {
			return failureWithMetadata("API port is unavailable", err)
		}
		if err := p.portCheck(payload.DashboardPort); err != nil {
			return failureWithMetadata("dashboard port is unavailable", err)
		}
	}
	imageID := ""
	if payload.HermesVersion != "" || payload.HermesSource != "" {
		expectedImage := runtimeassets.ImageReference(payload.HermesVersion, payload.HermesSource)
		if expectedImage == "" || payload.Image != expectedImage {
			return failureWithMetadata("prepare Hermes image", errors.New("release identity does not match the requested runtime image"))
		}
		preparedImageID, err := p.prepareHermesImage(ctx, domain.HermesUpgradePayload{
			TargetImage: payload.Image, TargetVersion: payload.HermesVersion, TargetSource: payload.HermesSource,
		})
		if err != nil {
			return failureWithMetadata("prepare Hermes image", err)
		}
		imageID = preparedImageID
	}
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		return failureWithMetadata("create instance directory", err)
	}
	if !envExists {
		apiKey, err := randomSecret(32)
		if err != nil {
			return failureWithMetadata("generate API key", err)
		}
		dashboardPassword, err := randomSecret(24)
		if err != nil {
			return failureWithMetadata("generate dashboard password", err)
		}
		dashboardSecret, err := randomSecret(32)
		if err != nil {
			return failureWithMetadata("generate dashboard secret", err)
		}
		env := renderEnv(payload, apiKey, dashboardPassword, dashboardSecret)
		compose := renderCompose(payload, projectName, dataVolume)
		if err := writeExclusiveContext(ctx, envPath, []byte(env), 0o600); err != nil {
			return failureWithMetadata("write instance secrets", err)
		}
		if err := writeExclusiveContext(ctx, composePath, []byte(compose), 0o600); err != nil {
			return failureWithMetadata("write instance manifest", err)
		}
	}

	if imageID == "" {
		if output, err := p.docker(ctx, "image", "inspect", payload.Image); err != nil {
			return failureWithMetadataOutput("Hermes image is unavailable", err, output)
		}
		imageIDOutput, err := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.Image)
		imageID = strings.TrimSpace(imageIDOutput)
		if err != nil || !imageIDPattern.MatchString(imageID) {
			return failureWithMetadataOutput("Hermes immutable image identity is unavailable", err, imageIDOutput)
		}
	}
	if output, err := p.compose(ctx, managedPath, projectName, "up", "-d"); err != nil {
		return failureWithMetadataOutput("Docker Compose provisioning failed", err, output)
	}
	if err := p.waitForHealth(ctx, payload.APIPort, 150*time.Second); err != nil {
		return domain.JobResult{
			Success: false, Error: "Hermes healthcheck failed: " + err.Error(),
			ProjectName: projectName, DataVolume: dataVolume, ManagedPath: managedPath,
		}
	}
	var readinessErr error
	if providers.IsRuntimePending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier) {
		// A new dashboard can take longer than one minute to become responsive
		// while several Fleet-owned instances are starting on the same host. Keep
		// the provisioning operation active through that cold-start window so the
		// runtime can converge without a manual retry.
		readinessErr = p.waitForDashboard(ctx, payload.DashboardPort, provisionDashboardReadyTimeout)
	} else {
		readinessErr = p.ensureManagedRuntimeReadyWithTimeout(
			ctx, managedPath, projectName,
			payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
			imageID, payload.DashboardPort, provisionDashboardReadyTimeout,
		)
	}
	if readinessErr != nil {
		return domain.JobResult{
			Success: false, Error: "Hermes runtime readiness failed: " + readinessErr.Error(),
			ProjectName: projectName, DataVolume: dataVolume, ManagedPath: managedPath,
		}
	}
	return domain.JobResult{
		Success: true, Message: "Hermes instance is running", ProjectName: projectName,
		DataVolume: dataVolume, ManagedPath: managedPath, ImageID: imageID,
	}
}

func (p *Provisioner) lifecycle(ctx context.Context, jobType string, payload domain.ActionPayload) domain.JobResult {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) {
		return domain.JobResult{Success: false, Error: "invalid managed instance identity"}
	}
	expectedProject, _, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	expectedPath := filepath.Join(p.root, expectedDirectory)
	if jobType == "instance.delete" {
		metadataEmpty := payload.ProjectName == "" && payload.ManagedPath == ""
		metadataExact := payload.ProjectName == expectedProject && filepath.Clean(payload.ManagedPath) == expectedPath
		if !metadataEmpty && !metadataExact {
			return domain.JobResult{Success: false, Error: "managed runtime identity does not match the lifecycle job"}
		}
		pathInfo, pathErr := os.Lstat(expectedPath)
		if errors.Is(pathErr, os.ErrNotExist) {
			return domain.JobResult{Success: true, Message: "No managed runtime exists; lifecycle record retired"}
		}
		if pathErr != nil {
			return failure("managed runtime cannot be safely retired", pathErr)
		}
		if !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 {
			return domain.JobResult{Success: false, Error: "managed runtime cannot be safely retired: managed path is not a real directory"}
		}
		if _, manifestErr := os.Lstat(filepath.Join(expectedPath, "compose.yaml")); errors.Is(manifestErr, os.ErrNotExist) {
			return domain.JobResult{Success: true, Message: "Incomplete provisioning record retired; retained artifacts were not modified"}
		} else if manifestErr != nil {
			return failure("managed runtime cannot be safely retired", manifestErr)
		}
		if metadataEmpty {
			return domain.JobResult{Success: false, Error: "managed runtime metadata is missing while active artifacts still exist"}
		}
	}
	if payload.ProjectName != expectedProject || filepath.Clean(payload.ManagedPath) != expectedPath {
		return domain.JobResult{Success: false, Error: "managed runtime identity does not match the lifecycle job"}
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	var output string
	switch jobType {
	case "instance.start":
		if failed := p.prepareRuntimeLifecycle(ctx, managedPath, payload); failed != nil {
			return *failed
		}
		output, err = p.compose(ctx, managedPath, payload.ProjectName, "up", "-d", "--remove-orphans")
		if err == nil {
			err = p.verifyRuntimeLifecycle(ctx, managedPath, payload)
		}
	case "instance.stop":
		output, err = p.compose(ctx, managedPath, payload.ProjectName, "stop")
	case "instance.delete":
		// Data is preserved by default; removing volumes requires a separate future operation.
		output, err = p.compose(ctx, managedPath, payload.ProjectName, "down", "--remove-orphans")
	}
	if err != nil {
		return failureWithOutput("lifecycle operation failed", err, output)
	}
	return domain.JobResult{Success: true, Message: "Lifecycle operation completed"}
}

func (p *Provisioner) repairRuntime(ctx context.Context, payload domain.RuntimeRepairPayload) domain.JobResult {
	action := payload.ActionPayload
	if !instanceIDPattern.MatchString(action.InstanceID) || !safeNamePattern.MatchString(action.Name) {
		return domain.JobResult{Success: false, Error: "invalid managed instance identity"}
	}
	expectedProject, _, expectedDirectory := domain.ManagedIdentity(action.InstanceID, action.Name)
	expectedPath := filepath.Join(p.root, expectedDirectory)
	if action.ProjectName != expectedProject || filepath.Clean(action.ManagedPath) != expectedPath {
		return domain.JobResult{Success: false, Error: "managed runtime identity does not match the repair job"}
	}
	if payload.Phase < 1 || payload.Phase > 3 || payload.Attempt < 1 || payload.Attempt > 3 {
		return domain.JobResult{Success: false, Error: "invalid runtime repair phase or attempt"}
	}
	if payload.Trigger != "automatic" && payload.Trigger != "manual" {
		return domain.JobResult{Success: false, Error: "invalid runtime repair trigger"}
	}
	if !action.PreserveData {
		return domain.JobResult{Success: false, Error: "runtime repair must preserve instance data"}
	}
	managedPath, err := p.safeManagedPath(action.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	if failed := p.prepareRuntimeLifecycle(ctx, managedPath, action); failed != nil {
		return *failed
	}

	var output string
	switch payload.Phase {
	case 1:
		upOutput, upErr := p.compose(ctx, managedPath, action.ProjectName, "up", "-d", "--remove-orphans")
		output = upOutput
		if upErr != nil {
			return failureWithOutput("runtime repair could not recreate missing services", upErr, output)
		}
		restartOutput, restartErr := p.compose(ctx, managedPath, action.ProjectName, "restart", "hermes", "dashboard")
		output = strings.TrimSpace(strings.Join([]string{upOutput, restartOutput}, "\n"))
		if restartErr != nil {
			return failureWithOutput("runtime repair could not restart services", restartErr, output)
		}
	case 2:
		output, err = p.compose(ctx, managedPath, action.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
		if err != nil {
			return failureWithOutput("runtime repair could not recreate services", err, output)
		}
	case 3:
		downOutput, downErr := p.compose(ctx, managedPath, action.ProjectName, "down", "--remove-orphans")
		output = downOutput
		if downErr != nil {
			return failureWithOutput("runtime repair could not stop the managed services", downErr, output)
		}
		upOutput, upErr := p.compose(ctx, managedPath, action.ProjectName, "up", "-d", "--remove-orphans", "--force-recreate")
		output = strings.TrimSpace(strings.Join([]string{downOutput, upOutput}, "\n"))
		if upErr != nil {
			return failureWithOutput("runtime repair could not rebuild the managed services", upErr, output)
		}
	}
	if err := p.verifyRuntimeLifecycle(ctx, managedPath, action); err != nil {
		return failureWithOutput("runtime repair verification failed", err, output)
	}
	return domain.JobResult{Success: true, Message: fmt.Sprintf("Runtime repair phase %d completed and verified", payload.Phase)}
}

func (p *Provisioner) prepareRuntimeLifecycle(ctx context.Context, managedPath string, payload domain.ActionPayload) *domain.JobResult {
	if err := providers.ValidateRuntimeOrPending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier); err != nil {
		result := failure("invalid runtime configuration", err)
		return &result
	}
	if err := providers.ValidateImageReference(payload.Image); err != nil {
		result := failure("invalid Hermes image reference", err)
		return &result
	}
	if !imageIDPattern.MatchString(payload.ImageID) {
		return &domain.JobResult{Success: false, Error: "invalid provisioned image identity"}
	}
	if payload.APIPort < 1024 || payload.APIPort > 65535 {
		return &domain.JobResult{Success: false, Error: "invalid API port"}
	}
	if payload.DashboardPort < 1024 || payload.DashboardPort > 65535 {
		return &domain.JobResult{Success: false, Error: "invalid dashboard port"}
	}
	imageIDOutput, inspectErr := p.docker(ctx, "image", "inspect", "--format", "{{.Id}}", payload.Image)
	currentImageID := strings.TrimSpace(imageIDOutput)
	if inspectErr != nil {
		result := failureWithOutput("Hermes immutable image identity is unavailable", inspectErr, imageIDOutput)
		return &result
	}
	if !imageIDPattern.MatchString(currentImageID) {
		return &domain.JobResult{Success: false, Error: "Hermes immutable image identity is invalid"}
	}
	if currentImageID != payload.ImageID {
		return &domain.JobResult{Success: false, Error: "Hermes image reference changed; reconcile the image before starting"}
	}
	dataVolume := payload.ProjectName + "-data"
	volumeLabelsOutput, volumeErr := p.docker(ctx, "volume", "inspect", "--format", "{{json .Labels}}", dataVolume)
	if volumeErr != nil {
		result := failureWithOutput("the expected Fleet data volume is unavailable", volumeErr, volumeLabelsOutput)
		return &result
	}
	volumeLabels := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(volumeLabelsOutput)), &volumeLabels); err != nil {
		result := failure("Docker returned invalid data volume ownership", err)
		return &result
	}
	instanceOwned := volumeLabels["io.hermes-fleet.instance-id"] == payload.InstanceID
	legacyComposeOwned := volumeLabels["io.hermes-fleet.instance-id"] == "" &&
		volumeLabels["com.docker.compose.project"] == payload.ProjectName &&
		volumeLabels["com.docker.compose.volume"] == "hermes-data"
	if !instanceOwned && !legacyComposeOwned {
		return &domain.JobResult{Success: false, Error: "Fleet data volume ownership does not match the instance"}
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(managedPath, filename)); err != nil {
			result := failure("managed runtime file is unsafe", err)
			return &result
		}
	}
	manifest := renderCompose(domain.ProvisionPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: payload.Image,
		Provider: payload.Provider, Model: payload.Model, Reasoning: payload.Reasoning,
		ServiceTier: payload.ServiceTier, APIPort: payload.APIPort, DashboardPort: payload.DashboardPort,
	}, payload.ProjectName, dataVolume)
	if err := writeAtomicReplaceContext(ctx, filepath.Join(managedPath, "compose.yaml"), []byte(manifest), 0o600); err != nil {
		result := failure("could not update the managed Compose manifest", err)
		return &result
	}
	return nil
}

func (p *Provisioner) verifyRuntimeLifecycle(ctx context.Context, managedPath string, payload domain.ActionPayload) error {
	if err := p.waitForHealth(ctx, payload.APIPort, 120*time.Second); err != nil {
		return err
	}
	if providers.IsRuntimePending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier) {
		return p.waitForDashboard(ctx, payload.DashboardPort, 60*time.Second)
	}
	return p.ensureManagedRuntimeReady(
		ctx, managedPath, payload.ProjectName,
		payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
		payload.ImageID, payload.DashboardPort,
	)
}

func (p *Provisioner) restoreManagedPath(candidate string) (string, bool, error) {
	if candidate == "" {
		return "", false, errors.New("managed path is empty")
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", false, err
	}
	relative, err := filepath.Rel(p.root, absolute)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || relative == ".." {
		return "", false, errors.New("path is outside the managed root")
	}
	if filepath.Dir(absolute) != p.root {
		return "", false, errors.New("managed path must be a direct child of the managed root")
	}
	if recoveryWorkspace, blocked := p.recoveryBlocks[filepath.Base(absolute)]; blocked {
		return "", false, fmt.Errorf(
			"managed instance is quarantined after an interrupted restore (%s); recover or remove that rollback workspace before restarting the Host Agent",
			recoveryWorkspace,
		)
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return absolute, false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("managed path must be absent or a real directory")
	}
	for _, filename := range []string{".env", "compose.yaml"} {
		if err := requireRegularFile(filepath.Join(absolute, filename)); err != nil {
			return "", false, fmt.Errorf("managed workspace is incomplete: %w", err)
		}
	}
	return absolute, true, nil
}

func (p *Provisioner) safeManagedPath(candidate string) (string, error) {
	if candidate == "" {
		return "", errors.New("managed path is empty")
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(p.root, absolute)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || relative == ".." {
		return "", errors.New("path is outside the managed root")
	}
	if filepath.Dir(absolute) != p.root {
		return "", errors.New("managed path must be a direct child of the managed root")
	}
	if recoveryWorkspace, blocked := p.recoveryBlocks[filepath.Base(absolute)]; blocked {
		return "", fmt.Errorf(
			"managed instance is quarantined after an interrupted restore (%s); recover or remove that rollback workspace before restarting the Host Agent",
			recoveryWorkspace,
		)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed path must be a real directory")
	}
	if _, err := os.Stat(filepath.Join(absolute, "compose.yaml")); err != nil {
		return "", errors.New("managed compose manifest is missing")
	}
	return absolute, nil
}

func (p *Provisioner) compose(ctx context.Context, directory, project string, args ...string) (string, error) {
	commandArgs := []string{"compose", "--env-file", filepath.Join(directory, ".env"), "-p", project, "-f", filepath.Join(directory, "compose.yaml")}
	commandArgs = append(commandArgs, args...)
	return p.docker(ctx, commandArgs...)
}

func (p *Provisioner) composeInput(
	ctx context.Context,
	input io.Reader,
	directory string,
	project string,
	args ...string,
) (string, error) {
	commandArgs := []string{
		"compose", "--env-file", filepath.Join(directory, ".env"),
		"-p", project, "-f", filepath.Join(directory, "compose.yaml"),
	}
	commandArgs = append(commandArgs, args...)
	return p.dockerInput(ctx, input, commandArgs...)
}

func (p *Provisioner) docker(ctx context.Context, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.dockerRun != nil {
		return p.dockerRun(ctx, args...)
	}
	command := exec.CommandContext(ctx, p.dockerPath, args...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (p *Provisioner) dockerInput(ctx context.Context, input io.Reader, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.dockerInputRun != nil {
		return p.dockerInputRun(ctx, input, args...)
	}
	if p.dockerRun != nil {
		return p.dockerRun(ctx, args...)
	}
	command := exec.CommandContext(ctx, p.dockerPath, args...)
	command.Stdin = input
	output, err := command.CombinedOutput()
	return string(output), err
}

func (p *Provisioner) waitForHealth(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		response, err := p.httpClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("timeout waiting for /health")
}

func (p *Provisioner) waitForDashboard(ctx context.Context, port int, timeout time.Duration) error {
	return p.waitForDashboardAtInterval(ctx, port, timeout, dashboardReadinessPollInterval)
}

func (p *Provisioner) waitForDashboardAtInterval(ctx context.Context, port int, timeout, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		pollInterval = dashboardReadinessPollInterval
	}
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/chat", port)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		response, err := p.httpClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if dashboardReadyStatus(response.StatusCode) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("timeout waiting for Dashboard after %s", timeout.Round(time.Second))
}

func dashboardReadyStatus(statusCode int) bool {
	return (statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices) ||
		statusCode == http.StatusUnauthorized
}

func checkPort(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return listener.Close()
}

func randomSecret(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func writeExclusiveContext(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) {
		return errors.New("instance secrets already exist; refusing to overwrite")
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		_ = os.Remove(path)
		return err
	}
	_, err = file.Write(data)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return err
}

func writeAtomicReplaceContext(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".fleet-update-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("file is not a regular file")
	}
	return nil
}

func updateEnvContent(original []byte, updates map[string]string) ([]byte, error) {
	return updateEnvContentWithKeys(original, updates, providers.ManagedEnvironmentKeys())
}

func updateEnvContentWithKeys(original []byte, updates map[string]string, managedKeys []string) ([]byte, error) {
	for _, value := range updates {
		if strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("environment value contains a line break")
		}
	}
	text := strings.TrimSuffix(string(original), "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	seen := map[string]bool{}
	for index, line := range lines {
		key, _, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found {
			continue
		}
		if value, ok := updates[key]; ok {
			lines[index] = formatEnvAssignment(key, value)
			seen[key] = true
		}
	}
	for _, key := range managedKeys {
		if !seen[key] {
			lines = append(lines, formatEnvAssignment(key, updates[key]))
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func safeCommandError(err error, output string) string {
	message := strings.TrimSpace(output)
	if message == "" {
		message = err.Error()
	}
	if len(message) > 500 {
		message = message[len(message)-500:]
	}
	return message
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func renderEnv(payload domain.ProvisionPayload, apiKey, dashboardPassword, dashboardSecret string) string {
	values := [][2]string{
		{"API_SERVER_KEY", apiKey},
		{"HERMES_DASHBOARD_BASIC_AUTH_USERNAME", "admin"},
		{"HERMES_DASHBOARD_BASIC_AUTH_PASSWORD", dashboardPassword},
		{"HERMES_DASHBOARD_BASIC_AUTH_SECRET", dashboardSecret},
		{"HERMES_INFERENCE_PROVIDER", payload.Provider},
		{"HERMES_INFERENCE_MODEL", payload.Model},
		{"HERMES_REASONING_EFFORT", payload.Reasoning},
		{"HERMES_SERVICE_TIER", payload.ServiceTier},
		{"TELEGRAM_BOT_TOKEN", ""},
		{"TELEGRAM_ALLOWED_USERS", ""},
		{"TELEGRAM_GROUP_ALLOWED_USERS", ""},
		{"TELEGRAM_GROUP_ALLOWED_CHATS", ""},
		{"TELEGRAM_PROXY", ""},
		{"WHATSAPP_ENABLED", "false"},
		{"WHATSAPP_MODE", "bot"},
		{"WHATSAPP_ALLOWED_USERS", ""},
	}
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, formatEnvAssignment(value[0], value[1]))
	}
	return strings.Join(lines, "\n") + "\n"
}

func formatEnvAssignment(key, value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`).Replace(value)
	return key + `="` + escaped + `"`
}

func renderCompose(payload domain.ProvisionPayload, projectName, dataVolume string) string {
	return fmt.Sprintf(`name: %s

x-hermes-environment: &hermes-environment
  HERMES_HOME: /data
  API_SERVER_ENABLED: "true"
  API_SERVER_HOST: 0.0.0.0
  API_SERVER_PORT: "8642"
  API_SERVER_KEY: ${API_SERVER_KEY}
  HERMES_INFERENCE_PROVIDER: ${HERMES_INFERENCE_PROVIDER}
  HERMES_INFERENCE_MODEL: ${HERMES_INFERENCE_MODEL}
  HERMES_REASONING_EFFORT: ${HERMES_REASONING_EFFORT}
  HERMES_SERVICE_TIER: ${HERMES_SERVICE_TIER}
  OPENROUTER_API_KEY: ${OPENROUTER_API_KEY:-}
  OPENROUTER_BASE_URL: ${OPENROUTER_BASE_URL:-}
  OPENAI_API_KEY: ${OPENAI_API_KEY:-}
  OPENAI_BASE_URL: ${OPENAI_BASE_URL:-}
  LM_API_KEY: ${LM_API_KEY:-}
  LM_BASE_URL: ${LM_BASE_URL:-}
  GOOGLE_API_KEY: ${GOOGLE_API_KEY:-}
  GEMINI_BASE_URL: ${GEMINI_BASE_URL:-}
  DEEPSEEK_API_KEY: ${DEEPSEEK_API_KEY:-}
  DEEPSEEK_BASE_URL: ${DEEPSEEK_BASE_URL:-}

services:
  hermes:
    image: %s
    container_name: %s
    restart: unless-stopped
    pids_limit: ${HERMES_FLEET_HERMES_PIDS_LIMIT:-512}
    mem_limit: ${HERMES_FLEET_HERMES_MEMORY_LIMIT:-4g}
    logging:
      driver: json-file
      options:
        max-size: "${HERMES_FLEET_LOG_MAX_SIZE:-25m}"
        max-file: "${HERMES_FLEET_LOG_MAX_FILES:-4}"
    labels:
      io.hermes-fleet.managed: "true"
      io.hermes-fleet.instance-id: %s
    environment:
      <<: *hermes-environment
      HERMES_FLEET_CONFIG_OWNER: "true"
      TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN:-}
      TELEGRAM_ALLOWED_USERS: ${TELEGRAM_ALLOWED_USERS:-}
      TELEGRAM_GROUP_ALLOWED_USERS: ${TELEGRAM_GROUP_ALLOWED_USERS:-}
      TELEGRAM_GROUP_ALLOWED_CHATS: ${TELEGRAM_GROUP_ALLOWED_CHATS:-}
      TELEGRAM_PROXY: ${TELEGRAM_PROXY:-}
      WHATSAPP_ENABLED: ${WHATSAPP_ENABLED:-false}
      WHATSAPP_MODE: ${WHATSAPP_MODE:-bot}
      WHATSAPP_ALLOWED_USERS: ${WHATSAPP_ALLOWED_USERS:-}
    ports:
      - "127.0.0.1:%d:8642"
    volumes:
      - hermes-data:/data
      - ./workspace:/workspace
    working_dir: /workspace
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:8642/health"]
      interval: 10s
      timeout: 5s
      retries: 12
      start_period: 20s

  dashboard:
    image: %s
    container_name: %s
    restart: unless-stopped
    pids_limit: ${HERMES_FLEET_DASHBOARD_PIDS_LIMIT:-256}
    mem_limit: ${HERMES_FLEET_DASHBOARD_MEMORY_LIMIT:-2g}
    logging:
      driver: json-file
      options:
        max-size: "${HERMES_FLEET_LOG_MAX_SIZE:-25m}"
        max-file: "${HERMES_FLEET_LOG_MAX_FILES:-4}"
    depends_on:
      hermes:
        condition: service_healthy
    command: ["hermes", "dashboard", "--host", "0.0.0.0", "--port", "9119", "--no-open", "--tui"]
    labels:
      io.hermes-fleet.managed: "true"
      io.hermes-fleet.instance-id: %s
    environment:
      <<: *hermes-environment
      HERMES_FLEET_CONFIG_OWNER: "false"
      HERMES_DASHBOARD_TUI: "1"
      HERMES_DASHBOARD_BASIC_AUTH_USERNAME: ${HERMES_DASHBOARD_BASIC_AUTH_USERNAME}
      HERMES_DASHBOARD_BASIC_AUTH_PASSWORD: ${HERMES_DASHBOARD_BASIC_AUTH_PASSWORD}
      HERMES_DASHBOARD_BASIC_AUTH_SECRET: ${HERMES_DASHBOARD_BASIC_AUTH_SECRET}
    ports:
      - "127.0.0.1:%d:9119"
    volumes:
      - hermes-data:/data
      - ./workspace:/workspace
    working_dir: /workspace
    networks:
      - default
      - fleet-edge
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS -u \"$${HERMES_DASHBOARD_BASIC_AUTH_USERNAME}:$${HERMES_DASHBOARD_BASIC_AUTH_PASSWORD}\" http://127.0.0.1:9119/chat >/dev/null"]
      interval: 10s
      timeout: 5s
      retries: 12
      start_period: 20s

networks:
  fleet-edge:
    name: hermes-fleet-edge
    external: true

volumes:
  hermes-data:
    name: %s
    labels:
      io.hermes-fleet.managed: "true"
      io.hermes-fleet.instance-id: %s
`, strconv.Quote(projectName), strconv.Quote(payload.Image), strconv.Quote("hermes-fleet-instance-"+payload.Name+"-hermes"),
		strconv.Quote(payload.InstanceID), payload.APIPort, strconv.Quote(payload.Image),
		strconv.Quote("hermes-fleet-instance-"+payload.Name+"-dashboard"), strconv.Quote(payload.InstanceID),
		payload.DashboardPort, strconv.Quote(dataVolume), strconv.Quote(payload.InstanceID))
}

func failure(prefix string, err error) domain.JobResult {
	return domain.JobResult{Success: false, Error: prefix + ": " + err.Error()}
}

func failureWithOutput(prefix string, err error, output string) domain.JobResult {
	message := strings.TrimSpace(output)
	if len(message) > 2000 {
		message = message[len(message)-2000:]
	}
	if message == "" {
		message = err.Error()
	}
	return domain.JobResult{Success: false, Error: prefix + ": " + message}
}
