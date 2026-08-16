package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/compatibility"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recoverycodec"
)

const Version = compatibility.HostAgentVersion

const leaseTokenHeader = "X-Fleet-Lease-Token"

const recoveryCompletionTimeout = 2*time.Hour + 5*time.Minute

const DefaultShutdownGracePeriod = 10 * time.Minute

const (
	defaultObservationConcurrency = 6
	maximumObservationConcurrency = 16
)

var (
	errJobLeaseLost         = errors.New("job lease lost")
	errChatArtifactRejected = errors.New("chat artifact rejected")
)

type Config struct {
	ControlPlaneURL        string        `json:"control_plane_url"`
	HostID                 string        `json:"host_id"`
	HostToken              string        `json:"host_token"`
	Name                   string        `json:"name"`
	Hostname               string        `json:"hostname"`
	ManagedRoot            string        `json:"managed_root"`
	PollInterval           time.Duration `json:"-"`
	HeartbeatInterval      time.Duration `json:"-"`
	ObservationInterval    time.Duration `json:"-"`
	LeaseRenewInterval     time.Duration `json:"-"`
	LeaseDuration          time.Duration `json:"-"`
	InitialRetryDelay      time.Duration `json:"-"`
	ShutdownGracePeriod    time.Duration `json:"-"`
	JobConcurrency         int           `json:"job_concurrency,omitempty"`
	ObservationConcurrency int           `json:"observation_concurrency,omitempty"`
}

type Enrollment struct {
	HostID    string `json:"host_id"`
	HostToken string `json:"host_token"`
}

type HTTPError struct {
	Operation  string
	StatusCode int
	Message    string
	Text       string
}

func (e *HTTPError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return fmt.Sprintf("%s failed (%d): %s", e.Operation, e.StatusCode, e.Message)
}

func IsHTTPStatus(err error, statusCode int) bool {
	var responseError *HTTPError
	return errors.As(err, &responseError) && responseError.StatusCode == statusCode
}

type Executor interface {
	// Execute must stop promptly when ctx is canceled because lease loss uses
	// cancellation to fence external side effects.
	Execute(context.Context, domain.Job) domain.JobResult
}

type ProgressExecutor interface {
	ExecuteWithProgress(context.Context, domain.Job, func(context.Context, domain.JobProgress) error) domain.JobResult
}

type ChatStreamExecutor interface {
	ExecuteChatStream(context.Context, domain.Job, func(context.Context, domain.ChatStreamEvent) error) domain.JobResult
}

type Observer interface {
	// Observe must be read-only and stop promptly when ctx is canceled.
	Observe(context.Context, domain.ObservationTarget) domain.InstanceObservation
}

type Client struct {
	config          Config
	httpClient      *http.Client
	executor        Executor
	observer        Observer
	output          io.Writer
	observationWake chan struct{}
}

func New(config Config, executor Executor) *Client {
	return NewWithOutput(config, executor, os.Stdout)
}

func NewWithOutput(config Config, executor Executor, output io.Writer) *Client {
	if config.PollInterval == 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 10 * time.Second
	}
	if config.ObservationInterval == 0 {
		config.ObservationInterval = 30 * time.Second
	}
	if config.LeaseRenewInterval == 0 {
		config.LeaseRenewInterval = time.Minute
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 5 * time.Minute
	}
	if config.InitialRetryDelay == 0 {
		config.InitialRetryDelay = time.Second
	}
	if config.ShutdownGracePeriod <= 0 {
		config.ShutdownGracePeriod = DefaultShutdownGracePeriod
	}
	if config.JobConcurrency == 0 {
		config.JobConcurrency = compatibility.DefaultJobConcurrency
	}
	if config.JobConcurrency < 1 {
		config.JobConcurrency = 1
	}
	if config.JobConcurrency > compatibility.MaximumJobConcurrency {
		config.JobConcurrency = compatibility.MaximumJobConcurrency
	}
	if config.ObservationConcurrency == 0 {
		config.ObservationConcurrency = defaultObservationConcurrency
	}
	if config.ObservationConcurrency < 1 {
		config.ObservationConcurrency = 1
	}
	if config.ObservationConcurrency > maximumObservationConcurrency {
		config.ObservationConcurrency = maximumObservationConcurrency
	}
	observer, _ := executor.(Observer)
	if output == nil {
		output = io.Discard
	}
	return &Client{
		config: config, executor: executor, observer: observer,
		httpClient:      &http.Client{Timeout: 20 * time.Second},
		output:          output,
		observationWake: make(chan struct{}, 1),
	}
}

func (c *Client) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.output, format, args...)
}

func Enroll(ctx context.Context, controlPlaneURL, token, name, hostname string) (Enrollment, error) {
	payload := map[string]string{
		"enrollment_token": token, "name": name, "hostname": hostname,
		"os": runtime.GOOS, "arch": runtime.GOARCH, "agent_version": Version,
	}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlPlaneURL, "/")+"/api/v1/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return Enrollment{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		return Enrollment{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return Enrollment{}, &HTTPError{
			Operation: "enrollment", StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message)),
		}
	}
	var enrollment Enrollment
	if err := json.NewDecoder(response.Body).Decode(&enrollment); err != nil {
		return Enrollment{}, err
	}
	if enrollment.HostID == "" || enrollment.HostToken == "" {
		return Enrollment{}, errors.New("enrollment response is incomplete")
	}
	return enrollment, nil
}

func Recover(ctx context.Context, controlPlaneURL, adminToken, name, hostname string) (Enrollment, error) {
	return recoverWithClient(
		ctx, &http.Client{Timeout: 20 * time.Second}, controlPlaneURL, adminToken, name, hostname,
	)
}

func recoverWithClient(
	ctx context.Context,
	client *http.Client,
	controlPlaneURL, adminToken, name, hostname string,
) (Enrollment, error) {
	if strings.TrimSpace(adminToken) == "" {
		return Enrollment{}, errors.New("admin token is required")
	}

	hostsRequest, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(controlPlaneURL, "/")+"/api/v1/hosts", nil,
	)
	if err != nil {
		return Enrollment{}, err
	}
	hostsRequest.Header.Set("Authorization", "Bearer "+adminToken)
	hostsResponse, err := client.Do(hostsRequest)
	if err != nil {
		return Enrollment{}, err
	}
	defer hostsResponse.Body.Close()
	if hostsResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(hostsResponse.Body, 4096))
		return Enrollment{}, &HTTPError{
			Operation: "host lookup", StatusCode: hostsResponse.StatusCode, Message: strings.TrimSpace(string(message)),
		}
	}

	var hosts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(hostsResponse.Body, 1<<20)).Decode(&hosts); err != nil {
		return Enrollment{}, fmt.Errorf("decode host lookup response: %w", err)
	}
	hostID := ""
	for _, host := range hosts {
		if host.Name != name {
			continue
		}
		if hostID != "" {
			return Enrollment{}, fmt.Errorf("host lookup returned more than one exact match for %q", name)
		}
		hostID = host.ID
	}
	if hostID == "" {
		return Enrollment{}, &HTTPError{
			Operation: "host lookup", StatusCode: http.StatusNotFound, Message: "host name is not enrolled",
		}
	}

	body, err := json.Marshal(map[string]string{
		"confirm_name":  name,
		"hostname":      hostname,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"agent_version": Version,
	})
	if err != nil {
		return Enrollment{}, err
	}
	rotateRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(controlPlaneURL, "/")+"/api/v1/hosts/"+url.PathEscape(hostID)+"/credentials/rotate",
		bytes.NewReader(body),
	)
	if err != nil {
		return Enrollment{}, err
	}
	rotateRequest.Header.Set("Authorization", "Bearer "+adminToken)
	rotateRequest.Header.Set("Content-Type", "application/json")
	rotateResponse, err := client.Do(rotateRequest)
	if err != nil {
		return Enrollment{}, err
	}
	defer rotateResponse.Body.Close()
	if rotateResponse.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(rotateResponse.Body, 4096))
		return Enrollment{}, &HTTPError{
			Operation:  "host credential recovery",
			StatusCode: rotateResponse.StatusCode,
			Message:    strings.TrimSpace(string(message)),
		}
	}

	var enrollment Enrollment
	if err := json.NewDecoder(io.LimitReader(rotateResponse.Body, 4096)).Decode(&enrollment); err != nil {
		return Enrollment{}, fmt.Errorf("decode host credential recovery response: %w", err)
	}
	if enrollment.HostID != hostID || enrollment.HostToken == "" {
		return Enrollment{}, errors.New("host credential recovery response is incomplete")
	}
	return enrollment, nil
}

func (c *Client) Run(ctx context.Context) error {
	if err := c.waitForInitialHeartbeat(ctx); err != nil {
		return err
	}
	heartbeatContext, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go c.runHeartbeatLoop(heartbeatContext)
	if c.observer != nil {
		observationContext, cancelObservations := context.WithCancel(ctx)
		defer cancelObservations()
		go c.runObservationLoop(observationContext)
	}
	var workers sync.WaitGroup
	// Process shutdown stops claims immediately. Active jobs use this drain
	// context instead; each job still derives a lease-fenced child context that
	// is canceled immediately when renewal proves the lease is no longer valid.
	executionContext, cancelExecutions := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelExecutions()
	workers.Add(c.config.JobConcurrency)
	for worker := 0; worker < c.config.JobConcurrency; worker++ {
		go func() {
			defer workers.Done()
			c.runJobLoop(ctx, executionContext)
		}()
	}
	<-ctx.Done()
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	timer := time.NewTimer(c.config.ShutdownGracePeriod)
	defer timer.Stop()
	select {
	case <-workersDone:
	case <-timer.C:
		cancelExecutions()
		<-workersDone
	}
	return ctx.Err()
}

func (c *Client) runJobLoop(claimContext, executionContext context.Context) {
	pollTicker := time.NewTicker(c.config.PollInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-claimContext.Done():
			return
		case <-pollTicker.C:
			if err := c.processNextWithExecutionContext(claimContext, executionContext); err != nil && !errors.Is(err, context.Canceled) {
				c.logf("host-agent: %v\n", err)
			}
		}
	}
}

// Probe performs one authenticated heartbeat and returns only after the
// control plane has accepted this exact Host Agent identity.
func (c *Client) Probe(ctx context.Context) error {
	return c.heartbeat(ctx)
}

func (c *Client) runObservationLoop(ctx context.Context) {
	if err := c.observeTargets(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.logf("host-agent observation: %v\n", err)
	}
	ticker := time.NewTicker(c.config.ObservationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.observationWake:
			if err := c.observeTargets(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logf("host-agent observation: %v\n", err)
			}
		case <-ticker.C:
			if err := c.observeTargets(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logf("host-agent observation: %v\n", err)
			}
		}
	}
}

func (c *Client) observeTargets(ctx context.Context) error {
	response, err := c.request(ctx, http.MethodPost, "/api/v1/agent/observations/targets", map[string]any{})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	var payload struct {
		Targets []domain.ObservationTarget `json:"targets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("decode observation targets: %w", err)
	}
	if len(payload.Targets) == 0 {
		return nil
	}
	if len(payload.Targets) > 100 {
		return errors.New("control plane returned too many observation targets")
	}
	workerCount := min(c.config.ObservationConcurrency, len(payload.Targets))
	jobs := make(chan domain.ObservationTarget)
	errorsFound := make(chan error, len(payload.Targets))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for target := range jobs {
				if err := c.observeAndReportTarget(ctx, target); err != nil {
					errorsFound <- fmt.Errorf("observe %s: %w", target.InstanceID, err)
				}
			}
		}()
	}

sendTargets:
	for _, target := range payload.Targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			break sendTargets
		}
	}
	close(jobs)
	workers.Wait()
	close(errorsFound)
	if err := ctx.Err(); err != nil {
		return err
	}
	var reportedErrors []error
	for reportedError := range errorsFound {
		reportedErrors = append(reportedErrors, reportedError)
	}
	return errors.Join(reportedErrors...)
}

func (c *Client) observeAndReportTarget(ctx context.Context, target domain.ObservationTarget) error {
	observationContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	observation := c.observer.Observe(observationContext, target)
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	observation.InstanceID = target.InstanceID
	observation.TargetGeneration = target.Generation
	observation.RefreshRequestID = target.RefreshRequestID
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}
	reported, err := c.request(ctx, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{observation},
	})
	if err != nil {
		return err
	}
	defer reported.Body.Close()
	if reported.StatusCode != http.StatusNoContent {
		return responseError(reported)
	}
	return nil
}

func (c *Client) waitForInitialHeartbeat(ctx context.Context) error {
	delay := c.config.InitialRetryDelay
	for {
		if err := c.heartbeat(ctx); err == nil {
			return nil
		} else if !errors.Is(err, context.Canceled) {
			c.logf("host-agent initial heartbeat: %v; retrying in %s\n", err, delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (c *Client) runHeartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.heartbeat(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logf("host-agent heartbeat: %v\n", err)
			}
		}
	}
}

func (c *Client) heartbeat(ctx context.Context) error {
	payload := map[string]string{"hostname": c.config.Hostname, "os": runtime.GOOS, "arch": runtime.GOARCH, "agent_version": Version}
	response, err := c.request(ctx, http.MethodPost, "/api/v1/agent/heartbeat", payload)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *Client) processNext(ctx context.Context) error {
	return c.processNextWithExecutionContext(ctx, ctx)
}

func (c *Client) processNextWithExecutionContext(claimContext, executionParent context.Context) error {
	response, err := c.request(claimContext, http.MethodPost, "/api/v1/agent/jobs/claim", map[string]any{})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	var job domain.Job
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		return err
	}
	// A decoded claim is not active work yet. Shutdown must be able to stop it
	// before acknowledgement; only an acknowledged job enters the drain context.
	if err := claimContext.Err(); err != nil {
		return err
	}
	ack, err := c.requestWithLease(claimContext, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/ack", map[string]any{}, job.LeaseToken)
	if err != nil {
		return err
	}
	if ack.StatusCode != http.StatusNoContent {
		err := responseError(ack)
		ack.Body.Close()
		return err
	}
	ack.Body.Close()
	executionContext, cancelExecution := context.WithCancelCause(executionParent)
	defer cancelExecution(context.Canceled)
	renewalDone := make(chan error, 1)
	renewalJob := domain.Job{ID: job.ID, Type: job.Type, LeaseToken: job.LeaseToken}
	go func(job domain.Job) {
		renewalErr := c.runLeaseRenewalLoop(executionContext, job)
		if renewalErr != nil && !errors.Is(renewalErr, context.Canceled) {
			cancelExecution(renewalErr)
		}
		renewalDone <- renewalErr
	}(renewalJob)
	var result domain.JobResult
	inputArtifact := ""
	if job.Type == "instance.recovery.restore" || job.Type == "instance.hermes.upgrade" {
		inputArtifact, err = c.downloadRecoveryPoint(executionContext, job)
		if err != nil {
			result = domain.JobResult{Success: false, Error: "recovery point download failed: " + err.Error(), InstanceStatus: domain.InstanceStopped}
		} else {
			job.InputArtifact = inputArtifact
			defer os.Remove(inputArtifact)
		}
	}
	if result.Error == "" && job.Type == "instance.messaging.configure" {
		job.InputSecret, err = c.downloadMessagingConfiguration(executionContext, job)
		if err != nil {
			status := ""
			var payload domain.MessagingApplyPayload
			if json.Unmarshal(job.Payload, &payload) == nil {
				status = payload.DesiredStatus
			}
			result = domain.JobResult{
				Success: false, Error: "messaging configuration download failed: " + err.Error(), InstanceStatus: status,
			}
		} else {
			defer clearSecret(job.InputSecret)
		}
	}
	if result.Error == "" && job.Type == "instance.mcp.configure" {
		job.InputSecret, err = c.downloadMCPConfiguration(executionContext, job)
		if err != nil {
			status := ""
			var payload domain.MCPApplyPayload
			if json.Unmarshal(job.Payload, &payload) == nil {
				status = payload.DesiredStatus
			}
			result = domain.JobResult{
				Success: false, Error: "MCP configuration download failed: " + err.Error(), InstanceStatus: status,
			}
		} else {
			defer clearSecret(job.InputSecret)
		}
	}
	if result.Error == "" && job.Type == "instance.chat.send" {
		job.InputSecret, err = c.downloadChatInput(executionContext, job)
		if err != nil {
			result = domain.JobResult{Success: false, Error: "chat input download failed: " + err.Error()}
		} else {
			defer clearSecret(job.InputSecret)
		}
	}
	if result.Error == "" && executionContext.Err() == nil {
		if job.Type == "instance.chat.send" {
			if executor, ok := c.executor.(ChatStreamExecutor); ok {
				result = executor.ExecuteChatStream(executionContext, job, func(eventContext context.Context, event domain.ChatStreamEvent) error {
					return c.reportChatEvent(eventContext, job, event)
				})
			} else {
				result = c.executor.Execute(executionContext, job)
			}
		} else if job.Type == "instance.hermes.update" {
			result = c.executeHermesUpdate(executionContext, job)
		} else if executor, ok := c.executor.(ProgressExecutor); ok {
			result = executor.ExecuteWithProgress(executionContext, job, func(progressContext context.Context, progress domain.JobProgress) error {
				return c.reportJobProgress(progressContext, job, progress)
			})
		} else {
			result = c.executor.Execute(executionContext, job)
		}
	} else {
		job.InputArtifact = ""
		job.InputSecret = nil
	}
	for _, upload := range result.ChatArtifacts {
		if upload.LocalPath != "" {
			defer os.Remove(upload.LocalPath)
		}
	}
	if result.Success && job.Type == "instance.chat.send" && len(result.ChatArtifacts) > 0 {
		for _, upload := range result.ChatArtifacts {
			artifact := upload.Artifact
			if upload.Error == "" && upload.LocalPath != "" {
				preparing := artifact
				preparing.Status = "preparing"
				preparing.Error = ""
				preparing.URL = ""
				preparingPayload := domain.ChatEventPayload{
					Kind: "artifact", Event: "fleet.artifact.preparing", Label: "Preparing " + preparing.Name,
					Status: preparing.Status, Artifact: &preparing,
				}
				encodedPreparing, encodeErr := json.Marshal(preparingPayload)
				if encodeErr != nil {
					return encodeErr
				}
				if err := c.reportChatEvent(executionContext, job, domain.ChatStreamEvent{
					Sequence: upload.Sequence, Type: domain.ChatEventArtifact, Content: string(encodedPreparing),
				}); err != nil {
					return err
				}
				uploadContext, cancelUpload := context.WithTimeout(executionContext, 2*time.Minute)
				ready, uploadErr := c.uploadChatArtifact(uploadContext, job, upload)
				cancelUpload()
				_ = os.Remove(upload.LocalPath)
				if errors.Is(uploadErr, errJobLeaseLost) {
					return uploadErr
				}
				if uploadErr != nil {
					artifact.Status = "failed"
					artifact.URL = ""
					artifact.Error = "Fleet could not store this output file."
					if errors.Is(uploadErr, errChatArtifactRejected) {
						artifact.Status = "rejected"
						artifact.Error = "Fleet rejected this output because it violates the artifact storage policy."
					}
				} else {
					artifact = ready
				}
			} else {
				_ = os.Remove(upload.LocalPath)
				artifact.Status = "missing"
				artifact.URL = ""
				if artifact.Error == "" {
					artifact.Error = "The output file is unavailable."
				}
			}
			payload := domain.ChatEventPayload{
				Kind: "artifact", Event: "fleet.artifact." + artifact.Status,
				Label: "Created " + artifact.Name, Status: artifact.Status, Artifact: &artifact,
			}
			if artifact.Status != "ready" {
				payload.Label = "Could not attach " + artifact.Name
			}
			encoded, encodeErr := json.Marshal(payload)
			if encodeErr != nil {
				return encodeErr
			}
			if err := c.reportChatEvent(executionContext, job, domain.ChatStreamEvent{
				Sequence: upload.Sequence + 1, Type: domain.ChatEventArtifact, Content: string(encoded),
			}); err != nil {
				return err
			}
		}
		result.ChatArtifacts = nil
	}
	artifactPath := result.RecoveryArtifact
	if artifactPath != "" {
		defer os.Remove(artifactPath)
	}
	recoveryKey := result.RecoveryKey
	if len(recoveryKey) > 0 {
		defer func() {
			for index := range recoveryKey {
				recoveryKey[index] = 0
			}
		}()
	}
	if job.Type == "instance.recovery.create" && result.Success {
		if artifactPath == "" || len(result.RecoveryKey) != 32 {
			result.Success = false
			result.Error = "recovery point executor did not return an encrypted artifact"
		} else if err := c.uploadRecoveryPoint(executionContext, job, result); err != nil {
			result.Success = false
			result.Error = "recovery point upload failed: " + err.Error()
		}
	}
	if executionContext.Err() != nil {
		cancelExecution(context.Canceled)
		renewalErr := <-renewalDone
		if errors.Is(renewalErr, errJobLeaseLost) {
			return renewalErr
		}
		if err := executionParent.Err(); err != nil {
			return err
		}
		return context.Cause(executionContext)
	}
	result.RecoveryArtifact = ""
	result.RecoveryKey = nil
	completeErr := c.completeJob(executionContext, job, result)
	cancelExecution(context.Canceled)
	renewalErr := <-renewalDone
	if errors.Is(renewalErr, errJobLeaseLost) {
		return renewalErr
	}
	if err := executionParent.Err(); err != nil {
		return err
	}
	if completeErr == nil && result.Success && jobNeedsImmediateObservation(job.Type) {
		c.requestImmediateObservation()
	}
	return completeErr
}

func (c *Client) requestImmediateObservation() {
	if c.observer == nil {
		return
	}
	select {
	case c.observationWake <- struct{}{}:
	default:
	}
}

func jobNeedsImmediateObservation(jobType string) bool {
	switch jobType {
	case "instance.start", "instance.stop", "instance.restart", "instance.reconcile",
		"instance.hermes.update", "instance.runtime.configure", "instance.messaging.configure", "instance.mcp.configure",
		"instance.recovery.restore", domain.JobRepairHermesProfiles:
		return true
	default:
		return false
	}
}

func (c *Client) completeJob(ctx context.Context, job domain.Job, result domain.JobResult) error {
	completionClient := c.httpClient
	if job.Type == "instance.recovery.create" || job.Type == "instance.hermes.update" {
		extendedClient := *c.httpClient
		extendedClient.Timeout = recoveryCompletionTimeout
		completionClient = &extendedClient
	}
	retryDelay := c.config.InitialRetryDelay
	if retryDelay <= 0 {
		retryDelay = time.Second
	}
	if retryDelay > 5*time.Second {
		retryDelay = 5 * time.Second
	}
	for {
		response, err := c.requestWithLeaseUsingClient(
			ctx, completionClient, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, job.LeaseToken,
		)
		if err == nil {
			statusCode := response.StatusCode
			if statusCode == http.StatusNoContent {
				response.Body.Close()
				return nil
			}
			err = responseError(response)
			response.Body.Close()
			if statusCode == http.StatusConflict {
				return fmt.Errorf("%w: completion rejected: %v", errJobLeaseLost, err)
			}
			if !isTransientCompletionStatus(statusCode) {
				return err
			}
		}
		if ctx.Err() != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return ctx.Err()
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func isTransientCompletionStatus(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
}

func (c *Client) executeHermesUpdate(ctx context.Context, job domain.Job) domain.JobResult {
	var payload domain.HermesUpdatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes update workflow payload: " + err.Error(), InstanceStatus: domain.InstanceFailed}
	}
	result := domain.JobResult{
		Success:         false,
		RecoveryPointID: payload.Backup.RecoveryPointID,
		InstanceStatus:  payload.OriginalStatus,
	}
	report := func(stage string) bool {
		if err := c.reportJobProgress(ctx, job, domain.JobProgress{Stage: stage}); err != nil {
			result.Error = "Hermes update progress could not be recorded: " + err.Error()
			return false
		}
		return true
	}
	execute := func(jobType string, value any, inputArtifact string) domain.JobResult {
		if err := ctx.Err(); err != nil {
			return domain.JobResult{Success: false, Error: err.Error()}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return domain.JobResult{Success: false, Error: "encode Hermes update step: " + err.Error()}
		}
		step := job
		step.Type = jobType
		step.Payload = encoded
		step.InputArtifact = inputArtifact
		return c.executor.Execute(ctx, step)
	}
	action := func(image, imageID string) domain.ActionPayload {
		return domain.ActionPayload{
			InstanceID: payload.Upgrade.InstanceID, Name: payload.Upgrade.Name,
			Image: image, ImageID: imageID, ProjectName: payload.Upgrade.ProjectName,
			ManagedPath: payload.Upgrade.ManagedPath, Provider: payload.Upgrade.Provider,
			Model: payload.Upgrade.Model, Reasoning: payload.Upgrade.Reasoning,
			ServiceTier: payload.Upgrade.ServiceTier, APIPort: payload.Upgrade.APIPort,
			DashboardPort: payload.Upgrade.DashboardPort, PreserveData: true,
		}
	}
	restoreOriginalState := func() string {
		if payload.OriginalStatus != domain.InstanceRunning {
			return domain.InstanceStopped
		}
		started := execute("instance.start", action(payload.Upgrade.CurrentImage, payload.Upgrade.CurrentImageID), "")
		if started.Success {
			return domain.InstanceRunning
		}
		if result.Error != "" {
			result.Error += "; "
		}
		result.Error += "restoring the original running state failed: " + started.Error
		return domain.InstanceFailed
	}

	if !report("PREPARING_RELEASE") {
		return result
	}
	prepared := execute("instance.hermes.prepare", payload.Upgrade, "")
	if !prepared.Success {
		result.Error = prepared.Error
		return result
	}

	if !report("STOPPING") {
		return result
	}
	if payload.OriginalStatus == domain.InstanceRunning {
		stopped := execute("instance.stop", action(payload.Upgrade.CurrentImage, payload.Upgrade.CurrentImageID), "")
		if !stopped.Success {
			result.Error = stopped.Error
			result.InstanceStatus = restoreOriginalState()
			return result
		}
	}
	result.InstanceStatus = domain.InstanceStopped

	if !report("BACKING_UP") {
		result.InstanceStatus = restoreOriginalState()
		return result
	}
	plaintext, backupSHA256, backupSize, reusableBackup, err := c.downloadExistingUpdateBackup(ctx, job)
	if err != nil {
		result.Error = "existing update backup could not be recovered: " + err.Error()
		result.InstanceStatus = restoreOriginalState()
		return result
	}
	if reusableBackup {
		result.RecoverySHA256 = backupSHA256
		result.RecoverySizeBytes = backupSize
	} else {
		backup := execute("instance.recovery.create", payload.Backup, "")
		artifactPath := backup.RecoveryArtifact
		if artifactPath != "" {
			defer os.Remove(artifactPath)
		}
		if len(backup.RecoveryKey) > 0 {
			defer func() {
				for index := range backup.RecoveryKey {
					backup.RecoveryKey[index] = 0
				}
			}()
		}
		if !backup.Success || artifactPath == "" || len(backup.RecoveryKey) != 32 {
			result.Error = backup.Error
			if result.Error == "" {
				result.Error = "instance backup executor did not return a complete encrypted artifact"
			}
			result.InstanceStatus = restoreOriginalState()
			return result
		}
		result.RecoverySHA256 = backup.RecoverySHA256
		result.RecoverySizeBytes = backup.RecoverySizeBytes
		if err := c.uploadRecoveryPoint(ctx, job, backup); err != nil {
			result.Error = "instance backup upload failed: " + err.Error()
			result.InstanceStatus = restoreOriginalState()
			return result
		}
		if err := c.verifyRecoveryPoint(ctx, job, backup); err != nil {
			result.Error = "instance backup verification failed: " + err.Error()
			result.InstanceStatus = restoreOriginalState()
			return result
		}
		plaintext, err = decryptRecoveryArtifact(ctx, backup)
		if err != nil {
			result.Error = "instance backup staging failed: " + err.Error()
			result.InstanceStatus = restoreOriginalState()
			return result
		}
	}
	defer os.Remove(plaintext)

	payload.Upgrade.Rollback.RecoverySHA256 = result.RecoverySHA256
	payload.Upgrade.Rollback.RecoverySizeBytes = result.RecoverySizeBytes
	if !report("INSTALLING") {
		result.InstanceStatus = restoreOriginalState()
		return result
	}
	installed := execute("instance.hermes.upgrade", payload.Upgrade, plaintext)
	if !installed.Success {
		result.Error = installed.Error
		result.InstanceStatus = installed.InstanceStatus
		if installed.InstanceStatus == domain.InstanceStopped {
			result.InstanceStatus = restoreOriginalState()
		}
		return result
	}

	if !report("RESTORING_STATE") {
		result.InstanceStatus = domain.InstanceStopped
		return result
	}
	result.ImageID = installed.ImageID
	if payload.OriginalStatus == domain.InstanceRunning {
		started := execute("instance.start", action(payload.Upgrade.TargetImage, installed.ImageID), "")
		if !started.Success {
			startError := started.Error
			if startError == "" {
				startError = "target runtime start failed without an executor error"
			}
			targetStopped := execute(
				"instance.stop",
				action(payload.Upgrade.TargetImage, installed.ImageID),
				"",
			)
			if !targetStopped.Success {
				stopError := targetStopped.Error
				if stopError == "" {
					stopError = "target runtime stop failed without an executor error"
				}
				result.Error = "Hermes target runtime could not be started: " + startError +
					"; target runtime could not be stopped safely: " + stopError +
					"; automatic backup restore was not attempted"
				result.InstanceStatus = domain.InstanceFailed
				result.ImageID = ""
				return result
			}
			restored := execute("instance.recovery.restore", payload.Upgrade.Rollback, plaintext)
			if !restored.Success || restored.InstanceStatus != domain.InstanceStopped {
				restoreError := restored.Error
				if restoreError == "" {
					restoreError = "backup restore did not return a verified stopped state"
				}
				result.Error = "Hermes target runtime could not be started: " + startError +
					"; automatic backup restore failed: " + restoreError
				result.InstanceStatus = domain.InstanceFailed
				result.ImageID = ""
				return result
			}
			originalStarted := execute(
				"instance.start",
				action(payload.Upgrade.CurrentImage, payload.Upgrade.CurrentImageID),
				"",
			)
			if !originalStarted.Success {
				originalStartError := originalStarted.Error
				if originalStartError == "" {
					originalStartError = "original runtime start failed without an executor error"
				}
				result.Error = "Hermes target runtime could not be started: " + startError +
					"; verified backup was restored but the original runtime could not be restarted: " + originalStartError
				result.InstanceStatus = domain.InstanceFailed
				result.ImageID = ""
				return result
			}
			result.Error = "Hermes target runtime could not be started: " + startError +
				"; verified backup and original runtime were restored"
			result.InstanceStatus = domain.InstanceRunning
			result.ImageID = ""
			return result
		}
		result.InstanceStatus = domain.InstanceRunning
	}

	if !report("VERIFYING_VERSION") {
		result.ImageID = ""
		return result
	}
	result.Success = true
	result.Message = "Hermes " + payload.Upgrade.TargetVersion + " installed, verified, and restored to its original runtime state"
	return result
}

func (c *Client) verifyRecoveryPoint(ctx context.Context, job domain.Job, result domain.JobResult) error {
	response, err := c.requestWithLease(
		ctx, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/recovery-point/verify",
		map[string]any{
			"recovery_point_id": result.RecoveryPointID,
			"sha256":            result.RecoverySHA256,
			"size_bytes":        result.RecoverySizeBytes,
		},
		job.LeaseToken,
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *Client) downloadExistingUpdateBackup(ctx context.Context, job domain.Job) (string, string, int64, bool, error) {
	var payload domain.HermesUpdatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", "", 0, false, errors.New("Hermes update backup payload is invalid")
	}
	if payload.Backup.RecoveryPointID == "" || payload.Backup.MaxBytes < 1 {
		return "", "", 0, false, errors.New("Hermes update backup payload is incomplete")
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/recovery-point",
		nil,
	)
	if err != nil {
		return "", "", 0, false, err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	downloadClient := *c.httpClient
	downloadClient.Timeout = 2*time.Hour + 5*time.Minute
	response, err := downloadClient.Do(request)
	if err != nil {
		return "", "", 0, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", "", 0, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return "", "", 0, false, responseError(response)
	}
	size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 1 || size > payload.Backup.MaxBytes {
		return "", "", 0, false, errors.New("verified update backup size is invalid")
	}
	digest, err := hex.DecodeString(response.Header.Get("X-Fleet-Recovery-SHA256"))
	if err != nil || len(digest) != sha256.Size {
		return "", "", 0, false, errors.New("verified update backup checksum is invalid")
	}
	artifact, err := os.CreateTemp("", ".hermes-fleet-update-retry-*.tar")
	if err != nil {
		return "", "", 0, false, err
	}
	path := artifact.Name()
	keep := false
	defer func() {
		_ = artifact.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := artifact.Chmod(0o600); err != nil {
		return "", "", 0, false, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(artifact, hash), io.LimitReader(response.Body, payload.Backup.MaxBytes+1))
	if err != nil {
		return "", "", 0, false, err
	}
	if written != size || !bytes.Equal(hash.Sum(nil), digest) {
		return "", "", 0, false, errors.New("verified update backup download does not match its metadata")
	}
	if err := artifact.Sync(); err != nil {
		return "", "", 0, false, err
	}
	if err := artifact.Close(); err != nil {
		return "", "", 0, false, err
	}
	keep = true
	return path, hex.EncodeToString(digest), size, true, nil
}

func decryptRecoveryArtifact(ctx context.Context, result domain.JobResult) (string, error) {
	source, err := os.Open(result.RecoveryArtifact)
	if err != nil {
		return "", fmt.Errorf("open encrypted staging artifact: %w", err)
	}
	defer source.Close()
	target, err := os.CreateTemp("", ".hermes-fleet-update-backup-")
	if err != nil {
		return "", fmt.Errorf("create update backup staging file: %w", err)
	}
	path := target.Name()
	keep := false
	defer func() {
		_ = target.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := target.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure update backup staging file: %w", err)
	}
	digest := sha256.New()
	written, err := recoverycodec.Decrypt(
		ctx, io.MultiWriter(target, digest), source, result.RecoveryKey, result.RecoveryPointID+":artifact",
	)
	if err != nil {
		return "", err
	}
	if written != result.RecoverySizeBytes || hex.EncodeToString(digest.Sum(nil)) != result.RecoverySHA256 {
		return "", errors.New("decrypted update backup does not match its verified metadata")
	}
	if err := target.Sync(); err != nil {
		return "", fmt.Errorf("sync update backup staging file: %w", err)
	}
	if err := target.Close(); err != nil {
		return "", fmt.Errorf("close update backup staging file: %w", err)
	}
	keep = true
	return path, nil
}

func (c *Client) downloadRecoveryPoint(ctx context.Context, job domain.Job) (string, error) {
	var payload domain.RecoveryRestorePayload
	if job.Type == "instance.hermes.upgrade" {
		var upgrade domain.HermesUpgradePayload
		if err := json.Unmarshal(job.Payload, &upgrade); err != nil {
			return "", errors.New("Hermes update payload is invalid")
		}
		payload = upgrade.Rollback
	} else if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", errors.New("recovery restore payload is invalid")
	}
	if payload.RecoveryPointID == "" ||
		payload.RecoverySizeBytes < 1 || payload.MaxBytes < payload.RecoverySizeBytes {
		return "", errors.New("recovery restore payload is incomplete")
	}
	digest, err := hex.DecodeString(payload.RecoverySHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("recovery restore checksum is invalid")
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/recovery-point",
		nil,
	)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	downloadClient := *c.httpClient
	downloadClient.Timeout = 2*time.Hour + 5*time.Minute
	response, err := downloadClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", responseError(response)
	}
	artifact, err := os.CreateTemp("", ".hermes-fleet-restore-*.tar")
	if err != nil {
		return "", err
	}
	path := artifact.Name()
	keep := false
	defer func() {
		_ = artifact.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := artifact.Chmod(0o600); err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(artifact, hash), io.LimitReader(response.Body, payload.MaxBytes+1))
	if err != nil {
		return "", err
	}
	if written != payload.RecoverySizeBytes || !bytes.Equal(hash.Sum(nil), digest) {
		return "", errors.New("downloaded recovery point size or checksum does not match")
	}
	if err := artifact.Sync(); err != nil {
		return "", err
	}
	if err := artifact.Close(); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func (c *Client) downloadMessagingConfiguration(ctx context.Context, job domain.Job) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/messaging-config",
		nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError(response)
	}
	const maximumConfigurationBytes = 64 << 10
	secret, err := io.ReadAll(io.LimitReader(response.Body, maximumConfigurationBytes+1))
	if err != nil {
		return nil, err
	}
	if len(secret) == 0 || len(secret) > maximumConfigurationBytes || !json.Valid(secret) {
		clearSecret(secret)
		return nil, errors.New("control plane returned an invalid messaging configuration")
	}
	return secret, nil
}

func (c *Client) downloadMCPConfiguration(ctx context.Context, job domain.Job) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/mcp-config",
		nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError(response)
	}
	const maximumConfigurationBytes = 256 << 10
	secret, err := io.ReadAll(io.LimitReader(response.Body, maximumConfigurationBytes+1))
	if err != nil {
		return nil, err
	}
	if len(secret) == 0 || len(secret) > maximumConfigurationBytes || !json.Valid(secret) {
		clearSecret(secret)
		return nil, errors.New("control plane returned an invalid MCP configuration")
	}
	return secret, nil
}

func (c *Client) downloadChatInput(ctx context.Context, job domain.Job) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/chat-input",
		nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError(response)
	}
	const maximumChatBytes = 64 << 10
	content, err := io.ReadAll(io.LimitReader(response.Body, maximumChatBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || len(content) > maximumChatBytes || !utf8.Valid(content) {
		clearSecret(content)
		return nil, errors.New("control plane returned invalid chat input")
	}
	return content, nil
}

func clearSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (c *Client) reportJobProgress(ctx context.Context, job domain.Job, progress domain.JobProgress) error {
	response, err := c.requestWithLease(
		ctx, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/progress", progress, job.LeaseToken,
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: progress update rejected", errJobLeaseLost)
	}
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *Client) reportChatEvent(ctx context.Context, job domain.Job, event domain.ChatStreamEvent) error {
	retryDelay := c.config.InitialRetryDelay
	if retryDelay <= 0 {
		retryDelay = 250 * time.Millisecond
	}
	if retryDelay > 2*time.Second {
		retryDelay = 2 * time.Second
	}
	for {
		response, err := c.requestWithLease(ctx, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/chat-events", event, job.LeaseToken)
		if err == nil {
			statusCode := response.StatusCode
			if statusCode == http.StatusNoContent {
				response.Body.Close()
				return nil
			}
			err = responseError(response)
			response.Body.Close()
			if statusCode == http.StatusConflict {
				return fmt.Errorf("%w: chat event rejected", errJobLeaseLost)
			}
			if !isTransientCompletionStatus(statusCode) {
				return err
			}
		}
		if ctx.Err() != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return ctx.Err()
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return ctx.Err()
		case <-timer.C:
		}
		if retryDelay < 5*time.Second {
			retryDelay *= 2
		}
	}
}

func (c *Client) uploadChatArtifact(
	ctx context.Context,
	job domain.Job,
	upload domain.ChatArtifactUpload,
) (domain.ChatArtifact, error) {
	artifact := upload.Artifact
	if artifact.ID == "" || artifact.Name == "" || artifact.SizeBytes < 1 || artifact.SHA256 == "" || upload.LocalPath == "" {
		return domain.ChatArtifact{}, errors.New("chat artifact upload metadata is incomplete")
	}
	retryDelay := c.config.InitialRetryDelay
	if retryDelay <= 0 {
		retryDelay = 250 * time.Millisecond
	}
	if retryDelay > 2*time.Second {
		retryDelay = 2 * time.Second
	}
	for {
		file, err := os.Open(upload.LocalPath)
		if err != nil {
			return domain.ChatArtifact{}, err
		}
		request, requestErr := http.NewRequestWithContext(
			ctx, http.MethodPut,
			strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+url.PathEscape(job.ID)+
				"/chat-artifacts/"+url.PathEscape(artifact.ID), file,
		)
		if requestErr != nil {
			_ = file.Close()
			return domain.ChatArtifact{}, requestErr
		}
		request.ContentLength = artifact.SizeBytes
		request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
		request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
		request.Header.Set(leaseTokenHeader, job.LeaseToken)
		request.Header.Set("X-Fleet-Artifact-Name", artifact.Name)
		request.Header.Set("X-Fleet-Artifact-Kind", artifact.Kind)
		request.Header.Set("X-Fleet-Artifact-SHA256", artifact.SHA256)
		request.Header.Set("Content-Type", artifact.MediaType)
		uploadClient := *c.httpClient
		uploadClient.Timeout = 5 * time.Minute
		response, err := uploadClient.Do(request)
		_ = file.Close()
		if err == nil {
			statusCode := response.StatusCode
			if statusCode == http.StatusOK {
				var ready domain.ChatArtifact
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&ready)
				response.Body.Close()
				if decodeErr != nil {
					return domain.ChatArtifact{}, decodeErr
				}
				return ready, nil
			}
			err = responseError(response)
			response.Body.Close()
			if statusCode == http.StatusConflict {
				return domain.ChatArtifact{}, fmt.Errorf("%w: chat artifact upload rejected", errJobLeaseLost)
			}
			if statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge || statusCode == http.StatusInsufficientStorage {
				return domain.ChatArtifact{}, fmt.Errorf("%w: %v", errChatArtifactRejected, err)
			}
			if !isTransientCompletionStatus(statusCode) {
				return domain.ChatArtifact{}, err
			}
		}
		if ctx.Err() != nil {
			return domain.ChatArtifact{}, context.Cause(ctx)
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.ChatArtifact{}, context.Cause(ctx)
		case <-timer.C:
		}
		if retryDelay < 5*time.Second {
			retryDelay *= 2
		}
	}
}

func (c *Client) uploadRecoveryPoint(ctx context.Context, job domain.Job, result domain.JobResult) error {
	if result.RecoveryPointID == "" || result.RecoverySHA256 == "" || result.RecoverySizeBytes < 1 {
		return errors.New("recovery point result metadata is incomplete")
	}
	artifact, err := os.Open(result.RecoveryArtifact)
	if err != nil {
		return fmt.Errorf("open encrypted staging artifact: %w", err)
	}
	reader, writer := io.Pipe()
	decryptionDone := make(chan error, 1)
	go func() {
		written, decryptErr := recoverycodec.Decrypt(
			ctx, writer, artifact, result.RecoveryKey, result.RecoveryPointID+":artifact",
		)
		if closeErr := artifact.Close(); decryptErr == nil && closeErr != nil {
			decryptErr = closeErr
		}
		if decryptErr == nil && written != result.RecoverySizeBytes {
			decryptErr = errors.New("decrypted staging artifact size does not match metadata")
		}
		_ = writer.CloseWithError(decryptErr)
		decryptionDone <- decryptErr
	}()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPut,
		strings.TrimRight(c.config.ControlPlaneURL, "/")+"/api/v1/agent/jobs/"+job.ID+"/recovery-point",
		reader,
	)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-decryptionDone
		return err
	}
	request.ContentLength = result.RecoverySizeBytes
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set(leaseTokenHeader, job.LeaseToken)
	request.Header.Set("X-Fleet-Recovery-Point-ID", result.RecoveryPointID)
	request.Header.Set("X-Fleet-Recovery-SHA256", result.RecoverySHA256)
	request.Header.Set("Content-Type", "application/x-tar")
	uploadClient := *c.httpClient
	uploadClient.Timeout = 2*time.Hour + 5*time.Minute
	response, requestErr := uploadClient.Do(request)
	if requestErr != nil {
		_ = reader.CloseWithError(requestErr)
	}
	decryptErr := <-decryptionDone
	if requestErr != nil {
		return requestErr
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	if decryptErr != nil {
		return decryptErr
	}
	return nil
}

func (c *Client) runLeaseRenewalLoop(ctx context.Context, job domain.Job) error {
	normalDelay := c.config.LeaseRenewInterval
	// Chat cancellation is implemented by fencing the active lease. Renew more
	// frequently while a response is streaming so a quiet upstream request is
	// still interrupted promptly after the operator presses Stop.
	if job.Type == "instance.chat.send" && normalDelay > 5*time.Second {
		normalDelay = 5 * time.Second
	}
	retryDelay := normalDelay / 4
	if retryDelay <= 0 {
		retryDelay = time.Millisecond
	}
	if retryDelay > 5*time.Second {
		retryDelay = 5 * time.Second
	}
	safetyWindow := normalDelay
	if maximum := c.config.LeaseDuration / 5; safetyWindow > maximum {
		safetyWindow = maximum
	}
	if safetyWindow <= 0 {
		safetyWindow = time.Millisecond
	}
	leaseDeadline := time.Now().Add(c.config.LeaseDuration)
	timer := time.NewTimer(normalDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			response, err := c.requestWithLease(
				ctx, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/renew", map[string]any{}, job.LeaseToken,
			)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				if time.Now().Add(retryDelay).After(leaseDeadline.Add(-safetyWindow)) {
					return fmt.Errorf("%w: transient renewal failures persisted until the lease safety deadline: %v", errJobLeaseLost, err)
				}
				timer.Reset(retryDelay)
				continue
			}
			if response.StatusCode != http.StatusNoContent {
				renewalErr := responseError(response)
				response.Body.Close()
				if isTransientLeaseRenewalStatus(response.StatusCode) &&
					!time.Now().Add(retryDelay).After(leaseDeadline.Add(-safetyWindow)) {
					timer.Reset(retryDelay)
					continue
				}
				return fmt.Errorf("%w: %v", errJobLeaseLost, renewalErr)
			}
			response.Body.Close()
			leaseDeadline = time.Now().Add(c.config.LeaseDuration)
			timer.Reset(normalDelay)
		}
	}
}

func isTransientLeaseRenewalStatus(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
}

func (c *Client) request(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	return c.requestWithLease(ctx, method, path, payload, "")
}

func (c *Client) requestWithLease(ctx context.Context, method, path string, payload any, leaseToken string) (*http.Response, error) {
	return c.requestWithLeaseUsingClient(ctx, c.httpClient, method, path, payload, leaseToken)
}

func (c *Client) requestWithLeaseUsingClient(
	ctx context.Context,
	httpClient *http.Client,
	method, path string,
	payload any,
	leaseToken string,
) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.config.ControlPlaneURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.config.HostToken)
	request.Header.Set("X-Fleet-Host-ID", c.config.HostID)
	request.Header.Set("Content-Type", "application/json")
	if leaseToken != "" {
		request.Header.Set(leaseTokenHeader, leaseToken)
	}
	return httpClient.Do(request)
}

func responseError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	trimmed := strings.TrimSpace(string(message))
	return &HTTPError{
		Operation:  "control plane request",
		StatusCode: response.StatusCode,
		Message:    trimmed,
		Text:       fmt.Sprintf("control plane returned %d: %s", response.StatusCode, trimmed),
	}
}
