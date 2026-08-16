package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/capacity"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/compatibility"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/events"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/identity"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/mcpconfig"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/mcpdiscovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/messaging"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/observability"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/providers"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/reliability"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/remoteaccess"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/security"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

const (
	agentVersion               = compatibility.HostAgentVersion
	jobLeaseDuration           = 5 * time.Minute
	leaseTokenHeader           = "X-Fleet-Lease-Token"
	codexDeviceURL             = "https://auth.openai.com/codex/device"
	maximumJSONBodyBytes       = 1 << 20
	defaultOperationPageLimit  = 50
	maximumOperationPageLimit  = 100
	maximumOperationCursorSize = 512
	remoteAccessSealContext    = "system-cloudflare-remote-access:v1"
)

var instanceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
var observationIdentityPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)
var codexUserCodePattern = regexp.MustCompile(`^[A-Z0-9]{4,12}(?:-[A-Z0-9]{4,12})?$`)
var hermesVersionPattern = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+){2}(?:[+-][A-Za-z0-9.-]+)?$`)
var hermesSourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$`)
var hermesCommitPattern = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)

var BuildID = "development"

var errObservationSequence = errors.New("observation is not newer than the stored report")
var errRuntimeRefreshRequired = errors.New("managed runtime refresh is required before runtime synchronization")

type Config struct {
	AdminToken            string
	EnrollmentToken       string
	Address               string
	OperatorURL           string
	DatabasePath          string
	DataDirectory         string
	ReleaseCatalogPath    string
	BackupRetention       int
	HermesCatalog         releases.Catalog
	HermesReleaseSource   releases.Source
	WebDir                string
	OfflineAfter          time.Duration
	ObservationStaleAfter time.Duration
	Sealer                *security.Sealer
	Backups               *backup.Manager
	RecoveryPoints        *recovery.Manager
	ChatArtifacts         *chatartifacts.Manager
	MaxRecoveryPointBytes int64
	RemoteAccess          *remoteaccess.Manager
	CapacityPolicy        capacity.Policy
	Reliability           *reliability.Manager
}

type Server struct {
	config             Config
	store              *store.Store
	logger             *slog.Logger
	mux                *http.ServeMux
	events             *events.Hub
	metrics            *observability.Metrics
	maintenanceOnce    sync.Once
	recoveryPointLocks keyedMutex
	policyRolloutLocks keyedMutex
	campaignLocks      keyedMutex
	deletionLocks      keyedMutex
	mcpDiscover        func(context.Context, mcpdiscovery.Request) ([]mcpdiscovery.Tool, error)
}

type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

func (locks *keyedMutex) lock(key string) func() {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*keyedMutexEntry)
	}
	entry := locks.entries[key]
	if entry == nil {
		entry = &keyedMutexEntry{}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()

		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, key)
		}
		locks.mu.Unlock()
	}
}

func New(config Config, dataStore *store.Store, logger *slog.Logger) *Server {
	if config.Address == "" {
		config.Address = "127.0.0.1:9180"
	}
	if config.OperatorURL == "" {
		config.OperatorURL = "http://127.0.0.1:9180"
	}
	if config.BackupRetention <= 0 {
		config.BackupRetention = 20
	}
	if config.OfflineAfter <= 0 {
		config.OfflineAfter = 30 * time.Second
	}
	if config.ObservationStaleAfter <= 0 {
		config.ObservationStaleAfter = 2 * time.Minute
	}
	if config.MaxRecoveryPointBytes <= 0 {
		config.MaxRecoveryPointBytes = 50 << 30
	}
	streamID, err := identity.New()
	if err != nil {
		streamID = fmt.Sprintf("%s-%d", BuildID, time.Now().UTC().UnixNano())
	}
	discoveryClient := mcpdiscovery.NewClient()
	server := &Server{
		config: config, store: dataStore, logger: logger, mux: http.NewServeMux(),
		events: events.New(streamID), metrics: observability.New(), mcpDiscover: discoveryClient.Discover,
	}
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler {
	return s.loggingMiddleware(securityHeadersMiddleware(s.mux))
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "base-uri 'none'; frame-ancestors 'none'; object-src 'none'; script-src 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// Start runs bounded background reconciliation. It is separated from New so
// tests and embedded callers retain explicit lifecycle control.
func (s *Server) Start(ctx context.Context) {
	s.maintenanceOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			nextArtifactReconcile := time.Time{}
			for {
				now := time.Now().UTC()
				if s.config.ChatArtifacts != nil && !now.Before(nextArtifactReconcile) {
					report, err := s.config.ChatArtifacts.Reconcile(now)
					if err != nil {
						s.logger.Error("reconcile chat artifact storage", "error", err)
					} else if report.Expired > 0 || report.Missing > 0 || report.Invalid > 0 ||
						report.RemovedOrphans > 0 || report.RemovedStaging > 0 {
						s.logger.Info("reconciled chat artifact storage", "expired", report.Expired,
							"missing", report.Missing, "invalid", report.Invalid, "removed_orphans", report.RemovedOrphans,
							"removed_staging", report.RemovedStaging, "ready_bytes", report.TotalBytes)
					}
					nextArtifactReconcile = now.Add(15 * time.Minute)
				}
				s.reconcileExpiredJobs(ctx)
				s.reconcileStalePublicationOperations(ctx)
				s.reconcilePendingInstanceDeletions(ctx)
				s.reconcilePolicyRollouts(ctx)
				s.reconcileCampaigns(ctx)
				s.recordFleetHealth(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (s *Server) recordFleetHealth(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	queue, err := s.store.QueueHealth(ctx, now)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Error("record Fleet queue health", "error", err)
		}
		return
	}
	queueStatus := "healthy"
	queueDetail := fmt.Sprintf("%d pending, %d active, %d expired leases", queue.Pending, queue.Active, queue.ExpiredLeases)
	if queue.ExpiredLeases > 0 || queue.AdmissionRejected {
		queueStatus = "degraded"
	}
	readiness := s.readinessSnapshot(ctx)
	readinessState := "healthy"
	if !readiness.Ready {
		readinessState = "degraded"
	}
	records := []struct {
		component string
		status    string
		detail    string
	}{
		{component: "control_plane", status: readinessState, detail: fmt.Sprintf("database=%s, storage=%s, catalog=%s", readiness.Database, readiness.Storage, readiness.Catalog)},
		{component: "host_queue", status: queueStatus, detail: queueDetail},
	}
	if s.config.RemoteAccess != nil {
		remote := s.config.RemoteAccess.Status()
		if remote.Configured {
			remoteStatus := "healthy"
			remoteDetail := remote.State
			if remote.State != "synced" && remote.State != "registered" {
				remoteStatus = "degraded"
			}
			if remote.LastError != "" {
				remoteDetail += ": " + remote.LastError
			}
			records = append(records, struct {
				component string
				status    string
				detail    string
			}{component: "remote_access", status: remoteStatus, detail: remoteDetail})
		}
	}
	for _, record := range records {
		if err := s.store.RecordFleetHealth(ctx, record.component, record.status, record.detail, now); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error("record Fleet health state", "component", record.component, "error", err)
		}
	}
}

func (s *Server) reconcileExpiredJobs(ctx context.Context) {
	count, err := s.store.ReconcileExpiredJobs(ctx, time.Now().UTC())
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Error("reconcile expired jobs", "error", err)
		return
	}
	s.metrics.JobsReconciled(count)
	if count > 0 {
		s.events.Publish("jobs.reconciled", "")
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /livez", s.liveness)
	s.mux.HandleFunc("GET /livez/", s.liveness)
	s.mux.HandleFunc("GET /readyz", s.readiness)
	s.mux.HandleFunc("GET /readyz/", s.readiness)
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /healthz/", s.health)
	s.mux.HandleFunc("POST /api/v1/agent/enroll", s.enrollAgent)
	s.mux.HandleFunc("POST /api/v1/agent/heartbeat", s.requireAgent(s.agentHeartbeat))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/claim", s.requireAgent(s.claimJob))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/ack", s.requireAgent(s.ackJob))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/renew", s.requireAgent(s.renewJob))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/progress", s.requireAgent(s.jobProgress))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/chat-events", s.requireAgent(s.appendChatEvent))
	s.mux.HandleFunc("PUT /api/v1/agent/jobs/{jobID}/chat-artifacts/{artifactID}", s.requireAgent(s.uploadChatArtifact))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/complete", s.requireAgent(s.completeJob))
	s.mux.HandleFunc("PUT /api/v1/agent/jobs/{jobID}/recovery-point", s.requireAgent(s.uploadRecoveryPoint))
	s.mux.HandleFunc("POST /api/v1/agent/jobs/{jobID}/recovery-point/verify", s.requireAgent(s.verifyRecoveryPointForUpdate))
	s.mux.HandleFunc("GET /api/v1/agent/jobs/{jobID}/recovery-point", s.requireAgent(s.downloadRecoveryPointForRestore))
	s.mux.HandleFunc("GET /api/v1/agent/jobs/{jobID}/messaging-config", s.requireAgent(s.downloadMessagingConfiguration))
	s.mux.HandleFunc("GET /api/v1/agent/jobs/{jobID}/mcp-config", s.requireAgent(s.downloadMCPConfiguration))
	s.mux.HandleFunc("GET /api/v1/agent/jobs/{jobID}/chat-input", s.requireAgent(s.downloadChatInput))
	s.mux.HandleFunc("POST /api/v1/agent/observations/targets", s.requireAgent(s.agentObservationTargets))
	s.mux.HandleFunc("POST /api/v1/agent/observations", s.requireAgent(s.agentObservations))

	s.mux.HandleFunc("GET /api/v1/overview", s.requireAdmin(s.overview))
	s.mux.HandleFunc("GET /api/v1/system", s.requireAdmin(s.systemInfo))
	s.mux.HandleFunc("GET /api/v1/system/runtime-health", s.requireAdmin(s.runtimeHealth))
	s.mux.HandleFunc("GET /api/v1/system/capabilities", s.requireAdmin(s.capabilities))
	s.mux.HandleFunc("GET /api/v1/events", s.requireAdmin(s.stateEvents))
	s.mux.HandleFunc("GET /metrics", s.requireAdmin(s.prometheusMetrics))
	s.mux.HandleFunc("POST /api/v1/system/recovery-drill", s.requireAdmin(s.startRecoveryDrill))
	s.mux.HandleFunc("POST /api/v1/system/recovery-kit/download", s.requireAdmin(s.downloadRecoveryKit))
	s.mux.HandleFunc("GET /api/v1/system/remote-access/configuration", s.requireAdmin(s.getRemoteAccessConfiguration))
	s.mux.HandleFunc("PUT /api/v1/system/remote-access/configuration", s.requireAdmin(s.configureRemoteAccess))
	s.mux.HandleFunc("PUT /api/v1/system/remote-access/cloudflare/admin", s.requireAdmin(s.configureCloudflareAdminBoundary))
	s.mux.HandleFunc("PUT /api/v1/system/remote-access/cloudflare/instance-publishing", s.requireAdmin(s.configureCloudflareInstancePublishing))
	s.mux.HandleFunc("DELETE /api/v1/system/remote-access/configuration", s.requireAdmin(s.disableRemoteAccess))
	s.mux.HandleFunc("POST /api/v1/system/remote-access/reconcile", s.requireAdmin(s.reconcileRemoteAccess))
	s.mux.HandleFunc("GET /api/v1/hosts", s.requireAdmin(s.listHosts))
	s.mux.HandleFunc("GET /api/v1/policies", s.requireAdmin(s.listPolicies))
	s.mux.HandleFunc("POST /api/v1/policies", s.requireAdmin(s.createPolicy))
	s.mux.HandleFunc("PUT /api/v1/policies/{policyID}", s.requireAdmin(s.updatePolicy))
	s.mux.HandleFunc("DELETE /api/v1/policies/{policyID}", s.requireAdmin(s.deletePolicy))
	s.mux.HandleFunc("GET /api/v1/policies/{policyID}/preview", s.requireAdmin(s.previewPolicy))
	s.mux.HandleFunc("POST /api/v1/policies/{policyID}/rollouts", s.requireAdmin(s.startPolicyRollout))
	s.mux.HandleFunc("GET /api/v1/rollouts/{rolloutID}", s.requireAdmin(s.getPolicyRollout))
	s.mux.HandleFunc("POST /api/v1/rollouts/{rolloutID}/pause", s.requireAdmin(s.pausePolicyRollout))
	s.mux.HandleFunc("POST /api/v1/rollouts/{rolloutID}/resume", s.requireAdmin(s.resumePolicyRollout))
	s.mux.HandleFunc("POST /api/v1/rollouts/{rolloutID}/cancel", s.requireAdmin(s.cancelPolicyRollout))
	s.mux.HandleFunc("GET /api/v1/campaigns", s.requireAdmin(s.listCampaigns))
	s.mux.HandleFunc("POST /api/v1/campaigns", s.requireAdmin(s.createCampaign))
	s.mux.HandleFunc("GET /api/v1/campaigns/{campaignID}", s.requireAdmin(s.getCampaign))
	s.mux.HandleFunc("POST /api/v1/campaigns/{campaignID}/retry", s.requireAdmin(s.retryCampaign))
	s.mux.HandleFunc("POST /api/v1/hosts/{hostID}/credentials/rotate", s.requireAdmin(s.rotateHostCredential))
	s.mux.HandleFunc("GET /api/v1/instances", s.requireAdmin(s.listInstances))
	s.mux.HandleFunc("GET /api/v1/chats", s.requireAdmin(s.listChatSessions))
	s.mux.HandleFunc("POST /api/v1/chats", s.requireAdmin(s.createChatSession))
	s.mux.HandleFunc("GET /api/v1/chats/{sessionID}", s.requireAdmin(s.getChatThread))
	s.mux.HandleFunc("PATCH /api/v1/chats/{sessionID}", s.requireAdmin(s.updateChatSessionConfiguration))
	s.mux.HandleFunc("DELETE /api/v1/chats/{sessionID}", s.requireAdmin(s.deleteChatSession))
	s.mux.HandleFunc("POST /api/v1/chats/{sessionID}/messages", s.requireAdmin(s.sendChatMessage))
	s.mux.HandleFunc("GET /api/v1/chats/{sessionID}/events", s.requireAdmin(s.chatEvents))
	s.mux.HandleFunc("GET /api/v1/chats/{sessionID}/artifacts/{artifactID}", s.requireAdmin(s.getChatArtifact))
	s.mux.HandleFunc("GET /api/v1/chats/{sessionID}/artifacts/{artifactID}/preview", s.requireAdmin(s.previewChatArtifact))
	s.mux.HandleFunc("GET /api/v1/chats/{sessionID}/artifacts/{artifactID}/download", s.requireAdmin(s.downloadChatArtifact))
	s.mux.HandleFunc("POST /api/v1/chats/{sessionID}/cancel", s.requireAdmin(s.cancelChatRun))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/profiles", s.requireAdmin(s.listHermesProfiles))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/profiles/refresh", s.requireAdmin(s.refreshHermesProfiles))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/profiles/repair", s.requireAdmin(s.repairHermesProfiles))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/profiles", s.requireAdmin(s.createHermesProfile))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/profiles/{profileName}/active", s.requireAdmin(s.activateHermesProfile))
	s.mux.HandleFunc("DELETE /api/v1/instances/{instanceID}/profiles/{profileName}", s.requireAdmin(s.deleteHermesProfile))
	s.mux.HandleFunc("GET /api/v1/artifacts", s.requireAdmin(s.listArtifacts))
	s.mux.HandleFunc("GET /api/v1/artifacts/usage", s.requireAdmin(s.artifactUsage))
	s.mux.HandleFunc("DELETE /api/v1/artifacts/{artifactID}", s.requireAdmin(s.deleteArtifact))
	s.mux.HandleFunc("GET /api/v1/hermes-releases", s.requireAdmin(s.listHermesReleases))
	s.mux.HandleFunc("POST /api/v1/instances", s.requireAdmin(s.createInstance))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/actions", s.requireAdmin(s.instanceAction))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/delete-cleanup/retry", s.requireAdmin(s.retryInstanceDeletionCleanup))
	s.mux.HandleFunc("PUT /api/v1/instances/{instanceID}/public-dashboard", s.requireAdmin(s.publishInstanceDashboard))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/runtime-remediation/cancel", s.requireAdmin(s.cancelRuntimeRemediation))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/hermes-update", s.requireAdmin(s.getHermesUpdate))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/hermes-update", s.requireAdmin(s.startHermesUpdate))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/credentials", s.requireAdmin(s.requestCredentials))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/codex-auth", s.requireAdmin(s.startCodexAuth))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/codex-auth/{operationID}", s.requireAdmin(s.getCodexAuth))
	s.mux.HandleFunc("DELETE /api/v1/instances/{instanceID}/codex-auth/{operationID}", s.requireAdmin(s.cancelCodexAuth))
	s.mux.HandleFunc("PUT /api/v1/instances/{instanceID}/codex-configuration", s.requireAdmin(s.configureCodex))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/messaging", s.requireAdmin(s.getMessagingConfiguration))
	s.mux.HandleFunc("PUT /api/v1/instances/{instanceID}/messaging", s.requireAdmin(s.configureMessaging))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/mcp", s.requireAdmin(s.getMCPConfiguration))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/mcp/discover", s.requireAdmin(s.discoverMCPTools))
	s.mux.HandleFunc("PUT /api/v1/instances/{instanceID}/mcp", s.requireAdmin(s.configureMCP))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/observations/refresh", s.requireAdmin(s.requestObservationRefresh))
	s.mux.HandleFunc("GET /api/v1/instances/{instanceID}/recovery-points", s.requireAdmin(s.listRecoveryPoints))
	s.mux.HandleFunc("POST /api/v1/instances/{instanceID}/recovery-points", s.requireAdmin(s.createRecoveryPoint))
	s.mux.HandleFunc("POST /api/v1/recovery-points/{recoveryPointID}/verify", s.requireAdmin(s.verifyRecoveryPoint))
	s.mux.HandleFunc("POST /api/v1/recovery-points/{recoveryPointID}/restore", s.requireAdmin(s.restoreRecoveryPoint))
	s.mux.HandleFunc("GET /api/v1/recovery-points/{recoveryPointID}/download", s.requireAdmin(s.downloadRecoveryPoint))
	s.mux.HandleFunc("DELETE /api/v1/recovery-points/{recoveryPointID}", s.requireAdmin(s.deleteRecoveryPoint))
	s.mux.HandleFunc("GET /api/v1/credential-reveals/{operationID}", s.requireAdmin(s.getCredentialReveal))
	s.mux.HandleFunc("GET /api/v1/operations", s.requireAdmin(s.listOperations))
	s.mux.HandleFunc("GET /api/v1/operations/{operationID}", s.requireAdmin(s.getOperation))
	s.mux.HandleFunc("GET /api/v1/backups", s.requireAdmin(s.listBackups))
	s.mux.HandleFunc("POST /api/v1/backups", s.requireAdmin(s.createBackup))
	s.mux.HandleFunc("POST /api/v1/backups/{backupID}/verify", s.requireAdmin(s.verifyBackup))
	s.mux.HandleFunc("GET /api/v1/backups/{backupID}/download", s.requireAdmin(s.downloadBackup))
	s.mux.HandleFunc("DELETE /api/v1/backups/{backupID}", s.requireAdmin(s.deleteBackup))

	s.mux.HandleFunc("GET /", s.serveWeb)
}

func (s *Server) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive", "version": agentVersion, "build_id": BuildID})
}

type readinessStatus struct {
	Ready       bool            `json:"ready"`
	Database    string          `json:"database"`
	Storage     string          `json:"storage"`
	Catalog     string          `json:"release_catalog"`
	Capacity    capacity.Status `json:"capacity"`
	LastChecked time.Time       `json:"last_checked"`
}

func (s *Server) readinessSnapshot(ctx context.Context) readinessStatus {
	status := readinessStatus{Ready: true, Database: "ready", Storage: "ready", Catalog: "ready", LastChecked: time.Now().UTC()}
	if err := s.store.Ready(ctx); err != nil {
		status.Ready = false
		status.Database = "unavailable"
		s.logger.Error("readiness database check", "error", err)
	}
	dataDirectory := s.config.DataDirectory
	if dataDirectory == "" {
		dataDirectory = filepath.Dir(s.config.DatabasePath)
	}
	capacityStatus, err := capacity.Probe(dataDirectory, s.config.CapacityPolicy)
	if err != nil {
		status.Ready = false
		status.Storage = "unavailable"
		s.logger.Error("readiness storage check", "error", err)
	} else {
		status.Capacity = capacityStatus
		if !capacityStatus.OperationsSafe {
			status.Ready = false
			status.Storage = "below_safety_reserve"
		}
	}
	if s.config.ReleaseCatalogPath != "" {
		if _, err := releases.LoadCatalog(s.config.ReleaseCatalogPath); err != nil {
			status.Ready = false
			status.Catalog = "unavailable"
			s.logger.Error("readiness release catalog check", "error", err)
		}
	} else if len(s.config.HermesCatalog.Releases) == 0 {
		status.Ready = false
		status.Catalog = "empty"
	}
	return status
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	status := s.readinessSnapshot(r.Context())
	httpStatus := http.StatusOK
	if !status.Ready {
		httpStatus = http.StatusServiceUnavailable
	}
	writeJSON(w, httpStatus, status)
}

// health remains a compatibility alias for readiness. Container health must
// represent whether Fleet can safely serve stateful requests, not only whether
// the HTTP process is alive.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status := s.readinessSnapshot(r.Context())
	payload := map[string]any{"status": "ok", "version": agentVersion, "build_id": BuildID, "ready": status.Ready}
	httpStatus := http.StatusOK
	if !status.Ready {
		payload["status"] = "unavailable"
		payload["checks"] = status
		httpStatus = http.StatusServiceUnavailable
	}
	writeJSON(w, httpStatus, payload)
}

type systemInfoResponse struct {
	FleetVersion    string                 `json:"fleet_version"`
	BuildID         string                 `json:"build_id"`
	OperatorURL     string                 `json:"operator_url"`
	DatabasePath    string                 `json:"database_path"`
	BackupRetention int                    `json:"backup_retention"`
	Readiness       readinessStatus        `json:"readiness"`
	Capacity        capacity.Status        `json:"capacity"`
	RecoveryDrill   reliability.State      `json:"recovery_drill"`
	RemoteAccess    remoteaccess.Status    `json:"remote_access"`
	Capabilities    compatibility.Manifest `json:"capabilities"`
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	remoteAccess := remoteaccess.Status{State: "disabled"}
	if s.config.RemoteAccess != nil {
		remoteAccess = s.config.RemoteAccess.Status()
	}
	readiness := s.readinessSnapshot(r.Context())
	drill := reliability.State{Status: reliability.DrillNever}
	if s.config.Reliability != nil {
		drill = s.config.Reliability.Status()
	}
	writeJSON(w, http.StatusOK, systemInfoResponse{
		FleetVersion: agentVersion, BuildID: BuildID, OperatorURL: s.config.OperatorURL, DatabasePath: s.config.DatabasePath,
		BackupRetention: s.config.BackupRetention, Readiness: readiness, Capacity: readiness.Capacity, RecoveryDrill: drill, RemoteAccess: remoteAccess,
		Capabilities: compatibility.Current(agentVersion),
	})
}

type runtimeHealthResponse struct {
	Status           string                      `json:"status"`
	StreamID         string                      `json:"stream_id"`
	StateRevision    uint64                      `json:"state_revision"`
	EventSubscribers int                         `json:"event_subscribers"`
	Compatibility    compatibility.Manifest      `json:"compatibility"`
	Queue            store.QueueHealth           `json:"queue"`
	Metrics          observability.Snapshot      `json:"metrics"`
	Components       []store.FleetHealthState    `json:"components"`
	RecentIncidents  []store.FleetHealthIncident `json:"recent_incidents"`
}

func (s *Server) runtimeHealth(w http.ResponseWriter, r *http.Request) {
	queue, err := s.store.QueueHealth(r.Context(), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "runtime health could not be read")
		return
	}
	components, err := s.store.ListFleetHealthStates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Fleet health history could not be read")
		return
	}
	incidents, err := s.store.ListFleetHealthIncidents(r.Context(), 10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Fleet health incidents could not be read")
		return
	}
	remoteAccessConfigured := false
	if s.config.RemoteAccess != nil {
		remoteAccessConfigured = s.config.RemoteAccess.Status().Configured
	}
	components, incidents = visibleFleetHealthHistory(components, incidents, remoteAccessConfigured)
	streamID, revision := s.events.Snapshot()
	status := "healthy"
	if queue.ExpiredLeases > 0 || queue.AdmissionRejected {
		status = "degraded"
	}
	for _, component := range components {
		if component.Status == "degraded" {
			status = "degraded"
			break
		}
	}
	writeJSON(w, http.StatusOK, runtimeHealthResponse{
		Status: status, StreamID: streamID, StateRevision: revision,
		EventSubscribers: s.events.Subscribers(), Compatibility: compatibility.Current(agentVersion),
		Queue: queue, Metrics: s.metrics.Snapshot(), Components: components, RecentIncidents: incidents,
	})
}

func visibleFleetHealthHistory(components []store.FleetHealthState, incidents []store.FleetHealthIncident, remoteAccessConfigured bool) ([]store.FleetHealthState, []store.FleetHealthIncident) {
	if remoteAccessConfigured {
		return components, incidents
	}
	visibleComponents := make([]store.FleetHealthState, 0, len(components))
	for _, component := range components {
		if component.Component != "remote_access" {
			visibleComponents = append(visibleComponents, component)
		}
	}
	visibleIncidents := make([]store.FleetHealthIncident, 0, len(incidents))
	for _, incident := range incidents {
		if incident.Component != "remote_access" {
			visibleIncidents = append(visibleIncidents, incident)
		}
	}
	return visibleComponents, visibleIncidents
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, compatibility.Current(agentVersion))
}

func (s *Server) prometheusMetrics(w http.ResponseWriter, _ *http.Request) {
	s.metrics.WritePrometheus(w)
}

func (s *Server) stateEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "state streaming is unavailable")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	streamID, revision := s.events.Snapshot()
	initial := events.Event{StreamID: streamID, Revision: revision, Type: "state.snapshot", OccurredAt: time.Now().UTC()}
	if err := writeStateEvent(w, initial); err != nil {
		return
	}
	flusher.Flush()
	updates, unsubscribe := s.events.Subscribe()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-updates:
			if err := writeStateEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeStateEvent(w io.Writer, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s:%d\nevent: fleet-state\ndata: %s\n\n", event.StreamID, event.Revision, payload)
	return err
}

func (s *Server) startRecoveryDrill(w http.ResponseWriter, _ *http.Request) {
	if s.config.Reliability == nil {
		writeError(w, http.StatusServiceUnavailable, "recovery drill service is not configured")
		return
	}
	state, err := s.config.Reliability.Start()
	if errors.Is(err, reliability.ErrDrillRunning) {
		writeJSON(w, http.StatusAccepted, state)
		return
	}
	if err != nil {
		s.logger.Error("start recovery drill", "error", err)
		writeError(w, http.StatusInternalServerError, "recovery drill could not be started")
		return
	}
	writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) downloadRecoveryKit(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.hermesCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Hermes release catalog is unavailable")
		return
	}
	kit, err := reliability.NewRecoveryKit(
		s.config.Backups, s.config.RecoveryPoints, s.store.ListInstances,
		agentVersion, BuildID, catalog,
	)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "recovery kit service is not configured")
		return
	}
	filename := "hermes-fleet-recovery-kit-" + time.Now().UTC().Format("20060102T150405Z") + ".tar"
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	tracker := &responseWriteTracker{ResponseWriter: w}
	if _, err := kit.Export(r.Context(), tracker); err != nil {
		if tracker.written > 0 {
			s.logger.Error("recovery kit stream interrupted", "error", err, "bytes_written", tracker.written)
			return
		}
		w.Header().Del("Content-Disposition")
		w.Header().Del("Pragma")
		if errors.Is(err, reliability.ErrRecoveryKitIncomplete) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.logger.Error("export recovery kit", "error", err)
		writeError(w, http.StatusInternalServerError, "recovery kit could not be exported")
	}
}

type responseWriteTracker struct {
	http.ResponseWriter
	written int64
}

func (writer *responseWriteTracker) Write(data []byte) (int, error) {
	written, err := writer.ResponseWriter.Write(data)
	writer.written += int64(written)
	return written, err
}

func (s *Server) requireOperationCapacity(w http.ResponseWriter) bool {
	dataDirectory := s.config.DataDirectory
	if dataDirectory == "" {
		dataDirectory = filepath.Dir(s.config.DatabasePath)
	}
	_, err := capacity.Require(dataDirectory, s.config.CapacityPolicy)
	if err == nil {
		return true
	}
	s.logger.Warn("block state-expanding operation", "error", err)
	writeError(w, http.StatusInsufficientStorage, "Fleet data storage is below the configured safety reserve; free storage or remove old backups before retrying")
	return false
}

func (s *Server) reconcileRemoteAccess(w http.ResponseWriter, r *http.Request) {
	if s.config.RemoteAccess == nil || !s.config.RemoteAccess.Status().Configured {
		writeError(w, http.StatusConflict, "remote access is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.config.RemoteAccess.Reconcile(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.config.RemoteAccess.Status())
}

type remoteAccessConfigurationRequest struct {
	Mode                 string                          `json:"mode"`
	AdminTunnelToken     string                          `json:"admin_tunnel_token"`
	InstancesTunnelToken string                          `json:"instances_tunnel_token"`
	AdminHostname        string                          `json:"admin_hostname"`
	AdminURL             string                          `json:"admin_url"`
	InstanceEndpoints    []remoteaccess.InstanceEndpoint `json:"instance_endpoints"`
}

type cloudflareAdminConfigurationRequest struct {
	TunnelToken string `json:"tunnel_token"`
	Hostname    string `json:"hostname"`
}

type cloudflareInstancePublishingRequest struct {
	TunnelToken    string `json:"tunnel_token"`
	AccountID      string `json:"account_id"`
	ZoneID         string `json:"zone_id"`
	APIToken       string `json:"api_token"`
	FleetNamespace string `json:"fleet_namespace"`
}

func (s *Server) getRemoteAccessConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.config.RemoteAccess == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.config.RemoteAccess.Configuration(r.Context()))
}

func (s *Server) configureCloudflareAdminBoundary(w http.ResponseWriter, r *http.Request) {
	var request cloudflareAdminConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.configureCloudflareBoundary(w, r, func(config *remoteaccess.Config) {
		if strings.TrimSpace(request.TunnelToken) != "" {
			config.Cloudflare.AdminTunnelToken = request.TunnelToken
		}
		config.Cloudflare.AdminHostname = request.Hostname
	})
}

func (s *Server) configureCloudflareInstancePublishing(w http.ResponseWriter, r *http.Request) {
	if s.config.RemoteAccess == nil || s.config.Sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	var request cloudflareInstancePublishingRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, previous, hadPrevious, err := s.readRemoteAccessConfiguration(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hadPrevious && previous.Mode != remoteaccess.ModeManagedCloudflare {
		writeError(w, http.StatusConflict, "disable the current remote access mode before configuring instance publishing")
		return
	}
	namespace, err := cloudflare.NormalizeFleetNamespace(request.FleetNamespace)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	previousNamespace := previous.Cloudflare.RouteAutomation.FleetNamespace
	if previousNamespace != "" && previousNamespace != namespace {
		instances, listErr := s.store.ListInstances(r.Context())
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "instances could not be checked before changing the Fleet namespace")
			return
		}
		if namespaceErr := validateFleetNamespaceChange(previousNamespace, namespace, instances); namespaceErr != nil {
			writeError(w, http.StatusConflict, namespaceErr.Error())
			return
		}
	}
	candidate := previous
	if !hadPrevious {
		candidate = remoteaccess.Config{Mode: remoteaccess.ModeManagedCloudflare}
	}
	candidate.Mode = remoteaccess.ModeManagedCloudflare
	if strings.TrimSpace(request.TunnelToken) != "" {
		candidate.Cloudflare.InstancesTunnelToken = request.TunnelToken
	}
	if strings.TrimSpace(request.APIToken) != "" {
		candidate.Cloudflare.RouteAutomation.APIToken = request.APIToken
	}
	candidate.Cloudflare.RouteAutomation.AccountID = strings.TrimSpace(request.AccountID)
	candidate.Cloudflare.RouteAutomation.ZoneID = strings.TrimSpace(request.ZoneID)
	candidate.Cloudflare.RouteAutomation.FleetNamespace = namespace
	candidate.Cloudflare.RouteAutomation.TunnelID = ""
	operationID, err := identity.New()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create operation identity")
		return
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, Type: "CONNECT_INSTANCE_PUBLISHING", Status: domain.OperationPending,
		Summary:   "Connect and verify instance publishing",
		Metadata:  operationMetadata(map[string]any{"scope": "system.remote_access.instance_publishing"}),
		Progress:  &domain.JobProgress{Stage: "VALIDATING_TUNNEL_TOKEN", Steps: instancePublishingConnectionSteps("VALIDATING_TUNNEL_TOKEN", "running", "")},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateControlPlaneOperation(r.Context(), operation); err != nil {
		writeError(w, http.StatusInternalServerError, "instance publishing operation could not be created")
		return
	}
	go s.runConnectInstancePublishing(operation.ID, candidate, previous, hadPrevious)
	writeJSON(w, http.StatusAccepted, operation)
}

func validateFleetNamespaceChange(previous, next string, instances []domain.Instance) error {
	if previous == "" || previous == next {
		return nil
	}
	for _, instance := range instances {
		if instance.PublicHostname != "" && instance.Status != domain.InstanceDeleted {
			return errors.New("Fleet namespace is locked while an instance dashboard is published; unpublish all dashboards before changing it")
		}
	}
	return nil
}

func instancePublishingConnectionSteps(stage, status, detail string) []domain.OperationStep {
	names := []string{"VALIDATING_TUNNEL_TOKEN", "VERIFYING_CLOUDFLARE_API", "STARTING_CONNECTOR", "VERIFYING_CONNECTOR"}
	steps := make([]domain.OperationStep, 0, len(names))
	reached := true
	for _, name := range names {
		stepStatus := "pending"
		stepDetail := ""
		if name == stage {
			stepStatus, stepDetail, reached = status, detail, false
		} else if reached {
			stepStatus = "succeeded"
		}
		steps = append(steps, domain.OperationStep{Stage: name, Status: stepStatus, Detail: stepDetail})
	}
	return steps
}

func (s *Server) runConnectInstancePublishing(operationID string, candidate, previous remoteaccess.Config, hadPrevious bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	update := func(stage, status, detail, actionCode, operationStatus string) {
		_ = s.store.UpdateControlPlaneOperation(ctx, operationID, operationStatus, domain.JobProgress{
			Stage: stage, Detail: detail, ActionCode: actionCode,
			Steps: instancePublishingConnectionSteps(stage, status, detail),
		}, map[bool]string{true: detail, false: ""}[operationStatus == domain.OperationFailed], time.Now().UTC())
		s.events.Publish("operation.changed", operationID)
	}
	update("VALIDATING_TUNNEL_TOKEN", "running", "", "", domain.OperationRunning)
	tunnelID, err := cloudflare.TunnelIDFromToken(candidate.Cloudflare.InstancesTunnelToken)
	if err != nil {
		update("VALIDATING_TUNNEL_TOKEN", "failed", err.Error(), "replace_tunnel_token", domain.OperationFailed)
		return
	}
	candidate.Cloudflare.InstancesTunnelID = tunnelID
	candidate.Cloudflare.RouteAutomation.TunnelID = tunnelID
	update("VERIFYING_CLOUDFLARE_API", "running", "", "", domain.OperationRunning)
	verifiedTunnelID, verifiedZone, err := s.config.RemoteAccess.VerifyInstancePublishing(ctx, candidate)
	if err != nil {
		action := "retry"
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "api token") || strings.Contains(lower, "http 401") || strings.Contains(lower, "http 403") {
			action = "replace_api_token"
		}
		update("VERIFYING_CLOUDFLARE_API", "failed", err.Error(), action, domain.OperationFailed)
		return
	}
	candidate.Cloudflare.InstancesTunnelID = verifiedTunnelID
	candidate.Cloudflare.RouteAutomation.TunnelID = verifiedTunnelID
	candidate.Cloudflare.RouteAutomation.ZoneName = verifiedZone
	update("STARTING_CONNECTOR", "running", "", "", domain.OperationRunning)
	if err := s.applyRemoteAccessConfiguration(ctx, candidate, previous, hadPrevious); err != nil {
		update("STARTING_CONNECTOR", "failed", err.Error(), "replace_tunnel_token", domain.OperationFailed)
		return
	}
	update("VERIFYING_CONNECTOR", "running", "", "", domain.OperationRunning)
	if err := s.config.RemoteAccess.Reconcile(ctx); err != nil {
		update("VERIFYING_CONNECTOR", "failed", err.Error(), "retry", domain.OperationFailed)
		return
	}
	status := s.config.RemoteAccess.Status()
	if status.Instances.ConnectorState != "" && status.Instances.ConnectorState != "running" {
		detail := status.Instances.ConnectorError
		if detail == "" {
			detail = "Instance publishing connector is not running"
		}
		update("VERIFYING_CONNECTOR", "failed", detail, "retry", domain.OperationFailed)
		return
	}
	update("VERIFYING_CONNECTOR", "succeeded", "Instance publishing is connected and verified", "", domain.OperationSucceeded)
}

func (s *Server) configureCloudflareBoundary(w http.ResponseWriter, r *http.Request, update func(*remoteaccess.Config)) {
	if s.config.RemoteAccess == nil || s.config.Sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	_, previousConfig, hadPrevious, err := s.readRemoteAccessConfiguration(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hadPrevious && previousConfig.Mode != remoteaccess.ModeManagedCloudflare {
		writeError(w, http.StatusConflict, "disable the current remote access mode before configuring Cloudflare tunnels")
		return
	}
	config := previousConfig
	if !hadPrevious {
		config = remoteaccess.Config{Mode: remoteaccess.ModeManagedCloudflare}
	}
	config.Mode = remoteaccess.ModeManagedCloudflare
	update(&config)
	if err := s.applyRemoteAccessConfiguration(r.Context(), config, previousConfig, hadPrevious); err != nil {
		var statusErr *remoteAccessApplyError
		if errors.As(err, &statusErr) {
			writeError(w, statusErr.Status, statusErr.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.config.RemoteAccess.Trigger()
	writeJSON(w, http.StatusOK, s.config.RemoteAccess.Status())
}

type remoteAccessApplyError struct {
	Status  int
	Message string
}

func (err *remoteAccessApplyError) Error() string { return err.Message }

func (s *Server) readRemoteAccessConfiguration(ctx context.Context) (store.RemoteAccessConfigRecord, remoteaccess.Config, bool, error) {
	record, err := s.store.GetRemoteAccessConfig(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return store.RemoteAccessConfigRecord{}, remoteaccess.Config{}, false, nil
	}
	if err != nil {
		return store.RemoteAccessConfigRecord{}, remoteaccess.Config{}, false, errors.New("stored remote access configuration could not be read")
	}
	opened, err := s.config.Sealer.Open(record.Ciphertext, remoteAccessSealContext)
	if err != nil {
		return store.RemoteAccessConfigRecord{}, remoteaccess.Config{}, false, errors.New("stored remote access configuration could not be decrypted")
	}
	config, err := remoteaccess.DecodeConfig(opened)
	if err != nil {
		return store.RemoteAccessConfigRecord{}, remoteaccess.Config{}, false, errors.New("stored remote access configuration could not be decrypted")
	}
	return record, config, true, nil
}

func (s *Server) applyRemoteAccessConfiguration(ctx context.Context, config, previousConfig remoteaccess.Config, hadPrevious bool) error {
	payload, err := json.Marshal(config)
	if err != nil {
		return errors.New("remote access configuration could not be encoded")
	}
	ciphertext, err := s.config.Sealer.Seal(payload, remoteAccessSealContext)
	if err != nil {
		return errors.New("remote access configuration could not be encrypted")
	}
	applyContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.config.RemoteAccess.Configure(applyContext, config); err != nil {
		return &remoteAccessApplyError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	record := store.RemoteAccessConfigRecord{Ciphertext: ciphertext, UpdatedAt: time.Now().UTC()}
	if err := s.store.PutRemoteAccessConfig(ctx, record); err == nil {
		return nil
	}
	rollbackContext, rollbackCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer rollbackCancel()
	if hadPrevious {
		_ = s.config.RemoteAccess.Configure(rollbackContext, previousConfig)
	} else {
		_ = s.config.RemoteAccess.Disable(rollbackContext)
	}
	return errors.New("remote access configuration could not be saved")
}

func (s *Server) configureRemoteAccess(w http.ResponseWriter, r *http.Request) {
	if s.config.RemoteAccess == nil || s.config.Sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	var request remoteAccessConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	previousRecord, previousRecordErr := s.store.GetRemoteAccessConfig(r.Context())
	var previousConfig remoteaccess.Config
	if previousRecordErr == nil {
		opened, err := s.config.Sealer.Open(previousRecord.Ciphertext, remoteAccessSealContext)
		if err == nil {
			previousConfig, err = remoteaccess.DecodeConfig(opened)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "stored remote access configuration could not be decrypted")
			return
		}
	} else if !errors.Is(previousRecordErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "stored remote access configuration could not be read")
		return
	}
	request.Mode = strings.TrimSpace(request.Mode)
	if request.Mode == "" {
		request.Mode = remoteaccess.ModeManagedCloudflare
	}
	if request.Mode == remoteaccess.ModeManagedCloudflare && previousRecordErr == nil && previousConfig.Mode == remoteaccess.ModeManagedCloudflare {
		if strings.TrimSpace(request.AdminTunnelToken) == "" {
			request.AdminTunnelToken = previousConfig.Cloudflare.AdminTunnelToken
		}
		if strings.TrimSpace(request.InstancesTunnelToken) == "" {
			request.InstancesTunnelToken = previousConfig.Cloudflare.InstancesTunnelToken
		}
	}
	instanceURLs := make(map[string]string, len(request.InstanceEndpoints))
	for _, endpoint := range request.InstanceEndpoints {
		instanceID := strings.TrimSpace(endpoint.InstanceID)
		if _, duplicate := instanceURLs[instanceID]; duplicate && request.Mode == remoteaccess.ModeExistingEndpoints {
			writeError(w, http.StatusBadRequest, "instance endpoint IDs must be unique")
			return
		}
		instanceURLs[instanceID] = endpoint.DashboardURL
	}
	config := remoteaccess.Config{
		Mode: request.Mode,
		Cloudflare: cloudflare.Config{
			AdminTunnelToken: request.AdminTunnelToken, InstancesTunnelToken: request.InstancesTunnelToken,
			AdminHostname: request.AdminHostname,
		},
		Existing: remoteaccess.ExistingEndpointsConfig{AdminURL: request.AdminURL, InstanceDashboardURLs: instanceURLs},
	}
	if request.Mode == remoteaccess.ModeManagedCloudflare && previousRecordErr == nil && previousConfig.Mode == remoteaccess.ModeManagedCloudflare {
		config.Cloudflare.RouteAutomation = previousConfig.Cloudflare.RouteAutomation
	}
	payload, err := json.Marshal(config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "remote access configuration could not be encoded")
		return
	}
	ciphertext, err := s.config.Sealer.Seal(payload, remoteAccessSealContext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "remote access configuration could not be encrypted")
		return
	}
	record := store.RemoteAccessConfigRecord{Ciphertext: ciphertext, UpdatedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.config.RemoteAccess.Configure(ctx, config); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.store.PutRemoteAccessConfig(r.Context(), record); err != nil {
		rollbackContext, rollbackCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer rollbackCancel()
		if previousRecordErr == nil {
			_ = s.config.RemoteAccess.Configure(rollbackContext, previousConfig)
		} else {
			_ = s.config.RemoteAccess.Disable(rollbackContext)
		}
		writeError(w, http.StatusInternalServerError, "remote access configuration could not be saved")
		return
	}
	writeJSON(w, http.StatusOK, s.config.RemoteAccess.Status())
}

func (s *Server) disableRemoteAccess(w http.ResponseWriter, r *http.Request) {
	if s.config.RemoteAccess == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	disableErr := s.config.RemoteAccess.Disable(ctx)
	if disableErr != nil {
		writeError(w, http.StatusBadGateway, "remote access cleanup is incomplete: "+disableErr.Error())
		return
	}
	if err := s.store.DeleteRemoteAccessConfig(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "remote access configuration could not be removed")
		return
	}
	writeJSON(w, http.StatusOK, s.config.RemoteAccess.Status())
}

type enrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	Name            string `json:"name"`
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	AgentVersion    string `json:"agent_version"`
}

func (s *Server) enrollAgent(w http.ResponseWriter, r *http.Request) {
	var request enrollRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !security.Equal(request.EnrollmentToken, s.config.EnrollmentToken) {
		writeError(w, http.StatusUnauthorized, "invalid enrollment token")
		return
	}
	if !instanceNamePattern.MatchString(request.Name) {
		writeError(w, http.StatusBadRequest, "host name must match ^[a-z][a-z0-9-]{2,31}$")
		return
	}
	hostID, err := identity.New()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create host identity")
		return
	}
	hostToken, err := security.GenerateToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create host credential")
		return
	}
	now := time.Now().UTC()
	host := domain.Host{
		ID: hostID, Name: request.Name, Hostname: request.Hostname, OS: request.OS, Arch: request.Arch,
		AgentVersion: request.AgentVersion, LastSeenAt: now, CreatedAt: now,
	}
	if err := s.store.EnrollHost(r.Context(), host, security.HashToken(hostToken)); err != nil {
		writeError(w, http.StatusConflict, "host name is already enrolled")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"host_id": hostID, "host_token": hostToken})
}

type rotateHostCredentialRequest struct {
	ConfirmName  string `json:"confirm_name"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

func (s *Server) rotateHostCredential(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("hostID")
	if !observationIdentityPattern.MatchString(hostID) {
		writeError(w, http.StatusBadRequest, "host identity is invalid")
		return
	}
	var request rotateHostCredentialRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !instanceNamePattern.MatchString(request.ConfirmName) {
		writeError(w, http.StatusBadRequest, "host name must match ^[a-z][a-z0-9-]{2,31}$")
		return
	}
	if request.Hostname == "" || request.OS == "" || request.Arch == "" || request.AgentVersion == "" ||
		len(request.Hostname) > 255 || len(request.OS) > 64 || len(request.Arch) > 64 || len(request.AgentVersion) > 64 {
		writeError(w, http.StatusBadRequest, "hostname, os, arch, and agent_version are required")
		return
	}
	if request.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "Host Agent version must match Hermes Fleet Manager")
		return
	}
	hostToken, err := security.GenerateToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create host credential")
		return
	}
	err = s.store.RotateHostCredential(
		r.Context(), hostID, request.ConfirmName, request.Hostname, request.OS, request.Arch,
		security.HashToken(hostToken),
	)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "host not found")
		return
	case errors.Is(err, store.ErrHostIdentityMismatch):
		writeError(w, http.StatusConflict, "host identity confirmation does not match")
		return
	case errors.Is(err, store.ErrHostBusy), errors.Is(err, store.ErrStateChanged):
		writeError(w, http.StatusConflict, "host credential cannot be rotated while the host has active jobs")
		return
	case err != nil:
		s.logger.Error("rotate host credential", "error", err)
		writeError(w, http.StatusInternalServerError, "host credential could not be rotated")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"host_id": hostID, "host_token": hostToken})
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Hostname     string `json:"hostname"`
		OS           string `json:"os"`
		Arch         string `json:"arch"`
		AgentVersion string `json:"agent_version"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hostID := r.Header.Get("X-Fleet-Host-ID")
	if err := s.store.Heartbeat(r.Context(), hostID, request.Hostname, request.OS, request.Arch, request.AgentVersion, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "heartbeat could not be recorded")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentObservationTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListObservationTargets(r.Context(), r.Header.Get("X-Fleet-Host-ID"))
	if err != nil {
		s.logger.Error("list observation targets", "error", err)
		writeError(w, http.StatusInternalServerError, "observation targets could not be listed")
		return
	}
	if targets == nil {
		targets = []domain.ObservationTarget{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

func (s *Server) agentObservations(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Observations []domain.InstanceObservation `json:"observations"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	if err := validateObservations(request.Observations, now, s.config.ObservationStaleAfter); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateObservationSequence(r.Context(), r.Header.Get("X-Fleet-Host-ID"), request.Observations); err != nil {
		switch {
		case errors.Is(err, store.ErrObservationOwnership):
			writeError(w, http.StatusForbidden, "observation target does not belong to this host")
		case errors.Is(err, errObservationSequence):
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.logger.Error("validate observation sequence", "error", err)
			writeError(w, http.StatusInternalServerError, "observation sequence could not be validated")
		}
		return
	}
	err := s.store.RecordObservations(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), request.Observations, now,
	)
	if errors.Is(err, store.ErrObservationOwnership) {
		writeError(w, http.StatusForbidden, "observation target does not belong to this host")
		return
	}
	if err != nil {
		s.logger.Error("record observations", "error", err)
		writeError(w, http.StatusInternalServerError, "observations could not be recorded")
		return
	}
	for _, observation := range request.Observations {
		workflowID, workflowErr := identity.New()
		if workflowErr != nil {
			s.logger.Error("create runtime remediation workflow identity", "instance_id", observation.InstanceID, "error", workflowErr)
			continue
		}
		remediation, trackErr := s.store.TrackRuntimeHealthObservation(
			r.Context(), observation, time.Now().UTC(), workflowID,
		)
		if trackErr != nil {
			s.logger.Error("track runtime health drift", "instance_id", observation.InstanceID, "error", trackErr)
			continue
		}
		if remediation.Queue {
			queueErr := s.queueAutomaticRuntimeRepair(
				r.Context(), r.Header.Get("X-Fleet-Host-ID"), observation.InstanceID, remediation.State,
			)
			if queueErr != nil {
				if recordErr := s.store.RecordRuntimeRemediationQueueFailure(
					r.Context(), observation.InstanceID, "Automatic runtime repair could not be queued safely", time.Now().UTC(),
				); recordErr != nil {
					s.logger.Error("record automatic runtime repair queue failure", "instance_id", observation.InstanceID, "error", recordErr)
				}
				if !errors.Is(queueErr, store.ErrStateChanged) && !errors.Is(queueErr, store.ErrInstanceBusy) {
					s.logger.Error("queue automatic runtime repair", "instance_id", observation.InstanceID, "error", queueErr)
				}
			}
			continue
		}
		if observationCheckStatus(observation, "runtime") == domain.ObservationCheckDrift {
			// Runtime recovery has priority over configuration synchronization.
			// Running both workflows for the same observation would race lifecycle state.
			continue
		}
		refreshRequired, refreshErr := s.runtimeRefreshRequired(r.Context(), observation.InstanceID)
		if refreshErr != nil {
			s.logger.Error("check managed runtime refresh before synchronization", "instance_id", observation.InstanceID, "error", refreshErr)
			continue
		}
		if refreshRequired {
			// A version-preserving runtime refresh must run before the current
			// wrapper can safely accept Fleet configuration synchronization.
			continue
		}
		attemptedAt := time.Now().UTC()
		queue, trackErr := s.store.TrackRuntimeConfigurationObservation(r.Context(), observation, attemptedAt)
		if trackErr != nil {
			s.logger.Error("track runtime configuration drift", "instance_id", observation.InstanceID, "error", trackErr)
			continue
		}
		if queue {
			if queueErr := s.queueAutomaticRuntimeSync(
				r.Context(), r.Header.Get("X-Fleet-Host-ID"), observation.InstanceID,
			); queueErr != nil {
				if errors.Is(queueErr, errRuntimeRefreshRequired) {
					continue
				}
				if recordErr := s.store.RecordRuntimeConfigurationQueueFailure(
					r.Context(), observation.InstanceID, attemptedAt,
				); recordErr != nil {
					s.logger.Error("roll back automatic runtime synchronization attempt", "instance_id", observation.InstanceID, "error", recordErr)
				}
				if !errors.Is(queueErr, store.ErrStateChanged) && !errors.Is(queueErr, store.ErrInstanceBusy) {
					s.logger.Error("queue automatic runtime synchronization", "instance_id", observation.InstanceID, "error", queueErr)
				}
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) validateObservationSequence(
	ctx context.Context, hostID string, observations []domain.InstanceObservation,
) error {
	for _, observation := range observations {
		instance, err := s.store.GetInstance(ctx, observation.InstanceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: observation target no longer exists", errObservationSequence)
			}
			return err
		}
		if instance.HostID != hostID {
			return store.ErrObservationOwnership
		}
		if observation.TargetGeneration != instance.UpdatedAt.UTC().Format(time.RFC3339Nano) {
			return fmt.Errorf("%w: desired state changed before the report arrived", errObservationSequence)
		}
		if instance.Observation != nil &&
			instance.Observation.TargetGeneration == observation.TargetGeneration &&
			!observation.ObservedAt.After(instance.Observation.ObservedAt) {
			return fmt.Errorf("%w: a newer or equal report is already stored", errObservationSequence)
		}
	}
	return nil
}

func observationCheckStatus(observation domain.InstanceObservation, name string) string {
	for _, check := range observation.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}

func (s *Server) queueAutomaticRuntimeRepair(
	ctx context.Context,
	hostID, instanceID string,
	remediation domain.RuntimeRemediation,
) error {
	instance, err := s.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.HostID != hostID || instance.Status != domain.InstanceRunning {
		return store.ErrStateChanged
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		return store.ErrStateChanged
	}
	host, err := s.store.GetHost(ctx, hostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		return store.ErrStateChanged
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(domain.RuntimeRepairPayload{
		ActionPayload: domain.ActionPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, ImageID: instance.ImageID, Provider: instance.Provider, Model: instance.Model,
			Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
			APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, PreserveData: true,
		},
		Phase: remediation.Phase, Attempt: remediation.AttemptInPhase, Trigger: "automatic",
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, WorkflowID: remediation.WorkflowID, Actor: "SYSTEM",
		Type: "REPAIR_RUNTIME", Status: domain.OperationPending,
		Summary: "Automatically repair and verify " + instance.Name,
		Metadata: operationMetadata(map[string]any{
			"workflow_step":          "automatic-runtime-repair",
			"recovery_phase":         remediation.Phase,
			"recovery_attempt":       remediation.AttemptInPhase,
			"recovery_total_attempt": remediation.TotalAttempts,
			"recovery_max_attempts":  remediation.MaxAttempts,
		}),
		CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.runtime.repair", Status: domain.JobPending, Payload: payload,
		CreatedAt: now, UpdatedAt: now,
	}
	return s.store.QueueRuntimeRepair(ctx, domain.InstanceRunning, operation, job, remediation)
}

func (s *Server) queueAutomaticRuntimeSync(ctx context.Context, hostID, instanceID string) error {
	instance, err := s.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.HostID != hostID || (instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped) {
		return store.ErrStateChanged
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		return store.ErrStateChanged
	}
	if !instance.CodexConfigured || providers.ValidateRuntime(
		instance.Provider, instance.Model, instance.Reasoning, instance.ServiceTier,
	) != nil {
		return store.ErrStateChanged
	}
	refreshRequired, err := s.runtimeRefreshRequiredForInstance(ctx, instance)
	if err != nil {
		return err
	}
	if refreshRequired {
		return errRuntimeRefreshRequired
	}
	host, err := s.store.GetHost(ctx, hostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		return store.ErrStateChanged
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(domain.RuntimeSyncPayload{
		InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		DesiredStatus: instance.Status, DashboardPort: instance.DashboardPort,
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "SYNC_RUNTIME", Status: domain.OperationPending,
		Summary: "Automatically complete Hermes setup " + instance.Name, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operationID, HostID: hostID, InstanceID: instance.ID,
		Type: "instance.runtime.sync", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	return s.store.QueueAction(ctx, instance.Status, domain.InstanceUpdating, operation, job)
}

func (s *Server) claimJob(w http.ResponseWriter, r *http.Request) {
	hostID := r.Header.Get("X-Fleet-Host-ID")
	job, err := s.store.ClaimJob(r.Context(), hostID, jobLeaseDuration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job claim failed")
		return
	}
	if err := s.reconcileTerminalRecoveryReservations(r.Context(), hostID); err != nil {
		s.logger.Error("reconcile terminal recovery reservations", "host_id", hostID, "error", err)
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) reconcileTerminalRecoveryReservations(ctx context.Context, hostID string) error {
	if s.config.RecoveryPoints == nil {
		return nil
	}
	points, err := s.config.RecoveryPoints.List(ctx, "")
	if err != nil {
		return err
	}
	for _, point := range points {
		if point.HostID != hostID || (point.Status != recovery.StatusCreating && point.Status != recovery.StatusUploaded) {
			continue
		}
		operation, err := s.store.GetOperation(ctx, point.OperationID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.InstanceID != point.InstanceID || operation.Status != domain.OperationFailed {
			continue
		}
		if err := s.config.RecoveryPoints.AbortTerminal(point.ID, point.HostID, point.JobID); err != nil &&
			!errors.Is(err, recovery.ErrState) {
			return err
		}
	}
	return nil
}

func (s *Server) ackJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.AcknowledgeJob(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"),
		r.Header.Get(leaseTokenHeader), jobLeaseDuration,
	); err != nil {
		writeError(w, http.StatusConflict, "job is not available for acknowledgement")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renewJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RenewJob(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"),
		r.Header.Get(leaseTokenHeader), jobLeaseDuration,
	); err != nil {
		writeError(w, http.StatusConflict, "job lease is no longer active")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobProgress(w http.ResponseWriter, r *http.Request) {
	var progress domain.JobProgress
	if err := decodeJSON(r, &progress); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, jobType, err := s.store.JobMetadata(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), r.Header.Get(leaseTokenHeader),
	)
	if err != nil {
		writeError(w, http.StatusConflict, "job lease is no longer active")
		return
	}
	if err := validateJobProgress(jobType, progress, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateJobProgress(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"),
		r.Header.Get(leaseTokenHeader), progress,
	); err != nil {
		writeError(w, http.StatusConflict, "job lease is no longer active")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateJobProgress(jobType string, progress domain.JobProgress, now time.Time) error {
	if jobType == "instance.hermes.update" {
		switch progress.Stage {
		case "PREPARING_RELEASE", "STOPPING", "BACKING_UP", "INSTALLING", "RESTORING_STATE", "VERIFYING_VERSION":
			if progress.VerificationURI == "" && progress.UserCode == "" && progress.ExpiresAt.IsZero() {
				return nil
			}
		}
		return errors.New("unsupported Hermes update progress")
	}
	if jobType == "instance.mcp.configure" {
		switch progress.Stage {
		case "VALIDATING", "WRITING_CONFIGURATION", "RESTARTING_RUNTIME", "TESTING_CONNECTIONS", "VERIFYING_TOOLS":
			if progress.VerificationURI == "" && progress.UserCode == "" && progress.ExpiresAt.IsZero() {
				return nil
			}
		}
		return errors.New("unsupported MCP configuration progress")
	}
	if jobType != "instance.auth.codex" {
		return errors.New("this job does not support progress updates")
	}
	switch progress.Stage {
	case "STARTING", "VERIFYING":
		if progress.VerificationURI != "" || progress.UserCode != "" || !progress.ExpiresAt.IsZero() {
			return errors.New("this progress stage cannot contain an authentication code")
		}
	case "AWAITING_USER":
		if progress.VerificationURI != codexDeviceURL {
			return errors.New("unsupported Codex verification URL")
		}
		if !codexUserCodePattern.MatchString(progress.UserCode) {
			return errors.New("invalid Codex user code")
		}
		if !progress.ExpiresAt.After(now) || progress.ExpiresAt.After(now.Add(16*time.Minute)) {
			return errors.New("invalid Codex authentication expiry")
		}
	default:
		return errors.New("unsupported job progress stage")
	}
	return nil
}

func (s *Server) completeJob(w http.ResponseWriter, r *http.Request) {
	leaseToken := r.Header.Get(leaseTokenHeader)
	var result domain.JobResult
	if err := decodeJSON(r, &result); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	completionHash, err := store.JobResultCompletionHash(result)
	if err != nil {
		writeError(w, http.StatusBadRequest, "job result could not be encoded")
		return
	}
	hostID, jobID := r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID")
	operationID, jobType, jobStatus, metadataErr := s.store.CompletionJobMetadata(
		r.Context(), hostID, jobID, leaseToken,
	)
	if metadataErr != nil {
		if completionErrorStatus(metadataErr) == http.StatusConflict {
			writeError(w, http.StatusConflict, "job lease is no longer active")
			return
		}
		s.logger.Error("load job completion metadata", "job_id", jobID, "host_id", hostID, "error", metadataErr)
		writeError(w, http.StatusInternalServerError, "job completion metadata could not be loaded")
		return
	}
	if jobStatus == domain.JobSucceeded || jobStatus == domain.JobFailed {
		// The first request committed but its HTTP response may have been lost.
		// Compare the canonical result hash before acknowledging it, and skip
		// credential sealing or recovery finalization on this duplicate path.
		if err := s.store.CompleteJobWithHash(
			r.Context(), hostID, jobID, leaseToken, completionHash, result, nil,
		); err != nil {
			if completionErrorStatus(err) == http.StatusConflict {
				writeError(w, http.StatusConflict, "job result does not match the recorded completion")
				return
			}
			s.logger.Error("verify duplicate job completion", "job_id", jobID, "host_id", hostID, "error", err)
			writeError(w, http.StatusInternalServerError, "job completion retry could not be verified")
			return
		}
		if jobType == "instance.delete" && result.Success {
			go s.reconcilePendingInstanceDeletions(context.Background())
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if jobType == "instance.chat.send" && result.Success {
		if result.ChatMessage == "" || len(result.ChatMessage) > maximumJSONBodyBytes {
			writeError(w, http.StatusBadRequest, "chat result is missing or too large")
			return
		}
		payloadData, payloadErr := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, jobType)
		var payload domain.ChatSendPayload
		if payloadErr != nil || json.Unmarshal(payloadData, &payload) != nil || payload.SessionID == "" {
			writeError(w, http.StatusConflict, "chat result no longer matches the active job")
			return
		}
		result.ChatCiphertext, err = s.config.Sealer.Seal([]byte(result.ChatMessage), store.ChatMessageSealContext(payload.SessionID))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat result could not be encrypted")
			return
		}
	}
	var reveal *store.EncryptedReveal
	if result.Credentials != nil {
		if jobType != "instance.credentials.inspect" || !result.Success {
			writeError(w, http.StatusBadRequest, "credential result does not match an active inspection job")
			return
		}
		plaintext, err := json.Marshal(result.Credentials)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "credential result could not be encoded")
			return
		}
		ciphertext, err := s.config.Sealer.Seal(plaintext, operationID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "credential result could not be encrypted")
			return
		}
		reveal = &store.EncryptedReveal{Ciphertext: ciphertext, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	}
	recoveryResult := jobType == "instance.recovery.create" || jobType == "instance.hermes.update"
	if recoveryResult {
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Hour))
		if s.config.RecoveryPoints == nil || result.RecoveryPointID == "" {
			writeError(w, http.StatusBadRequest, "instance backup result is incomplete")
			return
		}
		var recoveryErr error
		existingPoint, existingErr := s.config.RecoveryPoints.Get(result.RecoveryPointID)
		alreadyReady := existingErr == nil && existingPoint.Status == recovery.StatusReady &&
			existingPoint.HostID == hostID && existingPoint.JobID == jobID
		alreadyFailed := existingErr == nil && existingPoint.Status == recovery.StatusFailed &&
			existingPoint.HostID == hostID && existingPoint.JobID == jobID
		if alreadyReady {
			if jobType == "instance.recovery.create" {
				result.Success = true
				result.Error = ""
			}
			result.RecoverySHA256 = existingPoint.SHA256
			result.RecoverySizeBytes = existingPoint.SizeBytes
		} else if alreadyFailed {
			result.Success = false
			result.Error = existingPoint.Error
		}
		if result.Success && !alreadyReady {
			_, recoveryErr = s.config.RecoveryPoints.VerifyUploaded(
				r.Context(), result.RecoveryPointID, hostID, jobID, result.RecoverySHA256, result.RecoverySizeBytes,
			)
			if errors.Is(recoveryErr, recovery.ErrIntegrity) {
				result.Success = false
				result.Error = "control plane rejected the instance backup integrity or archive manifest"
				recoveryErr = s.config.RecoveryPoints.Fail(result.RecoveryPointID, hostID, jobID, result.Error)
			}
		} else if !alreadyReady && !alreadyFailed {
			recoveryErr = s.config.RecoveryPoints.Fail(result.RecoveryPointID, hostID, jobID, result.Error)
		}
		if recoveryErr != nil {
			if resetErr := s.config.RecoveryPoints.ResetForRetry(result.RecoveryPointID, hostID, jobID); resetErr != nil {
				s.logger.Error("reset recovery point after finalization failure", "error", resetErr)
			}
			s.logger.Error("finalize recovery point", "recovery_point_id", result.RecoveryPointID, "error", recoveryErr)
			if completionErrorStatus(recoveryErr) == http.StatusConflict {
				writeError(w, http.StatusConflict, "instance backup artifact conflicts with the active job")
				return
			}
			writeError(w, http.StatusInternalServerError, "instance backup artifact could not be finalized")
			return
		}
	} else if result.RecoveryPointID != "" || result.RecoverySHA256 != "" || result.RecoverySizeBytes != 0 {
		writeError(w, http.StatusBadRequest, "instance backup result does not match the active job")
		return
	}
	if err := s.store.CompleteJobWithHash(
		r.Context(), hostID, jobID, leaseToken, completionHash, result, reveal,
	); err != nil {
		if recoveryResult {
			if resetErr := s.config.RecoveryPoints.ResetForRetry(
				result.RecoveryPointID, r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"),
			); resetErr != nil {
				s.logger.Error("reset recovery point after job completion failure", "error", resetErr)
			}
		}
		if completionErrorStatus(err) == http.StatusConflict {
			writeError(w, http.StatusConflict, "job lease or state no longer accepts this result")
			return
		}
		s.logger.Error("record job completion", "job_id", jobID, "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "job result could not be recorded")
		return
	}
	if s.config.RemoteAccess != nil {
		s.config.RemoteAccess.Trigger()
	}
	if jobType == "instance.delete" && result.Success {
		go s.reconcilePendingInstanceDeletions(context.Background())
	}
	w.WriteHeader(http.StatusNoContent)
}

func completionErrorStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrLeaseLost),
		errors.Is(err, store.ErrStateChanged),
		errors.Is(err, store.ErrInvalidJobResult),
		errors.Is(err, recovery.ErrNotFound),
		errors.Is(err, recovery.ErrState):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) verifyRecoveryPointForUpdate(w http.ResponseWriter, r *http.Request) {
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	hostID, jobID := r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID")
	leaseToken := r.Header.Get(leaseTokenHeader)
	_, jobType, err := s.store.JobMetadata(r.Context(), hostID, jobID, leaseToken)
	if err != nil || jobType != "instance.hermes.update" {
		writeError(w, http.StatusConflict, "instance backup verification does not match an active Hermes update")
		return
	}
	payloadData, err := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, jobType)
	if err != nil {
		writeError(w, http.StatusConflict, "Hermes update lease is no longer active")
		return
	}
	var update domain.HermesUpdatePayload
	if err := json.Unmarshal(payloadData, &update); err != nil {
		writeError(w, http.StatusConflict, "Hermes update backup payload is invalid")
		return
	}
	var request struct {
		RecoveryPointID string `json:"recovery_point_id"`
		SHA256          string `json:"sha256"`
		SizeBytes       int64  `json:"size_bytes"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.RecoveryPointID != update.Backup.RecoveryPointID {
		writeError(w, http.StatusConflict, "instance backup does not belong to this Hermes update")
		return
	}
	point, err := s.config.RecoveryPoints.Get(request.RecoveryPointID)
	if err == nil && point.Status == recovery.StatusReady && point.HostID == hostID && point.JobID == jobID &&
		point.SHA256 == request.SHA256 && point.SizeBytes == request.SizeBytes {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.config.RecoveryPoints.VerifyUploaded(
		r.Context(), request.RecoveryPointID, hostID, jobID, request.SHA256, request.SizeBytes,
	); err != nil {
		if errors.Is(err, recovery.ErrIntegrity) {
			_ = s.config.RecoveryPoints.Fail(request.RecoveryPointID, hostID, jobID, "control plane rejected the instance backup integrity or archive manifest")
		}
		s.logger.Error("verify automatic Hermes update backup", "recovery_point_id", request.RecoveryPointID, "error", err)
		writeError(w, http.StatusConflict, "instance backup could not be verified")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) uploadRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	leaseToken := r.Header.Get(leaseTokenHeader)
	_, jobType, err := s.store.JobMetadata(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), leaseToken,
	)
	if err != nil || (jobType != "instance.recovery.create" && jobType != "instance.hermes.update") {
		writeError(w, http.StatusConflict, "instance backup upload does not match an active job")
		return
	}
	if r.ContentLength < 1 || r.ContentLength > s.config.MaxRecoveryPointBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "instance backup exceeds the configured size limit")
		return
	}
	pointID := r.Header.Get("X-Fleet-Recovery-Point-ID")
	digest := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Fleet-Recovery-SHA256")))
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Hour))
	body := http.MaxBytesReader(w, r.Body, s.config.MaxRecoveryPointBytes)
	defer body.Close()
	if _, err := s.config.RecoveryPoints.Upload(
		r.Context(), pointID, r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), digest, r.ContentLength, body,
		func(ctx context.Context) error {
			_, currentJobType, err := s.store.JobMetadata(ctx, r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), leaseToken)
			if err != nil {
				return err
			}
			if currentJobType != "instance.recovery.create" && currentJobType != "instance.hermes.update" {
				return store.ErrLeaseLost
			}
			return nil
		},
	); err != nil {
		s.logger.Error("store recovery point upload", "recovery_point_id", pointID, "error", err)
		writeError(w, http.StatusConflict, "instance backup upload was rejected")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) downloadRecoveryPointForRestore(w http.ResponseWriter, r *http.Request) {
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	hostID := r.Header.Get("X-Fleet-Host-ID")
	_, jobType, err := s.store.JobMetadata(
		r.Context(), hostID, r.PathValue("jobID"), r.Header.Get(leaseTokenHeader),
	)
	if err != nil || (jobType != "instance.recovery.restore" && jobType != "instance.hermes.upgrade" &&
		jobType != "instance.hermes.update") {
		writeError(w, http.StatusConflict, "instance backup download lease is no longer active")
		return
	}
	payloadData, err := s.store.ActiveJobPayload(
		r.Context(), hostID, r.PathValue("jobID"), r.Header.Get(leaseTokenHeader), jobType,
	)
	if err != nil {
		writeError(w, http.StatusConflict, "instance backup download lease is no longer active")
		return
	}
	var (
		payload       domain.RecoveryRestorePayload
		updatePayload *domain.RecoveryPointPayload
	)
	if jobType == "instance.hermes.update" {
		var update domain.HermesUpdatePayload
		if err := json.Unmarshal(payloadData, &update); err != nil {
			writeError(w, http.StatusConflict, "Hermes update backup payload is invalid")
			return
		}
		updatePayload = &update.Backup
	} else if jobType == "instance.hermes.upgrade" {
		var upgrade domain.HermesUpgradePayload
		if err := json.Unmarshal(payloadData, &upgrade); err != nil || !upgrade.Rollback.RequireImageID {
			writeError(w, http.StatusConflict, "Hermes update rollback payload is invalid")
			return
		}
		payload = upgrade.Rollback
	} else {
		if err := json.Unmarshal(payloadData, &payload); err != nil {
			writeError(w, http.StatusConflict, "instance backup restore payload is invalid")
			return
		}
	}
	recoveryPointID := payload.RecoveryPointID
	if updatePayload != nil {
		recoveryPointID = updatePayload.RecoveryPointID
	}
	metadata, err := s.config.RecoveryPoints.Get(recoveryPointID)
	if err != nil || metadata.HostID != hostID {
		writeError(w, http.StatusConflict, "instance backup does not match the active restore job")
		return
	}
	if updatePayload != nil {
		if metadata.JobID != r.PathValue("jobID") {
			writeError(w, http.StatusConflict, "instance backup does not belong to this Hermes update")
			return
		}
		if !recoveryPointPayloadMatchesMetadata(*updatePayload, metadata) {
			writeError(w, http.StatusConflict, "instance backup does not match the active Hermes update")
			return
		}
		if metadata.Status == recovery.StatusCreating {
			writeError(w, http.StatusNotFound, "verified update backup is not available yet")
			return
		}
		if metadata.Status == recovery.StatusUploaded {
			metadata, err = s.config.RecoveryPoints.VerifyUploaded(
				r.Context(), metadata.ID, hostID, r.PathValue("jobID"), metadata.SHA256, metadata.SizeBytes,
			)
		}
	} else if !restorePayloadMatchesMetadata(payload, metadata) || metadata.Status != recovery.StatusReady {
		writeError(w, http.StatusConflict, "instance backup does not match the active restore job")
		return
	}
	if err != nil {
		s.writeRecoveryError(w, "prepare verified backup download", err)
		return
	}
	if metadata.Status != recovery.StatusReady {
		writeError(w, http.StatusConflict, "verified instance backup is not available")
		return
	}
	if metadata, err = s.config.RecoveryPoints.Verify(r.Context(), metadata.ID); err != nil {
		s.writeRecoveryError(w, "verify before restore", err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	w.Header().Set("X-Fleet-Recovery-SHA256", metadata.SHA256)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Hour))
	if _, err := s.config.RecoveryPoints.Stream(r.Context(), metadata.ID, w); err != nil {
		s.logger.Error("stream recovery point to Host Agent", "recovery_point_id", metadata.ID, "error", err)
	}
}

func restorePayloadMatchesMetadata(payload domain.RecoveryRestorePayload, metadata recovery.Metadata) bool {
	return payload.RecoveryPointID == metadata.ID && payload.InstanceID == metadata.InstanceID && payload.Name == metadata.InstanceName &&
		payload.Image == metadata.Image && payload.ImageID == metadata.ImageID && payload.Provider == metadata.Provider &&
		payload.Model == metadata.Model && payload.Reasoning == metadata.Reasoning && payload.ServiceTier == metadata.ServiceTier &&
		payload.CodexConfigured == metadata.CodexConfigured &&
		payload.ProjectName == metadata.ProjectName && payload.DataVolume == metadata.DataVolume && payload.ManagedPath == metadata.ManagedPath &&
		payload.AgentVersion == metadata.AgentVersion && payload.CreatedAt.Equal(metadata.CreatedAt) &&
		payload.RecoverySHA256 == metadata.SHA256 && payload.RecoverySizeBytes == metadata.SizeBytes
}

func recoveryPointPayloadMatchesMetadata(payload domain.RecoveryPointPayload, metadata recovery.Metadata) bool {
	return payload.RecoveryPointID == metadata.ID && payload.InstanceID == metadata.InstanceID && payload.Name == metadata.InstanceName &&
		payload.Image == metadata.Image && payload.ImageID == metadata.ImageID && payload.Provider == metadata.Provider &&
		payload.Model == metadata.Model && payload.Reasoning == metadata.Reasoning && payload.ServiceTier == metadata.ServiceTier &&
		payload.CodexConfigured == metadata.CodexConfigured &&
		payload.ProjectName == metadata.ProjectName && payload.DataVolume == metadata.DataVolume && payload.ManagedPath == metadata.ManagedPath &&
		payload.AgentVersion == metadata.AgentVersion && payload.CreatedAt.Equal(metadata.CreatedAt)
}

func (s *Server) startCodexAuth(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if instance.Provider != "openai-codex" {
		writeError(w, http.StatusConflict, "Codex authentication is only available for an openai-codex configuration")
		return
	}
	if instance.Status != domain.InstanceRunning {
		writeError(w, http.StatusConflict, "start the instance before authenticating Codex")
		return
	}
	if instance.ProjectName == "" || instance.ManagedPath == "" || instance.ImageID == "" {
		writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
		return
	}
	host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
	if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "Codex authentication requires Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create Codex authentication identity")
		return
	}
	payload, _ := json.Marshal(domain.CodexAuthPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName, ManagedPath: instance.ManagedPath,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CODEX_AUTH", Status: domain.OperationPending,
		Summary: "Authenticate Codex " + instance.Name, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.auth.codex", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueCodexAuth(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrStateChanged) || errors.Is(err, store.ErrInstanceBusy) {
			active, activeErr := s.store.GetActiveCodexAuthSession(r.Context(), instance.ID)
			if activeErr == nil {
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, http.StatusAccepted, active)
				return
			}
		}
		writeError(w, http.StatusConflict, "Codex authentication could not be queued")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, domain.CodexAuthSession{
		OperationID: operation.ID, InstanceID: instance.ID, Status: operation.Status,
		CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Server) getCodexAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	session, err := s.store.GetCodexAuthSession(r.Context(), r.PathValue("instanceID"), r.PathValue("operationID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Codex authentication session not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) cancelCodexAuth(w http.ResponseWriter, r *http.Request) {
	err := s.store.CancelCodexAuth(
		r.Context(),
		r.PathValue("instanceID"),
		r.PathValue("operationID"),
		"Codex authentication canceled by administrator",
	)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Codex authentication session not found")
		return
	}
	if errors.Is(err, store.ErrStateChanged) {
		writeError(w, http.StatusConflict, "Codex authentication is no longer active")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Codex authentication could not be canceled")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type codexConfigurationRequest struct {
	Model       string `json:"model"`
	Reasoning   string `json:"reasoning"`
	ServiceTier string `json:"service_tier"`
}

func (s *Server) configureCodex(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if instance.Provider != "openai-codex" {
		writeError(w, http.StatusConflict, "Codex configuration is only available for an openai-codex instance")
		return
	}
	if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
		writeError(w, http.StatusConflict, "wait for the current instance operation to finish")
		return
	}
	var request codexConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Reasoning = strings.TrimSpace(request.Reasoning)
	request.ServiceTier = strings.TrimSpace(request.ServiceTier)
	if err := providers.ValidateRuntime(instance.Provider, request.Model, request.Reasoning, request.ServiceTier); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	models, _, catalogErr := s.store.GetInstanceModelCatalog(r.Context(), instance.ID)
	if catalogErr != nil && !errors.Is(catalogErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "could not read the Hermes model catalog")
		return
	}
	if len(models) == 0 {
		writeError(w, http.StatusConflict, "refresh diagnostics before choosing a Codex model")
		return
	}
	modelSupported := false
	for _, model := range models {
		if model == request.Model {
			modelSupported = true
			break
		}
	}
	if !modelSupported {
		writeError(w, http.StatusBadRequest, "the selected model is not supported by this Hermes version")
		return
	}
	connected, observationErr := s.store.HasFreshObservationCheck(
		r.Context(), instance.ID, "codex_auth", domain.ObservationCheckOK,
		time.Now().UTC().Add(-s.config.ObservationStaleAfter),
	)
	if observationErr != nil {
		writeError(w, http.StatusInternalServerError, "could not verify Codex authentication")
		return
	}
	if !connected {
		writeError(w, http.StatusConflict, "authenticate Codex before choosing its model")
		return
	}
	refreshRequired, refreshErr := s.runtimeRefreshRequiredForInstance(r.Context(), instance)
	if refreshErr != nil {
		writeError(w, http.StatusInternalServerError, "could not verify managed runtime compatibility")
		return
	}
	if refreshRequired {
		writeError(w, http.StatusConflict, "refresh the managed runtime before applying Codex configuration")
		return
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
		return
	}
	host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
	if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "the instance requires an online Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create Codex configuration identity")
		return
	}
	payload, _ := json.Marshal(domain.RuntimeSyncPayload{
		InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: request.Model, Reasoning: request.Reasoning, ServiceTier: request.ServiceTier,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		DesiredStatus: instance.Status, DashboardPort: instance.DashboardPort,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CONFIGURE_CODEX", Status: domain.OperationPending,
		Summary: "Configure Codex " + instance.Name, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.runtime.configure", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueAction(r.Context(), instance.Status, domain.InstanceUpdating, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		writeError(w, http.StatusConflict, "Codex configuration could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

type messagingConfigurationRequest struct {
	Telegram struct {
		Enabled           bool     `json:"enabled"`
		BotToken          string   `json:"bot_token"`
		ClearBotToken     bool     `json:"clear_bot_token"`
		AllowedUsers      []string `json:"allowed_users"`
		GroupAllowedUsers []string `json:"group_allowed_users"`
		GroupAllowedChats []string `json:"group_allowed_chats"`
		RequireMention    bool     `json:"require_mention"`
		ProxyURL          string   `json:"proxy_url"`
	} `json:"telegram"`
	WhatsApp domain.WhatsAppMessagingConfiguration `json:"whatsapp"`
}

type messagingConfigurationView struct {
	Status          string     `json:"status"`
	LastError       string     `json:"last_error,omitempty"`
	DesiredRevision string     `json:"desired_revision,omitempty"`
	AppliedRevision string     `json:"applied_revision,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	AppliedAt       *time.Time `json:"applied_at,omitempty"`
	Telegram        struct {
		Enabled           bool     `json:"enabled"`
		TokenConfigured   bool     `json:"token_configured"`
		TokenHint         string   `json:"token_hint"`
		AllowedUsers      []string `json:"allowed_users"`
		GroupAllowedUsers []string `json:"group_allowed_users"`
		GroupAllowedChats []string `json:"group_allowed_chats"`
		RequireMention    bool     `json:"require_mention"`
		ProxyURL          string   `json:"proxy_url"`
	} `json:"telegram"`
	WhatsApp struct {
		Enabled                bool     `json:"enabled"`
		Mode                   string   `json:"mode"`
		AllowedUsers           []string `json:"allowed_users"`
		UnauthorizedDMBehavior string   `json:"unauthorized_dm_behavior"`
		ReplyPrefix            string   `json:"reply_prefix"`
	} `json:"whatsapp"`
}

func defaultMessagingConfiguration() domain.MessagingConfiguration {
	return domain.MessagingConfiguration{
		Telegram: domain.TelegramMessagingConfiguration{RequireMention: true},
		WhatsApp: domain.WhatsAppMessagingConfiguration{
			Mode: "bot", UnauthorizedDMBehavior: "ignore", ReplyPrefix: "⚕ **Hermes Agent**",
		},
	}
}

func messagingView(config domain.MessagingConfiguration, record *store.MessagingConfigRecord) messagingConfigurationView {
	view := messagingConfigurationView{Status: "NOT_CONFIGURED"}
	view.Telegram.Enabled = config.Telegram.Enabled
	view.Telegram.TokenConfigured = config.Telegram.BotToken != ""
	view.Telegram.TokenHint = telegramTokenHint(config.Telegram.BotToken)
	view.Telegram.AllowedUsers = messagingList(config.Telegram.AllowedUsers)
	view.Telegram.GroupAllowedUsers = messagingList(config.Telegram.GroupAllowedUsers)
	view.Telegram.GroupAllowedChats = messagingList(config.Telegram.GroupAllowedChats)
	view.Telegram.RequireMention = config.Telegram.RequireMention
	view.Telegram.ProxyURL = config.Telegram.ProxyURL
	view.WhatsApp.Enabled = config.WhatsApp.Enabled
	view.WhatsApp.Mode = config.WhatsApp.Mode
	view.WhatsApp.AllowedUsers = messagingList(config.WhatsApp.AllowedUsers)
	view.WhatsApp.UnauthorizedDMBehavior = config.WhatsApp.UnauthorizedDMBehavior
	view.WhatsApp.ReplyPrefix = config.WhatsApp.ReplyPrefix
	if record != nil {
		view.Status = record.Status
		view.LastError = record.LastError
		view.DesiredRevision = record.DesiredRevision
		view.AppliedRevision = record.AppliedRevision
		view.UpdatedAt = &record.UpdatedAt
		view.AppliedAt = record.AppliedAt
	}
	return view
}

func telegramTokenHint(token string) string {
	botID, _, found := strings.Cut(strings.TrimSpace(token), ":")
	if !found || botID == "" || strings.IndexFunc(botID, func(value rune) bool {
		return value < '0' || value > '9'
	}) >= 0 {
		if strings.TrimSpace(token) == "" {
			return ""
		}
		return "••••••••"
	}
	return botID + ":••••••••"
}

func messagingList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func (s *Server) openMessagingConfiguration(record store.MessagingConfigRecord) (domain.MessagingConfiguration, error) {
	if s.config.Sealer == nil {
		return domain.MessagingConfiguration{}, errors.New("messaging encryption is unavailable")
	}
	plaintext, err := s.config.Sealer.Open(record.Ciphertext, "instance-messaging:"+record.InstanceID)
	if err != nil {
		return domain.MessagingConfiguration{}, err
	}
	defer clearBytes(plaintext)
	var config domain.MessagingConfiguration
	if err := json.Unmarshal(plaintext, &config); err != nil {
		return domain.MessagingConfiguration{}, err
	}
	return messaging.NormalizeAndValidate(config)
}

func (s *Server) getMessagingConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	instanceID := r.PathValue("instanceID")
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	record, err := s.store.GetMessagingConfig(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, messagingView(defaultMessagingConfiguration(), nil))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be read")
		return
	}
	config, err := s.openMessagingConfiguration(record)
	if err != nil {
		s.logger.Error("open instance messaging configuration", "instance_id", instanceID, "error", err)
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be decrypted")
		return
	}
	writeJSON(w, http.StatusOK, messagingView(config, &record))
}

func (s *Server) configureMessaging(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
		writeError(w, http.StatusConflict, "wait for the current instance operation to finish")
		return
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
		return
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "messaging configuration requires an online Host Agent "+agentVersion)
		return
	}
	if s.config.Sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "messaging encryption is unavailable")
		return
	}
	var request messagingConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	config := domain.MessagingConfiguration{
		Telegram: domain.TelegramMessagingConfiguration{
			Enabled: request.Telegram.Enabled, BotToken: request.Telegram.BotToken,
			AllowedUsers: request.Telegram.AllowedUsers, GroupAllowedUsers: request.Telegram.GroupAllowedUsers,
			GroupAllowedChats: request.Telegram.GroupAllowedChats, RequireMention: request.Telegram.RequireMention,
			ProxyURL: request.Telegram.ProxyURL,
		},
		WhatsApp: request.WhatsApp,
	}
	if strings.TrimSpace(config.Telegram.BotToken) == "" && !request.Telegram.ClearBotToken {
		if existing, existingErr := s.store.GetMessagingConfig(r.Context(), instance.ID); existingErr == nil {
			previous, openErr := s.openMessagingConfiguration(existing)
			if openErr != nil {
				writeError(w, http.StatusInternalServerError, "existing Telegram token could not be retained")
				return
			}
			config.Telegram.BotToken = previous.Telegram.BotToken
		} else if !errors.Is(existingErr, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "existing messaging configuration could not be read")
			return
		}
	}
	config, err = messaging.NormalizeAndValidate(config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext, err := json.Marshal(config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be encoded")
		return
	}
	defer clearBytes(plaintext)
	digest := sha256.Sum256(plaintext)
	revision := hex.EncodeToString(digest[:])
	ciphertext, err := s.config.Sealer.Seal(plaintext, "instance-messaging:"+instance.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be encrypted")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "messaging configuration identity could not be created")
		return
	}
	payload, _ := json.Marshal(domain.MessagingApplyPayload{
		InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		DesiredStatus: instance.Status, APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, Revision: revision,
	})
	metadata, _ := json.Marshal(map[string]any{
		"revision": revision, "telegram_enabled": config.Telegram.Enabled, "whatsapp_enabled": config.WhatsApp.Enabled,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CONFIGURE_MESSAGING", Status: domain.OperationPending,
		Summary: "Apply messaging configuration " + instance.Name, Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.messaging.configure", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	record := store.MessagingConfigRecord{
		InstanceID: instance.ID, Ciphertext: ciphertext, DesiredRevision: revision, Status: "PENDING", UpdatedAt: now,
	}
	if err := s.store.QueueMessagingConfiguration(r.Context(), instance.Status, record, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "messaging configuration could not be queued while the instance is busy")
			return
		}
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) downloadMessagingConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	hostID, jobID, leaseToken := r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), r.Header.Get(leaseTokenHeader)
	payloadData, err := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, "instance.messaging.configure")
	if err != nil {
		writeError(w, http.StatusConflict, "messaging configuration lease is no longer active")
		return
	}
	var payload domain.MessagingApplyPayload
	if err := json.Unmarshal(payloadData, &payload); err != nil || payload.InstanceID == "" || payload.Revision == "" {
		writeError(w, http.StatusConflict, "messaging configuration job payload is invalid")
		return
	}
	record, err := s.store.GetMessagingConfig(r.Context(), payload.InstanceID)
	if err != nil || record.DesiredRevision != payload.Revision {
		writeError(w, http.StatusConflict, "messaging configuration revision is no longer current")
		return
	}
	config, err := s.openMessagingConfiguration(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "messaging configuration could not be decrypted")
		return
	}
	writeJSON(w, http.StatusOK, config)
}

type mcpConfigurationRequest struct {
	Servers []struct {
		Name             string   `json:"name"`
		Source           string   `json:"source"`
		URL              string   `json:"url"`
		AuthType         string   `json:"auth_type"`
		BearerToken      string   `json:"bearer_token"`
		ClearBearerToken bool     `json:"clear_bearer_token"`
		Enabled          bool     `json:"enabled"`
		Tools            []string `json:"tools"`
	} `json:"servers"`
}

type mcpDiscoveryRequest struct {
	OriginalName string `json:"original_name"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	AuthType     string `json:"auth_type"`
	BearerToken  string `json:"bearer_token"`
}

type mcpDiscoveryView struct {
	Tools []mcpdiscovery.Tool `json:"tools"`
}

type mcpDiscoveryErrorView struct {
	Error     string `json:"error"`
	Stage     string `json:"stage"`
	Action    string `json:"action"`
	Retryable bool   `json:"retryable"`
}

type mcpServerView struct {
	Name            string   `json:"name"`
	Source          string   `json:"source"`
	URL             string   `json:"url"`
	AuthType        string   `json:"auth_type"`
	TokenConfigured bool     `json:"token_configured"`
	TokenHint       string   `json:"token_hint"`
	Enabled         bool     `json:"enabled"`
	Tools           []string `json:"tools"`
}

type mcpConfigurationView struct {
	Status          string          `json:"status"`
	LastError       string          `json:"last_error,omitempty"`
	DesiredRevision string          `json:"desired_revision,omitempty"`
	AppliedRevision string          `json:"applied_revision,omitempty"`
	UpdatedAt       *time.Time      `json:"updated_at,omitempty"`
	AppliedAt       *time.Time      `json:"applied_at,omitempty"`
	Servers         []mcpServerView `json:"servers"`
}

func mcpView(config domain.MCPConfiguration, record *store.MCPConfigRecord) mcpConfigurationView {
	view := mcpConfigurationView{Status: "NOT_CONFIGURED", Servers: make([]mcpServerView, 0, len(config.Servers))}
	for _, server := range config.Servers {
		hint := ""
		if server.BearerToken != "" {
			hint = "••••••••"
		}
		view.Servers = append(view.Servers, mcpServerView{
			Name: server.Name, Source: server.Source, URL: server.URL, AuthType: server.AuthType,
			TokenConfigured: server.BearerToken != "", TokenHint: hint, Enabled: server.Enabled,
			Tools: append([]string(nil), server.Tools...),
		})
	}
	if record != nil {
		view.Status = record.Status
		view.LastError = record.LastError
		view.DesiredRevision = record.DesiredRevision
		view.AppliedRevision = record.AppliedRevision
		view.UpdatedAt = &record.UpdatedAt
		view.AppliedAt = record.AppliedAt
	}
	return view
}

func (s *Server) openMCPConfiguration(record store.MCPConfigRecord) (domain.MCPConfiguration, error) {
	if s.config.Sealer == nil {
		return domain.MCPConfiguration{}, errors.New("MCP encryption is unavailable")
	}
	plaintext, err := s.config.Sealer.Open(record.Ciphertext, "instance-mcp:"+record.InstanceID)
	if err != nil {
		return domain.MCPConfiguration{}, err
	}
	defer clearBytes(plaintext)
	var config domain.MCPConfiguration
	if err := json.Unmarshal(plaintext, &config); err != nil {
		return domain.MCPConfiguration{}, err
	}
	return mcpconfig.NormalizeAndValidate(config)
}

func (s *Server) getMCPConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	instanceID := r.PathValue("instanceID")
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	record, err := s.store.GetMCPConfig(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, mcpView(domain.MCPConfiguration{Servers: []domain.MCPServerConfiguration{}}, nil))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be read")
		return
	}
	config, err := s.openMCPConfiguration(record)
	if err != nil {
		s.logger.Error("open instance MCP configuration", "instance_id", instanceID, "error", err)
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be decrypted")
		return
	}
	writeJSON(w, http.StatusOK, mcpView(config, &record))
}

func (s *Server) discoverMCPTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	instanceID := r.PathValue("instanceID")
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if s.mcpDiscover == nil {
		writeError(w, http.StatusServiceUnavailable, "MCP discovery is unavailable")
		return
	}

	var request mcpDiscoveryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.OriginalName = strings.ToLower(strings.TrimSpace(request.OriginalName))
	request.Name = strings.ToLower(strings.TrimSpace(request.Name))
	request.URL = strings.TrimSpace(request.URL)
	request.AuthType = strings.ToLower(strings.TrimSpace(request.AuthType))
	request.BearerToken = strings.TrimSpace(request.BearerToken)

	if request.BearerToken == "" && request.AuthType == "bearer" && request.OriginalName != "" {
		record, err := s.store.GetMCPConfig(r.Context(), instanceID)
		if err == nil {
			previous, openErr := s.openMCPConfiguration(record)
			if openErr != nil {
				s.logger.Error("open MCP configuration for discovery", "instance_id", instanceID, "error", openErr)
				writeError(w, http.StatusInternalServerError, "stored MCP authentication could not be read")
				return
			}
			for _, server := range previous.Servers {
				if server.Name == request.OriginalName && server.URL == request.URL && server.AuthType == "bearer" {
					request.BearerToken = server.BearerToken
					break
				}
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "stored MCP configuration could not be read")
			return
		}
	}

	candidate, err := mcpconfig.NormalizeAndValidate(domain.MCPConfiguration{Servers: []domain.MCPServerConfiguration{{
		Name: request.Name, Source: "remote", URL: request.URL, AuthType: request.AuthType,
		BearerToken: request.BearerToken, Enabled: true, Tools: []string{"discovery"},
	}}})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	server := candidate.Servers[0]
	discoveryContext, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	tools, err := s.mcpDiscover(discoveryContext, mcpdiscovery.Request{URL: server.URL, BearerToken: server.BearerToken})
	if err != nil {
		s.logger.Warn("discover MCP tools", "instance_id", instanceID, "server", server.Name, "error", err)
		writeMCPDiscoveryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpDiscoveryView{Tools: tools})
}

func writeMCPDiscoveryError(w http.ResponseWriter, err error) {
	stage := "Connect to MCP server"
	var stageErr *mcpdiscovery.StageError
	if errors.As(err, &stageErr) {
		stage = map[string]string{
			"initialize":               "Initialize MCP session",
			"initialized notification": "Confirm MCP session",
			"tools/list":               "Load available tools",
		}[stageErr.Stage]
		if stage == "" {
			stage = "Connect to MCP server"
		}
	}

	message := "Fleet could not complete MCP discovery."
	action := "Check the remote MCP server logs and retry."
	var statusErr *mcpdiscovery.HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			message = "The remote MCP server rejected authentication."
			action = "Replace the bearer token with an active MCP token, then retry."
		case http.StatusNotFound:
			message = "The remote MCP server returned HTTP 404."
			action = "Verify the exact MCP URL and confirm its MCP handler is deployed. Replacing the token will not fix HTTP 404."
		default:
			message = fmt.Sprintf("The remote MCP server returned HTTP %d.", statusErr.StatusCode)
		}
	} else if strings.Contains(err.Error(), "could not be reached") || errors.Is(err, context.DeadlineExceeded) {
		message = "Fleet could not reach the remote MCP server."
		action = "Check DNS, TLS, and server availability, then retry."
	}

	writeJSON(w, http.StatusFailedDependency, mcpDiscoveryErrorView{
		Error: message, Stage: stage, Action: action, Retryable: true,
	})
}

func (s *Server) configureMCP(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
		writeError(w, http.StatusConflict, "wait for the current instance operation to finish")
		return
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
		return
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "MCP configuration requires an online Host Agent "+agentVersion)
		return
	}
	if s.config.Sealer == nil {
		writeError(w, http.StatusServiceUnavailable, "MCP encryption is unavailable")
		return
	}
	var request mcpConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existingTokens := map[string]string{}
	if existing, existingErr := s.store.GetMCPConfig(r.Context(), instance.ID); existingErr == nil {
		previous, openErr := s.openMCPConfiguration(existing)
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "existing MCP secrets could not be retained")
			return
		}
		for _, server := range previous.Servers {
			existingTokens[server.Name] = server.BearerToken
		}
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "existing MCP configuration could not be read")
		return
	}
	config := domain.MCPConfiguration{Servers: make([]domain.MCPServerConfiguration, 0, len(request.Servers))}
	for _, candidate := range request.Servers {
		token := strings.TrimSpace(candidate.BearerToken)
		if token == "" && !candidate.ClearBearerToken {
			token = existingTokens[strings.ToLower(strings.TrimSpace(candidate.Name))]
		}
		config.Servers = append(config.Servers, domain.MCPServerConfiguration{
			Name: candidate.Name, Source: candidate.Source, URL: candidate.URL, AuthType: candidate.AuthType,
			BearerToken: token, Enabled: candidate.Enabled, Tools: candidate.Tools,
		})
	}
	config, err = mcpconfig.NormalizeAndValidate(config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plaintext, err := json.Marshal(config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be encoded")
		return
	}
	defer clearBytes(plaintext)
	digest := sha256.Sum256(plaintext)
	revision := hex.EncodeToString(digest[:])
	ciphertext, err := s.config.Sealer.Seal(plaintext, "instance-mcp:"+instance.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be encrypted")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MCP configuration identity could not be created")
		return
	}
	payload, _ := json.Marshal(domain.MCPApplyPayload{
		InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		DesiredStatus: instance.Status, APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, Revision: revision,
	})
	names := make([]string, 0, len(config.Servers))
	for _, server := range config.Servers {
		names = append(names, server.Name)
	}
	metadata, _ := json.Marshal(map[string]any{"revision": revision, "servers": names, "server_count": len(names)})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CONFIGURE_MCP", Status: domain.OperationPending,
		Summary: "Apply MCP configuration " + instance.Name, Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.mcp.configure", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	record := store.MCPConfigRecord{InstanceID: instance.ID, Ciphertext: ciphertext, DesiredRevision: revision, Status: "PENDING", UpdatedAt: now}
	if err := s.store.QueueMCPConfiguration(r.Context(), instance.Status, record, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "MCP configuration could not be queued while the instance is busy")
			return
		}
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) downloadMCPConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	hostID, jobID, leaseToken := r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), r.Header.Get(leaseTokenHeader)
	payloadData, err := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, "instance.mcp.configure")
	if err != nil {
		writeError(w, http.StatusConflict, "MCP configuration lease is no longer active")
		return
	}
	var payload domain.MCPApplyPayload
	if err := json.Unmarshal(payloadData, &payload); err != nil || payload.InstanceID == "" || payload.Revision == "" {
		writeError(w, http.StatusConflict, "MCP configuration job payload is invalid")
		return
	}
	record, err := s.store.GetMCPConfig(r.Context(), payload.InstanceID)
	if err != nil || record.DesiredRevision != payload.Revision {
		writeError(w, http.StatusConflict, "MCP configuration revision is no longer current")
		return
	}
	config, err := s.openMCPConfiguration(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MCP configuration could not be decrypted")
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (s *Server) requestCredentials(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if instance.ManagedPath == "" || (instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped) {
		writeError(w, http.StatusConflict, "credentials are only available for a provisioned running or stopped instance")
		return
	}
	host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
	if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "credential inspection requires Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create inspection identity")
		return
	}
	payload, _ := json.Marshal(domain.ActionPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
		ManagedPath: instance.ManagedPath, APIPort: instance.APIPort, PreserveData: true,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CREDENTIAL_REVEAL", Status: domain.OperationPending,
		Summary: "Reveal credentials " + instance.Name, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.credentials.inspect", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) {
			writeError(w, http.StatusConflict, "the instance is busy with another operation; wait for it to finish, then retry")
			return
		}
		writeError(w, http.StatusConflict, "credential inspection could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) getCredentialReveal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	operationID := r.PathValue("operationID")
	operation, err := s.store.GetOperation(r.Context(), operationID)
	if err != nil || operation.Type != "CREDENTIAL_REVEAL" {
		writeError(w, http.StatusNotFound, "credential reveal not found")
		return
	}
	if operation.Status == domain.OperationFailed {
		writeError(w, http.StatusConflict, operation.Error)
		return
	}
	if operation.Status != domain.OperationSucceeded {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, operation)
		return
	}
	ciphertext, expiresAt, err := s.store.GetCredentialReveal(r.Context(), operationID, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusGone, "credential reveal expired")
		return
	}
	plaintext, err := s.config.Sealer.Open(ciphertext, operationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "credential reveal could not be decrypted")
		return
	}
	var credentials domain.Credentials
	if err := json.Unmarshal(plaintext, &credentials); err != nil {
		writeError(w, http.StatusInternalServerError, "credential reveal is invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": credentials, "expires_at": expiresAt})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context(), s.config.OfflineAfter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list hosts")
		return
	}
	instances, err := s.instancesWithEffectiveObservations(r.Context(), hosts, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list instances")
		return
	}
	operations, err := s.store.ListOperations(r.Context(), 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list operations")
		return
	}
	streamID, revision := s.events.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts": hosts, "instances": instances, "operations": operations,
		"stream_id": streamID, "state_revision": revision,
	})
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context(), s.config.OfflineAfter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list hosts")
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context(), s.config.OfflineAfter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list hosts")
		return
	}
	instances, err := s.instancesWithEffectiveObservations(r.Context(), hosts, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list instances")
		return
	}
	writeJSON(w, http.StatusOK, instances)
}

type publishInstanceDashboardRequest struct {
	PublicHostname string `json:"public_hostname"`
}

func publicationSteps(stage, status, detail string) []domain.OperationStep {
	names := []string{"VALIDATING_HOSTNAME", "CREATING_DNS", "UPDATING_INGRESS", "VERIFYING_CLOUDFLARE", "CHECKING_PUBLIC_ENDPOINT"}
	steps := make([]domain.OperationStep, 0, len(names))
	reached := true
	for _, name := range names {
		stepStatus := "pending"
		stepDetail := ""
		if name == stage {
			stepStatus, stepDetail, reached = status, detail, false
		} else if reached {
			stepStatus = "succeeded"
		}
		steps = append(steps, domain.OperationStep{Stage: name, Status: stepStatus, Detail: stepDetail})
	}
	return steps
}

func (s *Server) publishInstanceDashboard(w http.ResponseWriter, r *http.Request) {
	if s.config.RemoteAccess == nil {
		writeError(w, http.StatusServiceUnavailable, "remote access runtime is unavailable")
		return
	}
	var request publishInstanceDashboardRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if errors.Is(err, store.ErrNotFound) || instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "instance could not be loaded")
		return
	}
	configuration := s.config.RemoteAccess.Configuration(r.Context())
	hostname, err := cloudflare.NormalizePublicHostname(request.PublicHostname)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if hostname != "" && (configuration.Mode != remoteaccess.ModeManagedCloudflare || !configuration.InstancePublishingConfigured) {
		writeError(w, http.StatusConflict, "connect and verify Instance publishing in System → Remote access first")
		return
	}
	if hostname != "" {
		expected, hostnameErr := cloudflare.BuildInstancePublicHostname(
			configuration.InstancePublishingFleetNamespace,
			instance.Name,
			configuration.InstancePublishingZone,
		)
		if hostnameErr != nil {
			writeError(w, http.StatusConflict, hostnameErr.Error())
			return
		}
		current, currentErr := cloudflare.NormalizePublicHostname(instance.PublicHostname)
		if currentErr != nil {
			writeError(w, http.StatusConflict, currentErr.Error())
			return
		}
		if hostname != expected && (current == "" || hostname != current) {
			writeError(w, http.StatusBadRequest, "public hostname is managed by Fleet and must match "+expected)
			return
		}
	}
	operationID, err := identity.New()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create operation identity")
		return
	}
	now := time.Now().UTC()
	summary := "Publish dashboard " + instance.Name
	if hostname == "" {
		summary = "Unpublish dashboard " + instance.Name
	}
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "PUBLISH_DASHBOARD", Status: domain.OperationPending,
		Summary: summary, Metadata: operationMetadata(map[string]any{"public_hostname": hostname}),
		Progress:  &domain.JobProgress{Stage: "VALIDATING_HOSTNAME", Steps: publicationSteps("VALIDATING_HOSTNAME", "running", "")},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.StartInstancePublishing(r.Context(), instance.ID, hostname, operation); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	go s.runPublishInstanceDashboard(operation.ID, instance.ID, hostname)
	writeJSON(w, http.StatusAccepted, operation)
}

const (
	publicationOperationTimeout         = 2 * time.Minute
	publicationPropagationRetryInterval = 5 * time.Second
	publicationRecoveryGrace            = 30 * time.Second
	publicationRecoveryBatchSize        = 100
	instanceDeletionCleanupTimeout      = 2 * time.Minute
	instanceDeletionCleanupBatchSize    = 100
)

func deletionCleanupProgress(stage, status, detail string) domain.JobProgress {
	stages := []string{"DELETING_RUNTIME", "REMOVING_DNS", "REMOVING_INGRESS", "VERIFYING_ROUTE_REMOVAL", "FINALIZING_DELETION"}
	steps := make([]domain.OperationStep, 0, len(stages))
	reached := false
	for _, item := range stages {
		stepStatus := "pending"
		stepDetail := ""
		if item == stage {
			stepStatus = status
			stepDetail = detail
			reached = true
		} else if !reached {
			stepStatus = "succeeded"
		}
		steps = append(steps, domain.OperationStep{Stage: item, Status: stepStatus, Detail: stepDetail})
	}
	return domain.JobProgress{Stage: stage, Detail: detail, ActionCode: map[bool]string{true: "retry_cleanup"}[status == "failed"], Steps: steps}
}

func deletionCleanupFailureStage(err error) string {
	detail := strings.ToLower(err.Error())
	switch {
	case strings.Contains(detail, "dns"):
		return "REMOVING_DNS"
	case strings.Contains(detail, "ingress") || strings.Contains(detail, "tunnel"):
		return "REMOVING_INGRESS"
	default:
		return "VERIFYING_ROUTE_REMOVAL"
	}
}

func resourcesForInstance(resources []domain.RemoteAccessResource, instanceID string) []domain.RemoteAccessResource {
	owned := make([]domain.RemoteAccessResource, 0, 2)
	for _, resource := range resources {
		if resource.InstanceID == instanceID {
			owned = append(owned, resource)
		}
	}
	return owned
}

func (s *Server) updateInstanceDeletionCleanup(item store.PendingInstanceDeletion, progress domain.JobProgress, cleanupErr string, completed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.UpdateInstanceDeletionCleanup(ctx, item, progress, cleanupErr, completed, time.Now().UTC()); err != nil {
		return err
	}
	s.events.Publish("operation.changed", item.OperationID)
	s.events.Publish("instance.changed", item.InstanceID)
	return nil
}

func (s *Server) runInstanceDeletionCleanup(item store.PendingInstanceDeletion) {
	unlock := s.deletionLocks.lock(item.InstanceID)
	defer unlock()

	progress := deletionCleanupProgress("REMOVING_DNS", "running", "Removing Fleet-owned Cloudflare publication resources")
	if err := s.updateInstanceDeletionCleanup(item, progress, "", false); err != nil {
		if !errors.Is(err, store.ErrStateChanged) && !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("start instance deletion cleanup", "instance_id", item.InstanceID, "operation_id", item.OperationID, "error", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), instanceDeletionCleanupTimeout)
	defer cancel()
	resources, err := s.store.ListRemoteAccessResources(ctx)
	if err != nil {
		s.failInstanceDeletionCleanup(item, "VERIFYING_ROUTE_REMOVAL", fmt.Errorf("load Fleet-owned Cloudflare resources: %w", err))
		return
	}
	remaining := resourcesForInstance(resources, item.InstanceID)
	if len(remaining) > 0 {
		if s.config.RemoteAccess == nil {
			s.failInstanceDeletionCleanup(item, "REMOVING_DNS", errors.New("remote access is unavailable; Fleet-owned Cloudflare resources still require cleanup"))
			return
		}
		if err := s.config.RemoteAccess.Reconcile(ctx); err != nil {
			// Reconciliation can fail while validating another instance after the
			// target resources were already removed. Decide this deletion only
			// from its own durable ownership records.
			resources, readErr := s.store.ListRemoteAccessResources(ctx)
			if readErr != nil || len(resourcesForInstance(resources, item.InstanceID)) > 0 {
				if readErr != nil {
					err = fmt.Errorf("%v; verify remaining ownership: %w", err, readErr)
				}
				s.failInstanceDeletionCleanup(item, deletionCleanupFailureStage(err), err)
				return
			}
		}
	}

	progress = deletionCleanupProgress("VERIFYING_ROUTE_REMOVAL", "running", "Verifying Fleet-owned DNS and ingress are absent")
	if err := s.updateInstanceDeletionCleanup(item, progress, "", false); err != nil {
		return
	}
	resources, err = s.store.ListRemoteAccessResources(ctx)
	if err != nil {
		s.failInstanceDeletionCleanup(item, "VERIFYING_ROUTE_REMOVAL", fmt.Errorf("verify Fleet-owned Cloudflare cleanup: %w", err))
		return
	}
	if remaining = resourcesForInstance(resources, item.InstanceID); len(remaining) > 0 {
		s.failInstanceDeletionCleanup(item, "VERIFYING_ROUTE_REMOVAL", fmt.Errorf("%d Fleet-owned Cloudflare resources remain", len(remaining)))
		return
	}
	progress = deletionCleanupProgress("FINALIZING_DELETION", "succeeded", "Runtime and Fleet-owned Cloudflare resources were removed")
	if err := s.updateInstanceDeletionCleanup(item, progress, "", true); err != nil && !errors.Is(err, store.ErrStateChanged) {
		s.logger.Error("finalize instance deletion", "instance_id", item.InstanceID, "operation_id", item.OperationID, "error", err)
	}
}

func (s *Server) failInstanceDeletionCleanup(item store.PendingInstanceDeletion, stage string, cleanupErr error) {
	progress := deletionCleanupProgress(stage, "failed", cleanupErr.Error())
	if err := s.updateInstanceDeletionCleanup(item, progress, cleanupErr.Error(), false); err != nil && !errors.Is(err, store.ErrStateChanged) {
		s.logger.Error("record instance deletion cleanup failure", "instance_id", item.InstanceID, "operation_id", item.OperationID, "error", err)
	}
}

func (s *Server) reconcilePendingInstanceDeletions(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	items, err := s.store.ListPendingInstanceDeletions(ctx, instanceDeletionCleanupBatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Error("list pending instance deletion cleanups", "error", err)
		}
		return
	}
	for _, item := range items {
		go s.runInstanceDeletionCleanup(item)
	}
}

func (s *Server) retryInstanceDeletionCleanup(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.PendingInstanceDeletion(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "instance does not have pending Cloudflare cleanup")
			return
		}
		writeError(w, http.StatusInternalServerError, "instance deletion cleanup could not be loaded")
		return
	}
	progress := deletionCleanupProgress("REMOVING_DNS", "running", "Retrying Fleet-owned Cloudflare cleanup")
	if err := s.updateInstanceDeletionCleanup(item, progress, "", false); err != nil {
		writeError(w, http.StatusConflict, "instance deletion cleanup could not be restarted")
		return
	}
	operation, err := s.store.GetOperation(r.Context(), item.OperationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "instance deletion operation could not be loaded")
		return
	}
	go s.runInstanceDeletionCleanup(item)
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) reconcileStalePublicationOperations(ctx context.Context) {
	if ctx.Err() != nil || s.config.RemoteAccess == nil {
		return
	}
	accessStatus := s.config.RemoteAccess.Status()
	if !accessStatus.Configured || accessStatus.State == "syncing" {
		return
	}
	now := time.Now().UTC()
	cutoff := now.Add(-(publicationOperationTimeout + publicationRecoveryGrace))
	operations, err := s.store.ListStalePublishingOperations(ctx, cutoff, publicationRecoveryBatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Error("list stale dashboard publication operations", "error", err)
		}
		return
	}
	if len(operations) == 0 {
		return
	}
	configuration := s.config.RemoteAccess.Configuration(ctx)
	routes := make(map[string]*remoteaccess.PublishedRoute, len(configuration.InstanceRoutes))
	for index := range configuration.InstanceRoutes {
		route := &configuration.InstanceRoutes[index]
		routes[route.InstanceID] = route
	}
	for _, operation := range operations {
		status, progress, operationErr, decided := stalePublicationOutcome(operation, accessStatus, routes[operation.InstanceID])
		if !decided {
			continue
		}
		changed, finalizeErr := s.store.FinalizeStalePublishingOperation(
			ctx, operation.ID, cutoff, status, progress, operationErr, now,
		)
		if finalizeErr != nil {
			if !errors.Is(finalizeErr, context.Canceled) {
				s.logger.Error("finalize stale dashboard publication operation", "operation_id", operation.ID, "error", finalizeErr)
			}
			continue
		}
		if !changed {
			continue
		}
		s.logger.Info("reconciled stale dashboard publication operation", "operation_id", operation.ID, "instance_id", operation.InstanceID, "status", status)
		s.events.Publish("operation.changed", operation.ID)
		s.events.Publish("instance.changed", operation.InstanceID)
	}
}

func stalePublicationOutcome(
	operation domain.Operation,
	accessStatus remoteaccess.Status,
	route *remoteaccess.PublishedRoute,
) (string, domain.JobProgress, string, bool) {
	var metadata struct {
		PublicHostname *string `json:"public_hostname"`
	}
	if err := json.Unmarshal(operation.Metadata, &metadata); err != nil || metadata.PublicHostname == nil {
		detail := "Publication was interrupted and its saved target is invalid. Retry publishing."
		return domain.OperationFailed, domain.JobProgress{
			Stage: "VALIDATING_HOSTNAME", Detail: detail, ActionCode: "retry",
			Steps: publicationSteps("VALIDATING_HOSTNAME", "failed", detail),
		}, detail, true
	}

	hostname := *metadata.PublicHostname
	if hostname == "" {
		if accessStatus.State == "synced" && (route == nil || route.Hostname == "") {
			detail := "Fleet-owned DNS and ingress resources were removed; the interrupted operation was recovered from current Cloudflare state"
			return domain.OperationSucceeded, domain.JobProgress{
				Stage: "CHECKING_PUBLIC_ENDPOINT", Detail: detail,
				Steps: publicationSteps("CHECKING_PUBLIC_ENDPOINT", "succeeded", "Dashboard is not published"),
			}, "", true
		}
		return interruptedPublicationFailure(accessStatus, route, "Dashboard unpublishing did not reach a verified final state")
	}

	if route != nil && route.Hostname == hostname && publicationRouteVerified(route) {
		detail := "DNS, tunnel ingress, and public endpoint were verified after the interrupted operation"
		return domain.OperationSucceeded, domain.JobProgress{
			Stage: "CHECKING_PUBLIC_ENDPOINT", Detail: detail, Steps: publicationOperationSteps(route),
		}, "", true
	}
	if route != nil && route.Hostname != "" && route.Hostname != hostname {
		detail := "The instance publication target changed before this operation could be verified. Review the current hostname and retry if needed."
		return domain.OperationFailed, domain.JobProgress{
			Stage: "VERIFYING_CLOUDFLARE", Detail: detail, ActionCode: "retry",
			Steps: publicationSteps("VERIFYING_CLOUDFLARE", "failed", detail),
		}, detail, true
	}
	return interruptedPublicationFailure(accessStatus, route, "Dashboard publication did not reach a verified final state")
}

func interruptedPublicationFailure(
	accessStatus remoteaccess.Status,
	route *remoteaccess.PublishedRoute,
	fallback string,
) (string, domain.JobProgress, string, bool) {
	if accessStatus.State == "syncing" {
		return "", domain.JobProgress{}, "", false
	}
	stage, detail, action := publicationRouteFailure(route)
	if detail == "" {
		detail = fallback
		if accessStatus.LastError != "" {
			detail += ": " + accessStatus.LastError
		}
	}
	var steps []domain.OperationStep
	if route == nil {
		steps = publicationSteps(stage, "failed", detail)
	} else {
		steps = publicationOperationSteps(route)
	}
	return domain.OperationFailed, domain.JobProgress{
		Stage: stage, Detail: detail, ActionCode: action, Steps: steps,
	}, detail, true
}

func publicationRouteVerified(route *remoteaccess.PublishedRoute) bool {
	return route != nil &&
		route.DNSState == cloudflare.ResourceReady &&
		route.RouteState == cloudflare.ResourceReady &&
		route.EndpointState == cloudflare.EndpointReachable
}

func publicationRouteFailure(route *remoteaccess.PublishedRoute) (stage, detail, action string) {
	stage, action = "CHECKING_PUBLIC_ENDPOINT", "retry"
	if route == nil {
		return "VERIFYING_CLOUDFLARE", "Publication result is unavailable", action
	}
	detail = route.EndpointDetail
	if route.DNSState != cloudflare.ResourceReady {
		stage, detail = "CREATING_DNS", route.DNSDetail
	} else if route.RouteState != cloudflare.ResourceReady {
		stage, detail = "UPDATING_INGRESS", route.RouteDetail
	} else if route.EndpointState == cloudflare.EndpointAccessProtected {
		action = "review_cloudflare_access"
	}
	return stage, detail, action
}

func (s *Server) runPublishInstanceDashboard(operationID, instanceID, hostname string) {
	ctx, cancel := context.WithTimeout(context.Background(), publicationOperationTimeout)
	defer cancel()
	update := func(stage, status, detail, actionCode, operationStatus string, steps []domain.OperationStep) {
		if steps == nil {
			steps = publicationSteps(stage, status, detail)
		}
		errorText := ""
		if operationStatus == domain.OperationFailed {
			errorText = detail
		}
		persistContext, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer persistCancel()
		_ = s.store.UpdateControlPlaneOperation(persistContext, operationID, operationStatus, domain.JobProgress{
			Stage: stage, Detail: detail, ActionCode: actionCode, Steps: steps,
		}, errorText, time.Now().UTC())
		s.events.Publish("operation.changed", operationID)
		s.events.Publish("instance.changed", instanceID)
	}
	update("VALIDATING_HOSTNAME", "succeeded", "Hostname is valid and uniquely assigned", "", domain.OperationRunning, nil)
	if hostname == "" {
		update("CREATING_DNS", "running", "Removing Fleet-owned publication resources", "", domain.OperationRunning, nil)
	} else {
		update("CREATING_DNS", "running", "Creating or verifying the Fleet-owned DNS record", "", domain.OperationRunning, nil)
	}
	for {
		if err := s.config.RemoteAccess.Reconcile(ctx); err != nil {
			stage, detail, action := classifyPublicationFailure(err)
			update(stage, "failed", detail, action, domain.OperationFailed, nil)
			return
		}
		if hostname == "" {
			steps := publicationSteps("CHECKING_PUBLIC_ENDPOINT", "succeeded", "Dashboard is not published")
			update("CHECKING_PUBLIC_ENDPOINT", "succeeded", "Fleet-owned DNS and ingress resources were removed", "", domain.OperationSucceeded, steps)
			return
		}
		configuration := s.config.RemoteAccess.Configuration(ctx)
		var route *remoteaccess.PublishedRoute
		for index := range configuration.InstanceRoutes {
			if configuration.InstanceRoutes[index].InstanceID == instanceID {
				route = &configuration.InstanceRoutes[index]
				break
			}
		}
		if route == nil {
			update("VERIFYING_CLOUDFLARE", "failed", "Publication result is unavailable", "retry", domain.OperationFailed, nil)
			return
		}
		steps := publicationOperationSteps(route)
		if publicationRouteVerified(route) {
			update("CHECKING_PUBLIC_ENDPOINT", "succeeded", "DNS, tunnel ingress, and public endpoint are verified", "", domain.OperationSucceeded, steps)
			return
		}
		if route.EndpointState == cloudflare.EndpointPropagating && ctx.Err() == nil {
			update("CHECKING_PUBLIC_ENDPOINT", "running", route.EndpointDetail, "", domain.OperationRunning, steps)
			timer := time.NewTimer(publicationPropagationRetryInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				update("CHECKING_PUBLIC_ENDPOINT", "failed", "Public DNS did not finish propagating before the verification deadline", "retry", domain.OperationFailed, steps)
				return
			case <-timer.C:
				continue
			}
		}
		stage, detail, action := publicationRouteFailure(route)
		if detail == "" {
			detail = "Dashboard publication could not be verified"
		}
		update(stage, "failed", detail, action, domain.OperationFailed, steps)
		return
	}
}

func publicationOperationSteps(route *remoteaccess.PublishedRoute) []domain.OperationStep {
	configurationStatus := map[bool]string{true: "succeeded", false: "failed"}[route.DNSState == cloudflare.ResourceReady && route.RouteState == cloudflare.ResourceReady]
	configurationDetail := route.ProviderDetail
	if configurationStatus == "succeeded" {
		configurationDetail = "Cloudflare DNS and tunnel ingress match Fleet-owned configuration"
	}
	return []domain.OperationStep{
		{Stage: "VALIDATING_HOSTNAME", Status: "succeeded"},
		{Stage: "CREATING_DNS", Status: mapResourceStepStatus(route.DNSState), Detail: route.DNSDetail},
		{Stage: "UPDATING_INGRESS", Status: mapResourceStepStatus(route.RouteState), Detail: route.RouteDetail},
		{Stage: "VERIFYING_CLOUDFLARE", Status: configurationStatus, Detail: configurationDetail},
		{Stage: "CHECKING_PUBLIC_ENDPOINT", Status: mapEndpointStepStatus(route.EndpointState), Detail: route.EndpointDetail},
	}
}

func classifyPublicationFailure(err error) (stage, detail, action string) {
	stage, detail, action = "CREATING_DNS", err.Error(), "retry"
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "tunnel configuration") || strings.Contains(lower, "instance routes") || strings.Contains(lower, "ingress") {
		stage = "UPDATING_INGRESS"
	} else if strings.Contains(lower, "verification") || strings.Contains(lower, "verify cloudflare") {
		stage = "VERIFYING_CLOUDFLARE"
	}
	if strings.Contains(lower, "api token") || strings.Contains(lower, "http 403") {
		action = "replace_api_token"
		if stage == "UPDATING_INGRESS" {
			detail = "Cloudflare API token cannot edit tunnel configuration."
		}
	}
	return stage, detail, action
}

func mapResourceStepStatus(value string) string {
	if value == cloudflare.ResourceReady {
		return "succeeded"
	}
	if value == cloudflare.ResourcePending || value == "" {
		return "pending"
	}
	return "failed"
}

func mapEndpointStepStatus(value string) string {
	if value == cloudflare.EndpointReachable {
		return "succeeded"
	}
	if value == cloudflare.EndpointPropagating {
		return "running"
	}
	if value == cloudflare.EndpointUnchecked || value == cloudflare.EndpointChecking || value == "" {
		return "pending"
	}
	return "failed"
}

func (s *Server) requestObservationRefresh(w http.ResponseWriter, r *http.Request) {
	requestID, err := identity.New()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create observation request identity")
		return
	}
	request, err := s.store.RequestObservation(r.Context(), r.PathValue("instanceID"), requestID, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if errors.Is(err, store.ErrObservationNotReady) {
		writeError(w, http.StatusConflict, "instance is not ready for observation")
		return
	}
	if errors.Is(err, store.ErrObservationBusy) {
		writeError(w, http.StatusConflict, "a diagnostics refresh is already pending for this instance")
		return
	}
	if err != nil {
		s.logger.Error("request observation refresh", "error", err)
		writeError(w, http.StatusInternalServerError, "observation refresh could not be requested")
		return
	}
	writeJSON(w, http.StatusAccepted, request)
}

func (s *Server) instancesWithEffectiveObservations(ctx context.Context, hosts []domain.Host, now time.Time) ([]domain.Instance, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	hostStatus := make(map[string]string, len(hosts))
	for _, host := range hosts {
		hostStatus[host.ID] = host.Status
	}
	catalog, catalogErr := s.hermesCatalog(ctx)
	if catalogErr != nil {
		catalog = s.config.HermesCatalog
	}
	for index := range instances {
		instance := &instances[index]
		targetGeneration := instance.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if release, ok := releases.FindByRuntimeImage(catalog, instance.Image); ok {
			instance.HermesVersion = release.Version
			instance.HermesSource = release.Commit
		}
		if instance.Observation != nil &&
			(instance.Observation.TargetGeneration == "" || instance.Observation.TargetGeneration == targetGeneration) &&
			instance.Observation.HermesVersion != "" {
			instance.HermesVersion = instance.Observation.HermesVersion
			instance.HermesSource = instance.Observation.HermesSource
			instance.HermesVersionVerified = instance.Observation.TargetGeneration == targetGeneration
		}
		remediation, remediationErr := s.store.GetRuntimeRemediation(ctx, instance.ID)
		if remediationErr != nil {
			return nil, remediationErr
		}
		instance.RuntimeRemediation = remediation
		reason := ""
		switch {
		case instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting:
			reason = "Deleted or deleting instances are not observed"
		case instance.Status == domain.InstanceProvisioning || instance.Status == domain.InstanceRestarting ||
			instance.Status == domain.InstanceUpdating || instance.Status == domain.InstanceReconciling:
			reason = "Desired state is changing; awaiting a stable lifecycle state"
		case hostStatus[instance.HostID] != domain.HostOnline:
			reason = "Host is offline; current runtime state is unknown"
		case instance.Observation == nil:
			reason = "No runtime observation has been received"
		case instance.Observation.TargetGeneration != targetGeneration:
			reason = "Desired state changed; awaiting a current observation"
		case instance.Observation.ReceivedAt.IsZero() || instance.Observation.ObservedAt.IsZero() ||
			instance.Observation.ReceivedAt.After(now.Add(2*time.Minute)) ||
			instance.Observation.ObservedAt.After(instance.Observation.ReceivedAt.Add(2*time.Minute)) ||
			now.Sub(instance.Observation.ReceivedAt) > s.config.ObservationStaleAfter ||
			now.Sub(instance.Observation.ObservedAt) > s.config.ObservationStaleAfter:
			reason = "Runtime observation is stale"
		}
		if reason != "" {
			instance.HermesVersionVerified = false
			observation := domain.InstanceObservation{InstanceID: instance.ID, Status: domain.ObservationUnknown, Summary: reason}
			if instance.Observation != nil {
				observation = *instance.Observation
				observation.Status = domain.ObservationUnknown
				observation.Summary = reason
			}
			instance.Observation = &observation
		}
		if s.config.RemoteAccess != nil {
			instance.PublicDashboardURL = s.config.RemoteAccess.PublicDashboardURL(instance.ID, instance.PublicHostname)
		}
	}
	return instances, nil
}

func (s *Server) cancelRuntimeRemediation(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "instance could not be loaded")
		return
	}
	if instance.Status == domain.InstanceRestarting {
		writeError(w, http.StatusConflict, "the active recovery attempt must finish before automatic recovery can be stopped")
		return
	}
	if err := s.store.CancelRuntimeRemediation(r.Context(), instance.ID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusConflict, "automatic runtime recovery is not active")
		return
	}
	state, err := s.store.GetRuntimeRemediation(r.Context(), instance.ID)
	if err != nil || state == nil {
		writeError(w, http.StatusInternalServerError, "automatic runtime recovery state could not be loaded")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

type createInstanceRequest struct {
	Name          string `json:"name"`
	HostID        string `json:"host_id"`
	HermesVersion string `json:"hermes_version"`
	Image         string `json:"image"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	ServiceTier   string `json:"service_tier"`
	APIPort       int    `json:"api_port"`
	DashboardPort int    `json:"dashboard_port"`
}

func (s *Server) hermesCatalog(ctx context.Context) (releases.Catalog, error) {
	var sourceErr error
	if s.config.HermesReleaseSource != nil {
		catalog, err := s.config.HermesReleaseSource.List(ctx, 3)
		if err == nil {
			if len(catalog.Releases) == 3 {
				return catalog, nil
			}
			sourceErr = errors.New("Hermes release source returned an incomplete catalog")
		} else {
			sourceErr = err
		}
	}
	if len(s.config.HermesCatalog.Releases) != 3 {
		if sourceErr != nil {
			return releases.Catalog{}, sourceErr
		}
		return releases.Catalog{}, errors.New("Hermes release catalog is unavailable")
	}
	return s.config.HermesCatalog, nil
}

func (s *Server) listHermesReleases(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.hermesCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Hermes release catalog is unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, catalog)
}

func (s *Server) resolveHermesRelease(ctx context.Context, version string) (releases.Release, error) {
	catalog, err := s.hermesCatalog(ctx)
	if err != nil {
		return releases.Release{}, errors.New("Hermes release catalog is unavailable")
	}
	if version == "" {
		return catalog.Releases[0], nil
	}
	if release, ok := releases.Find(catalog, version); ok {
		return release, nil
	}
	return releases.Release{}, errors.New("select one of the Hermes versions offered by Fleet")
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	var request createInstanceRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.HermesVersion = strings.TrimSpace(request.HermesVersion)
	if request.HermesVersion == "" {
		legacyImage := strings.TrimSpace(request.Image)
		if separator := strings.LastIndex(legacyImage, ":"); separator >= 0 && separator < len(legacyImage)-1 {
			request.HermesVersion = legacyImage[separator+1:]
		}
	}
	release, err := s.resolveHermesRelease(r.Context(), request.HermesVersion)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.HermesVersion = release.Version
	request.Image = release.Image
	request.Provider = "openai-codex"
	request.Model = ""
	request.Reasoning = ""
	request.ServiceTier = ""
	if err := validateCreateInstance(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	host, err := s.store.GetHost(r.Context(), request.HostID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "selected host does not exist")
		return
	}
	now := time.Now().UTC()
	if host.LastSeenAt.IsZero() || now.Sub(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "selected host is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "selected host requires Host Agent "+agentVersion)
		return
	}
	instanceID, operationID, jobID, err := threeIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create resource identity")
		return
	}
	automaticPorts := request.APIPort == 0 && request.DashboardPort == 0
	for attempt := 0; attempt < 5; attempt++ {
		if automaticPorts {
			request.APIPort, request.DashboardPort, err = s.store.NextAvailablePorts(r.Context(), request.HostID)
			if err != nil {
				writeError(w, http.StatusConflict, "automatic host ports could not be allocated")
				return
			}
		}
		now := time.Now().UTC()
		instance := domain.Instance{
			ID: instanceID, Name: request.Name, HostID: request.HostID, Status: domain.InstanceProvisioning,
			Image: request.Image, Provider: request.Provider, Model: request.Model, Reasoning: request.Reasoning,
			ServiceTier: request.ServiceTier, APIPort: request.APIPort, DashboardPort: request.DashboardPort,
			CreatedAt: now, UpdatedAt: now,
		}
		payload, _ := json.Marshal(domain.ProvisionPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image,
			HermesVersion: release.Version, HermesSource: release.Commit, Provider: instance.Provider,
			Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
			APIPort: instance.APIPort, DashboardPort: instance.DashboardPort,
		})
		operation := domain.Operation{ID: operationID, InstanceID: instance.ID, Type: "PROVISION", Status: domain.OperationPending, Summary: "Provision " + instance.Name, CreatedAt: now, UpdatedAt: now}
		job := domain.Job{ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID, Type: "instance.provision", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now}
		if err = s.store.CreateInstance(r.Context(), instance, operation, job); err == nil {
			if s.config.RemoteAccess != nil {
				s.config.RemoteAccess.Trigger()
			}
			writeJSON(w, http.StatusAccepted, instance)
			return
		}
		if !automaticPorts {
			break
		}
	}
	if errors.Is(err, store.ErrQueueCapacity) {
		s.writeQueueAdmissionError(w, err)
		return
	}
	writeError(w, http.StatusConflict, "instance name or host ports are already allocated")
}

type hermesUpdateResponse struct {
	CurrentVersion    string            `json:"current_version,omitempty"`
	CurrentSource     string            `json:"current_source,omitempty"`
	CurrentImage      string            `json:"current_image"`
	OfficialStatus    string            `json:"official_status"`
	UpdateKind        string            `json:"update_kind"`
	OfficialSource    string            `json:"official_source,omitempty"`
	OfficialCheckedAt string            `json:"official_checked_at,omitempty"`
	OfficialStale     bool              `json:"official_stale,omitempty"`
	LatestRelease     *releases.Release `json:"latest_release,omitempty"`
	TargetVersion     string            `json:"target_version"`
	TargetSource      string            `json:"target_source"`
	TargetImage       string            `json:"target_image"`
	Available         bool              `json:"available"`
	Eligible          bool              `json:"eligible"`
	Reason            string            `json:"reason"`
}

const (
	hermesUpdateKindNone           = "NONE"
	hermesUpdateKindVersionUpdate  = "VERSION_UPDATE"
	hermesUpdateKindRuntimeRefresh = "RUNTIME_REFRESH"
)

func (s *Server) getHermesUpdate(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	status, err := s.hermesUpdateStatus(r.Context(), instance)
	if err != nil {
		s.logger.Error("resolve Hermes update", "instance_id", instance.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "Hermes update status could not be resolved")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) startHermesUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	var request struct {
		ConfirmName   string `json:"confirm_name"`
		WorkflowID    string `json:"workflow_id"`
		RestoreStatus string `json:"restore_status"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.ConfirmName != instance.Name {
		writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
		return
	}
	if request.WorkflowID != "" && !validWorkflowID(request.WorkflowID) {
		writeError(w, http.StatusBadRequest, "workflow identity is invalid")
		return
	}
	status, err := s.hermesUpdateStatus(r.Context(), instance)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Hermes update status could not be resolved")
		return
	}
	if !status.Available || !status.Eligible {
		writeError(w, http.StatusConflict, "this instance does not have an available Hermes maintenance action")
		return
	}
	restoreStatus := instance.Status
	if instance.Status == domain.InstanceFailed {
		if status.UpdateKind != hermesUpdateKindRuntimeRefresh {
			writeError(w, http.StatusConflict, "a failed instance can only run managed runtime recovery")
			return
		}
		if request.RestoreStatus != domain.InstanceRunning {
			writeError(w, http.StatusBadRequest, "managed runtime recovery requires restore_status RUNNING")
			return
		}
		restoreStatus = request.RestoreStatus
	} else {
		if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
			writeError(w, http.StatusConflict, "Hermes can only be updated from a stable running or stopped state")
			return
		}
		if request.RestoreStatus != "" {
			writeError(w, http.StatusBadRequest, "restore_status is only valid for managed runtime recovery")
			return
		}
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "Hermes updates require Host Agent "+agentVersion)
		return
	}
	workflowID := request.WorkflowID
	operation, err := s.queueHermesUpdate(r.Context(), instance, host, status, restoreStatus, workflowID, "FLEET_ADMIN")
	if err != nil {
		var queueErr *hermesUpdateQueueError
		if !errors.As(err, &queueErr) {
			writeError(w, http.StatusInternalServerError, "Hermes update could not be queued")
			return
		}
		switch queueErr.Stage {
		case "identity":
			writeError(w, http.StatusInternalServerError, "could not create update operation identity")
		case "backup":
			s.writeRecoveryError(w, "reserve automatic update backup", queueErr.Err)
		case "encode":
			writeError(w, http.StatusInternalServerError, "could not encode Hermes update")
		case "queue":
			if !s.writeQueueAdmissionError(w, queueErr.Err) {
				writeError(w, http.StatusConflict, "instance state changed before the Hermes update was queued")
			}
		default:
			writeError(w, http.StatusInternalServerError, "Hermes update could not be queued")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) hermesUpdateStatus(ctx context.Context, instance domain.Instance) (hermesUpdateResponse, error) {
	status := hermesUpdateResponse{
		CurrentImage: instance.Image, OfficialStatus: "CHECK_FAILED", UpdateKind: hermesUpdateKindNone,
	}
	catalog, catalogErr := s.hermesCatalog(ctx)
	targetGeneration := instance.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if instance.Observation != nil &&
		(instance.Observation.TargetGeneration == "" || instance.Observation.TargetGeneration == targetGeneration) {
		status.CurrentVersion = instance.Observation.HermesVersion
		status.CurrentSource = instance.Observation.HermesSource
	}
	if status.CurrentVersion == "" {
		if release, ok := releases.FindByRuntimeImage(catalog, instance.Image); ok {
			status.CurrentVersion = release.Version
			status.CurrentSource = release.Commit
		}
	}
	if catalogErr != nil || len(catalog.Releases) == 0 {
		status.Reason = "Official Hermes update information is temporarily unavailable"
		return status, nil
	}
	s.attachOfficialHermesRelease(&status, catalog)
	switch status.OfficialStatus {
	case "CHECK_FAILED":
		status.Reason = "Official Hermes update information is temporarily unavailable"
		return status, nil
	case "UNKNOWN":
		status.Reason = "The installed Hermes version could not be compared with the official release feed"
		return status, nil
	case "CURRENT":
		if status.LatestRelease == nil {
			status.Reason = "The official release feed did not return an installable Hermes release"
			return status, nil
		}
		currentRelease, managedImage := releases.FindByRuntimeImage(
			releases.Catalog{Releases: []releases.Release{*status.LatestRelease}},
			instance.Image,
		)
		if !managedImage ||
			!strings.EqualFold(strings.TrimSpace(status.CurrentSource), strings.TrimSpace(status.LatestRelease.Commit)) ||
			currentRelease.Version != status.LatestRelease.Version ||
			status.LatestRelease.Image == instance.Image {
			status.Reason = "The latest official Hermes version is installed"
			return status, nil
		}
		status.UpdateKind = hermesUpdateKindRuntimeRefresh
		status.TargetVersion = status.LatestRelease.Version
		status.TargetSource = status.LatestRelease.Commit
		status.TargetImage = status.LatestRelease.Image
	case "UPDATE_AVAILABLE":
		if status.LatestRelease == nil {
			status.Reason = "The official release feed did not return an installable Hermes release"
			return status, nil
		}
		status.UpdateKind = hermesUpdateKindVersionUpdate
		status.TargetVersion = status.LatestRelease.Version
		status.TargetSource = status.LatestRelease.Commit
		status.TargetImage = status.LatestRelease.Image
	default:
		status.Reason = "The official Hermes update status is invalid"
		return status, nil
	}
	if !hermesVersionPattern.MatchString(status.TargetVersion) || !hermesCommitPattern.MatchString(status.TargetSource) ||
		providers.ValidateImageReference(status.TargetImage) != nil {
		status.Reason = "No installable Hermes release is available"
		return status, nil
	}
	status.Available = true
	if status.TargetImage == instance.Image {
		status.Available = false
		status.UpdateKind = hermesUpdateKindNone
		status.Reason = "A Hermes update must use a new versioned image reference"
		return status, nil
	}
	if instance.Status == domain.InstanceFailed && status.UpdateKind == hermesUpdateKindRuntimeRefresh {
		recoverable, reason := s.failedRuntimeRefreshRecoveryEligibility(instance, time.Now().UTC())
		if !recoverable {
			status.Reason = reason
			return status, nil
		}
	} else if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
		status.Reason = "Wait until the instance reaches a stable running or stopped state"
		return status, nil
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		status.Reason = "Instance runtime metadata is incomplete"
		return status, nil
	}
	if s.config.RecoveryPoints == nil {
		status.Reason = "Instance backup storage is unavailable"
		return status, nil
	}
	status.Eligible = true
	if status.UpdateKind == hermesUpdateKindRuntimeRefresh {
		if instance.Status == domain.InstanceFailed {
			status.Reason = "Fleet can recover the retained runtime with a verified backup, refresh its managed wrapper, and restore Hermes to RUNNING"
		} else {
			status.Reason = "Hermes remains on the same version while Fleet refreshes its managed runtime, verifies a rollback backup, and restores the current state"
		}
	} else {
		status.Reason = "Fleet will prepare the release, create a verified backup, update Hermes, and restore the current runtime state"
	}
	return status, nil
}

func (s *Server) failedRuntimeRefreshRecoveryEligibility(instance domain.Instance, now time.Time) (bool, string) {
	if instance.Status != domain.InstanceFailed {
		return false, "Managed runtime recovery is only available for a failed instance"
	}
	observation := instance.Observation
	if observation == nil ||
		observation.TargetGeneration != instance.UpdatedAt.UTC().Format(time.RFC3339Nano) ||
		observation.ReceivedAt.IsZero() || observation.ObservedAt.IsZero() ||
		observation.ReceivedAt.After(now.Add(2*time.Minute)) ||
		observation.ObservedAt.After(observation.ReceivedAt.Add(2*time.Minute)) ||
		now.Sub(observation.ReceivedAt) > s.config.ObservationStaleAfter ||
		now.Sub(observation.ObservedAt) > s.config.ObservationStaleAfter {
		return false, "Refresh diagnostics to verify the retained runtime before recovery"
	}
	required := map[string]bool{
		"managed_path":  false,
		"manifest":      false,
		"environment":   false,
		"workspace":     false,
		"docker_daemon": false,
		"data_volume":   false,
		"containers":    false,
		"ownership":     false,
		"image":         false,
	}
	runtimeDrift := false
	for _, check := range observation.Checks {
		if _, ok := required[check.Name]; ok {
			required[check.Name] = check.Status == domain.ObservationCheckOK
		}
		if check.Name == "runtime" {
			runtimeDrift = check.Status == domain.ObservationCheckDrift
		}
	}
	if !runtimeDrift {
		return false, "Refresh diagnostics to confirm the failed lifecycle state before recovery"
	}
	for _, ok := range required {
		if !ok {
			return false, "Retained runtime artifacts must pass diagnostics before managed recovery"
		}
	}
	return true, ""
}

func (s *Server) runtimeRefreshRequired(ctx context.Context, instanceID string) (bool, error) {
	instance, err := s.store.GetInstance(ctx, instanceID)
	if err != nil {
		return false, err
	}
	return s.runtimeRefreshRequiredForInstance(ctx, instance)
}

func (s *Server) runtimeRefreshRequiredForInstance(ctx context.Context, instance domain.Instance) (bool, error) {
	catalog, err := s.hermesCatalog(ctx)
	if err != nil {
		return false, err
	}
	release, managedImage := releases.FindByRuntimeImage(catalog, instance.Image)
	if !managedImage || release.Image == instance.Image {
		return false, nil
	}
	if instance.Observation != nil {
		if version := strings.TrimPrefix(strings.TrimSpace(instance.Observation.HermesVersion), "v"); version != "" &&
			version != release.Version {
			return false, nil
		}
		if source := strings.TrimSpace(instance.Observation.HermesSource); source != "" &&
			!strings.EqualFold(source, release.Commit) {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) attachOfficialHermesRelease(status *hermesUpdateResponse, catalog releases.Catalog) {
	if len(catalog.Releases) == 0 {
		status.OfficialStatus = "CHECK_FAILED"
		return
	}
	latest := catalog.Releases[0]
	status.LatestRelease = &latest
	status.OfficialSource = catalog.Source
	status.OfficialCheckedAt = catalog.CheckedAt.UTC().Format(time.RFC3339)
	status.OfficialStale = catalog.Stale
	if status.CurrentVersion == "" || !hermesVersionPattern.MatchString(status.CurrentVersion) {
		status.OfficialStatus = "UNKNOWN"
		return
	}
	if releases.Compare(status.CurrentVersion, latest.Version) < 0 {
		status.OfficialStatus = "UPDATE_AVAILABLE"
		return
	}
	if releases.Compare(status.CurrentVersion, latest.Version) == 0 {
		status.OfficialStatus = "CURRENT"
		return
	}
	status.OfficialStatus = "UNKNOWN"
}

func recoveryPointMatchesInstance(point recovery.Metadata, instance domain.Instance) bool {
	return point.Status == recovery.StatusReady && !point.VerifiedAt.IsZero() && point.InstanceID == instance.ID &&
		point.Image == instance.Image && point.ImageID == instance.ImageID && point.Provider == instance.Provider &&
		point.Model == instance.Model && point.Reasoning == instance.Reasoning && point.ServiceTier == instance.ServiceTier &&
		point.CodexConfigured == instance.CodexConfigured && point.ProjectName == instance.ProjectName &&
		point.DataVolume == instance.DataVolume && point.ManagedPath == instance.ManagedPath
}

func (s *Server) instanceAction(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	var request struct {
		Action      string `json:"action"`
		ConfirmName string `json:"confirm_name"`
		WorkflowID  string `json:"workflow_id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validWorkflowID(request.WorkflowID) {
		writeError(w, http.StatusBadRequest, "workflow identity is invalid")
		return
	}
	if request.Action != "stop" && request.Action != "delete" && !s.requireOperationCapacity(w) {
		return
	}
	jobType, nextStatus := "", ""
	var payload []byte
	switch request.Action {
	case "retry":
		if instance.Status != domain.InstanceFailed {
			writeError(w, http.StatusConflict, "only a failed instance can be retried")
			return
		}
		refreshRequired, refreshErr := s.runtimeRefreshRequiredForInstance(r.Context(), instance)
		if refreshErr != nil {
			writeError(w, http.StatusInternalServerError, "could not verify managed runtime compatibility")
			return
		}
		if refreshRequired {
			writeError(w, http.StatusConflict, "managed runtime recovery is required; open the instance and recover its managed runtime")
			return
		}
		jobType, nextStatus = "instance.provision", domain.InstanceProvisioning
		payload, _ = json.Marshal(domain.ProvisionPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, Provider: instance.Provider,
			Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
			APIPort: instance.APIPort, DashboardPort: instance.DashboardPort,
		})
	case "start":
		if instance.Status != domain.InstanceStopped {
			writeError(w, http.StatusConflict, "only a stopped instance can be started")
			return
		}
		jobType, nextStatus = "instance.start", domain.InstanceProvisioning
	case "stop":
		if instance.Status != domain.InstanceRunning {
			writeError(w, http.StatusConflict, "only a running instance can be stopped")
			return
		}
		jobType, nextStatus = "instance.stop", domain.InstanceProvisioning
	case "repair-runtime":
		if instance.Status != domain.InstanceRunning {
			writeError(w, http.StatusConflict, "only an instance with a running desired state can repair runtime drift")
			return
		}
		if request.ConfirmName != instance.Name {
			writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
			return
		}
		if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
			writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
			return
		}
		hasFreshDrift, observationErr := s.store.HasFreshObservationCheck(
			r.Context(), instance.ID, "runtime", domain.ObservationCheckDrift,
			time.Now().UTC().Add(-s.config.ObservationStaleAfter),
		)
		if observationErr == nil && !hasFreshDrift {
			hasFreshDrift, observationErr = s.store.HasFreshObservationCheck(
				r.Context(), instance.ID, "health_endpoint", domain.ObservationCheckDrift,
				time.Now().UTC().Add(-s.config.ObservationStaleAfter),
			)
		}
		if observationErr != nil {
			writeError(w, http.StatusInternalServerError, "could not verify current diagnostics")
			return
		}
		if !hasFreshDrift {
			writeError(w, http.StatusConflict, "refresh diagnostics before repairing the runtime")
			return
		}
		host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
		if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
			writeError(w, http.StatusConflict, "the instance Host Agent is offline")
			return
		}
		if host.AgentVersion != agentVersion {
			writeError(w, http.StatusConflict, "runtime repair requires Host Agent "+agentVersion)
			return
		}
		jobType, nextStatus = "instance.runtime.repair", domain.InstanceRestarting
		payload, _ = json.Marshal(domain.RuntimeRepairPayload{
			ActionPayload: domain.ActionPayload{
				InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ProjectName: instance.ProjectName,
				ManagedPath: instance.ManagedPath, ImageID: instance.ImageID, Provider: instance.Provider, Model: instance.Model,
				Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
				APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, PreserveData: true,
			},
			Phase: 1, Attempt: 1, Trigger: "manual",
		})
	case "sync-runtime":
		if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
			writeError(w, http.StatusConflict, "only a running or stopped instance can synchronize runtime configuration")
			return
		}
		if request.ConfirmName != instance.Name {
			writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
			return
		}
		if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
			writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
			return
		}
		if !instance.CodexConfigured {
			writeError(w, http.StatusConflict, "configure Codex before synchronizing runtime configuration")
			return
		}
		if err := providers.ValidateRuntime(instance.Provider, instance.Model, instance.Reasoning, instance.ServiceTier); err != nil {
			writeError(w, http.StatusConflict, "saved Codex configuration is invalid")
			return
		}
		refreshRequired, refreshErr := s.runtimeRefreshRequiredForInstance(r.Context(), instance)
		if refreshErr != nil {
			writeError(w, http.StatusInternalServerError, "could not verify managed runtime compatibility")
			return
		}
		if refreshRequired {
			writeError(w, http.StatusConflict, "refresh the managed runtime before synchronizing runtime configuration")
			return
		}
		hasFreshDrift, observationErr := s.store.HasFreshObservationCheck(
			r.Context(), instance.ID, "runtime_configuration", domain.ObservationCheckDrift,
			time.Now().UTC().Add(-s.config.ObservationStaleAfter),
		)
		if observationErr != nil {
			writeError(w, http.StatusInternalServerError, "could not verify current diagnostics")
			return
		}
		if !hasFreshDrift {
			writeError(w, http.StatusConflict, "refresh diagnostics before synchronizing runtime configuration")
			return
		}
		host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
		if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
			writeError(w, http.StatusConflict, "the instance Host Agent is offline")
			return
		}
		if host.AgentVersion != agentVersion {
			writeError(w, http.StatusConflict, "runtime synchronization requires Host Agent "+agentVersion)
			return
		}
		jobType, nextStatus = "instance.runtime.sync", domain.InstanceUpdating
		payload, _ = json.Marshal(domain.RuntimeSyncPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
			Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
			ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
			DesiredStatus: instance.Status, DashboardPort: instance.DashboardPort,
		})
	case "reconcile-image":
		if instance.Status != domain.InstanceStopped {
			writeError(w, http.StatusConflict, "only a stopped instance can reconcile image metadata")
			return
		}
		if request.ConfirmName != instance.Name {
			writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
			return
		}
		if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
			writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
			return
		}
		host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
		if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
			writeError(w, http.StatusConflict, "the instance Host Agent is offline")
			return
		}
		if host.AgentVersion != agentVersion {
			writeError(w, http.StatusConflict, "image reconciliation requires Host Agent "+agentVersion)
			return
		}
		jobType, nextStatus = "instance.image.reconcile", domain.InstanceReconciling
		payload, _ = json.Marshal(domain.ImageReconcilePayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, PreviousImageID: instance.ImageID,
			ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		})
	case "fix-image-drift":
		if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
			writeError(w, http.StatusConflict, "only a running or stopped instance can fix image drift")
			return
		}
		if request.ConfirmName != instance.Name {
			writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
			return
		}
		if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
			writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
			return
		}
		hasFreshDrift, observationErr := s.store.HasFreshObservationCheck(
			r.Context(), instance.ID, "image", domain.ObservationCheckDrift,
			time.Now().UTC().Add(-s.config.ObservationStaleAfter),
		)
		if observationErr != nil {
			writeError(w, http.StatusInternalServerError, "could not verify current diagnostics")
			return
		}
		if !hasFreshDrift {
			writeError(w, http.StatusConflict, "refresh diagnostics before running this fix")
			return
		}
		host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
		if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
			writeError(w, http.StatusConflict, "the instance Host Agent is offline")
			return
		}
		if host.AgentVersion != agentVersion {
			writeError(w, http.StatusConflict, "automatic image repair requires Host Agent "+agentVersion)
			return
		}
		jobType, nextStatus = "instance.image.repair", domain.InstanceReconciling
		payload, _ = json.Marshal(domain.ImageRepairPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, PreviousImageID: instance.ImageID,
			ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
			APIPort: instance.APIPort, Restart: instance.Status == domain.InstanceRunning,
		})
	case "delete":
		if request.ConfirmName != instance.Name {
			writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
			return
		}
		if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped && instance.Status != domain.InstanceFailed {
			writeError(w, http.StatusConflict, "only a running, stopped, or failed instance can be deleted")
			return
		}
		jobType, nextStatus = "instance.delete", domain.InstanceDeleting
	default:
		writeError(w, http.StatusBadRequest, "action must be retry, start, stop, repair-runtime, sync-runtime, reconcile-image, fix-image-drift, or delete")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create operation identity")
		return
	}
	if payload == nil {
		payload, _ = json.Marshal(domain.ActionPayload{
			InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, ImageID: instance.ImageID, Provider: instance.Provider, Model: instance.Model,
			Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
			APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, PreserveData: true,
		})
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, WorkflowID: request.WorkflowID, Actor: "FLEET_ADMIN",
		Type: strings.ToUpper(strings.ReplaceAll(request.Action, "-", "_")), Status: domain.OperationPending,
		Summary:  actionSummary(request.Action) + " " + instance.Name,
		Metadata: operationMetadata(map[string]any{"workflow_step": request.Action}), CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID, Type: jobType, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now}
	if err := s.store.QueueAction(r.Context(), instance.Status, nextStatus, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		writeError(w, http.StatusConflict, "instance action could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	if len(query) == 0 {
		operations, err := s.store.ListOperations(r.Context(), maximumOperationPageLimit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not list operations")
			return
		}
		writeJSON(w, http.StatusOK, operations)
		return
	}
	for name := range query {
		if name != "limit" && name != "cursor" {
			writeError(w, http.StatusBadRequest, "operations query only accepts limit and cursor")
			return
		}
	}
	limit := defaultOperationPageLimit
	if values, present := query["limit"]; present {
		if len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > maximumOperationPageLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
			return
		}
		limit = parsed
	}
	var cursor *store.OperationCursor
	if values, present := query["cursor"]; present {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "cursor is invalid")
			return
		}
		decoded, err := decodeOperationCursor(values[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor is invalid")
			return
		}
		cursor = decoded
	}
	page, err := s.store.ListOperationsPage(r.Context(), limit, cursor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list operations")
		return
	}
	if page.Items == nil {
		page.Items = []domain.Operation{}
	}
	response := operationPageResponse{Items: page.Items}
	if page.NextCursor != nil {
		response.NextCursor = encodeOperationCursor(*page.NextCursor)
	}
	writeJSON(w, http.StatusOK, response)
}

type operationPageResponse struct {
	Items      []domain.Operation `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type operationCursorPayload struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func encodeOperationCursor(cursor store.OperationCursor) string {
	payload, _ := json.Marshal(operationCursorPayload{
		CreatedAt: cursor.CreatedAt.UTC().Format(time.RFC3339Nano),
		ID:        cursor.ID,
	})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeOperationCursor(raw string) (*store.OperationCursor, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > maximumOperationCursorSize {
		return nil, errors.New("operation cursor is empty or oversized")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 || len(decoded) > maximumOperationCursorSize {
		return nil, errors.New("operation cursor encoding is invalid")
	}
	var payload operationCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("operation cursor payload is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("operation cursor contains trailing data")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil || createdAt.IsZero() || !observationIdentityPattern.MatchString(payload.ID) {
		return nil, errors.New("operation cursor fields are invalid")
	}
	return &store.OperationCursor{CreatedAt: createdAt.UTC(), ID: payload.ID}, nil
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	operation, err := s.store.GetOperation(r.Context(), r.PathValue("operationID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "operation not found")
		return
	}
	if err != nil {
		s.logger.Error("get operation", "error", err)
		writeError(w, http.StatusInternalServerError, "operation could not be loaded")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, operation)
}

func (s *Server) listRecoveryPoints(w http.ResponseWriter, r *http.Request) {
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	instanceID := r.PathValue("instanceID")
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	items, err := s.config.RecoveryPoints.List(r.Context(), instanceID)
	if err != nil {
		s.logger.Error("list recovery points", "instance_id", instanceID, "error", err)
		writeError(w, http.StatusInternalServerError, "instance backups could not be listed")
		return
	}
	if items == nil {
		items = []recovery.Metadata{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), r.PathValue("instanceID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	var request struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := decodeOptionalJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validWorkflowID(request.WorkflowID) {
		writeError(w, http.StatusBadRequest, "workflow identity is invalid")
		return
	}
	if instance.Status != domain.InstanceStopped {
		writeError(w, http.StatusConflict, "instance backups can only be created while the instance is stopped")
		return
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		writeError(w, http.StatusConflict, "instance runtime metadata is incomplete")
		return
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "instance backups require Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create recovery operation identity")
		return
	}
	metadata, err := s.config.RecoveryPoints.Reserve(r.Context(), recovery.Reservation{
		InstanceID: instance.ID, InstanceName: instance.Name, HostID: instance.HostID,
		OperationID: operationID, JobID: jobID, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning,
		ServiceTier: instance.ServiceTier, CodexConfigured: instance.CodexConfigured,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume,
		ManagedPath: instance.ManagedPath, AgentVersion: host.AgentVersion,
	})
	if err != nil {
		s.writeRecoveryError(w, "reserve", err)
		return
	}
	payload, _ := json.Marshal(domain.RecoveryPointPayload{
		RecoveryPointID: metadata.ID, InstanceID: instance.ID, Name: instance.Name, Image: instance.Image,
		ImageID: instance.ImageID, Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning,
		ServiceTier: instance.ServiceTier, CodexConfigured: instance.CodexConfigured,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume,
		ManagedPath: instance.ManagedPath, AgentVersion: host.AgentVersion, CreatedAt: metadata.CreatedAt,
		MaxBytes: s.config.MaxRecoveryPointBytes,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, WorkflowID: request.WorkflowID, Actor: "FLEET_ADMIN",
		Type: "RECOVERY_POINT", Status: domain.OperationPending,
		Summary: "Create backup " + instance.Name, CreatedAt: now, UpdatedAt: now,
		Metadata: operationMetadata(map[string]any{"workflow_step": "backup", "backup_id": metadata.ID}),
	}
	job := domain.Job{
		ID: jobID, OperationID: operationID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.recovery.create", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueAction(r.Context(), domain.InstanceStopped, domain.InstanceBackingUp, operation, job); err != nil {
		if abortErr := s.config.RecoveryPoints.Abort(metadata.ID, jobID); abortErr != nil {
			s.logger.Error("abort unqueued recovery point", "error", abortErr)
		}
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		writeError(w, http.StatusConflict, "instance state changed before the backup was queued")
		return
	}
	writeJSON(w, http.StatusAccepted, metadata)
}

func (s *Server) verifyRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	metadata, err := s.config.RecoveryPoints.Verify(r.Context(), r.PathValue("recoveryPointID"))
	if err != nil {
		s.writeRecoveryError(w, "verify", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, metadata)
}

type restoreRecoveryRequest struct {
	ConfirmName string `json:"confirm_name"`
}

func (s *Server) restoreRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	recoveryPointID := r.PathValue("recoveryPointID")
	unlock := s.recoveryPointLocks.lock(recoveryPointID)
	defer unlock()

	metadata, err := s.config.RecoveryPoints.Get(recoveryPointID)
	if err != nil {
		s.writeRecoveryError(w, "restore", err)
		return
	}
	if metadata.Status != recovery.StatusReady {
		writeError(w, http.StatusConflict, "only a verified instance backup can be restored")
		return
	}
	var request restoreRecoveryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	instance, err := s.store.GetInstance(r.Context(), metadata.InstanceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance backup owner was not found")
		return
	}
	if request.ConfirmName != instance.Name {
		writeError(w, http.StatusBadRequest, "instance name confirmation does not match")
		return
	}
	if instance.Status != domain.InstanceStopped {
		writeError(w, http.StatusConflict, "stop the instance before restoring a backup")
		return
	}
	if metadata.InstanceName != instance.Name || metadata.HostID != instance.HostID || metadata.ProjectName != instance.ProjectName ||
		metadata.DataVolume != instance.DataVolume || metadata.ManagedPath != instance.ManagedPath {
		writeError(w, http.StatusConflict, "backup ownership does not match the exact Fleet instance")
		return
	}
	host, hostErr := s.store.GetHost(r.Context(), instance.HostID)
	if hostErr != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "instance backup restore requires Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the backup restore identity")
		return
	}
	payloadData, _ := json.Marshal(domain.RecoveryRestorePayload{
		RecoveryPointID: metadata.ID, InstanceID: metadata.InstanceID, Name: metadata.InstanceName,
		Image: metadata.Image, ImageID: metadata.ImageID, Provider: metadata.Provider, Model: metadata.Model,
		Reasoning: metadata.Reasoning, ServiceTier: metadata.ServiceTier, CodexConfigured: metadata.CodexConfigured,
		ProjectName: metadata.ProjectName,
		DataVolume:  metadata.DataVolume, ManagedPath: metadata.ManagedPath, AgentVersion: metadata.AgentVersion,
		CreatedAt: metadata.CreatedAt, RecoverySHA256: metadata.SHA256, RecoverySizeBytes: metadata.SizeBytes,
		MaxBytes: s.config.MaxRecoveryPointBytes,
	})
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "RESTORE", Status: domain.OperationPending,
		Summary: "Restore " + metadata.Filename + " to " + instance.Name, CreatedAt: now, UpdatedAt: now,
		Metadata: operationMetadata(map[string]any{"backup_id": metadata.ID}),
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.recovery.restore", Status: domain.JobPending, Payload: payloadData, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueAction(r.Context(), domain.InstanceStopped, domain.InstanceRestoring, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		writeError(w, http.StatusConflict, "instance state changed before the backup restore was queued")
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) downloadRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("recoveryPointID")
	metadata, err := s.config.RecoveryPoints.Get(id)
	if err != nil {
		s.writeRecoveryError(w, "download", err)
		return
	}
	if metadata.Status != recovery.StatusReady {
		writeError(w, http.StatusConflict, "instance backup is not ready for download")
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Hour))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Filename})
	w.Header().Set("Content-Disposition", disposition)
	if _, err := s.config.RecoveryPoints.Stream(r.Context(), id, w); err != nil {
		s.logger.Error("stream recovery point", "recovery_point_id", id, "error", err)
	}
}

func (s *Server) deleteRecoveryPoint(w http.ResponseWriter, r *http.Request) {
	if s.config.RecoveryPoints == nil {
		writeError(w, http.StatusServiceUnavailable, "instance backup storage is not configured")
		return
	}
	var request struct {
		ConfirmFilename string `json:"confirm_filename"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	recoveryPointID := r.PathValue("recoveryPointID")
	unlock := s.recoveryPointLocks.lock(recoveryPointID)
	defer unlock()

	active, err := s.store.HasActiveRecoveryPointReference(r.Context(), recoveryPointID)
	if err != nil {
		s.logger.Error("check active instance backup references", "recovery_point_id", recoveryPointID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not verify whether the instance backup is in use")
		return
	}
	if active {
		writeError(w, http.StatusConflict, "instance backup is used by an active operation")
		return
	}
	if err := s.config.RecoveryPoints.Delete(r.Context(), recoveryPointID, request.ConfirmFilename); err != nil {
		s.writeRecoveryError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeRecoveryError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, recovery.ErrNotFound):
		writeError(w, http.StatusNotFound, "instance backup not found")
	case errors.Is(err, recovery.ErrConfirmation):
		writeError(w, http.StatusBadRequest, "instance backup filename confirmation does not match")
	case errors.Is(err, recovery.ErrBusy), errors.Is(err, recovery.ErrState):
		writeError(w, http.StatusConflict, "instance backup is busy or not in the required state")
	case errors.Is(err, recovery.ErrLimitReached):
		writeError(w, http.StatusConflict, "instance backup retention limit reached; delete one explicitly before creating another")
	case errors.Is(err, recovery.ErrIntegrity):
		writeError(w, http.StatusConflict, "instance backup integrity verification failed")
	default:
		s.logger.Error("recovery point operation failed", "operation", operation, "error", err)
		writeError(w, http.StatusInternalServerError, "instance backup operation failed")
	}
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "backup service is not configured")
		return
	}
	items, err := s.config.Backups.List(r.Context())
	if err != nil {
		s.logger.Error("list backups", "error", err)
		writeError(w, http.StatusInternalServerError, "could not list backups")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "backup service is not configured")
		return
	}
	metadata, err := s.config.Backups.Create(r.Context())
	if err != nil {
		s.logger.Error("create backup", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create backup")
		return
	}
	writeJSON(w, http.StatusCreated, metadata)
}

func (s *Server) verifyBackup(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "backup service is not configured")
		return
	}
	metadata, err := s.config.Backups.Verify(r.Context(), r.PathValue("backupID"))
	if err != nil {
		s.writeBackupError(w, "verify backup", err)
		return
	}
	writeJSON(w, http.StatusOK, metadata)
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "backup service is not configured")
		return
	}
	if _, err := s.config.Backups.Verify(r.Context(), r.PathValue("backupID")); err != nil {
		s.writeBackupError(w, "verify backup before download", err)
		return
	}
	metadata, file, err := s.config.Backups.Open(r.PathValue("backupID"))
	if err != nil {
		s.writeBackupError(w, "open backup download", err)
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Backup-SHA256", metadata.SHA256)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Filename}))
	http.ServeContent(w, r, metadata.Filename, metadata.CreatedAt, file)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "backup service is not configured")
		return
	}
	var request struct {
		ConfirmFilename string `json:"confirm_filename"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.config.Backups.Delete(r.Context(), r.PathValue("backupID"), request.ConfirmFilename); err != nil {
		s.writeBackupError(w, "delete backup", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeBackupError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, backup.ErrNotFound):
		writeError(w, http.StatusNotFound, "backup not found")
	case errors.Is(err, backup.ErrConfirmation):
		writeError(w, http.StatusBadRequest, "backup filename confirmation does not match")
	case errors.Is(err, backup.ErrIntegrity):
		writeError(w, http.StatusConflict, "backup integrity check failed")
	default:
		s.logger.Error(operation, "error", err)
		writeError(w, http.StatusInternalServerError, "backup operation failed")
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !security.Equal(bearerToken(r), s.config.AdminToken) {
			writeError(w, http.StatusUnauthorized, "admin authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostID := r.Header.Get("X-Fleet-Host-ID")
		if hostID == "" {
			writeError(w, http.StatusUnauthorized, "host identity required")
			return
		}
		expected, err := s.store.HostTokenHash(r.Context(), hostID)
		if err != nil || !security.Equal(security.HashToken(bearerToken(r)), expected) {
			writeError(w, http.StatusUnauthorized, "invalid host credential")
			return
		}
		next(w, r)
	}
}

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if s.config.WebDir == "" {
		writeError(w, http.StatusNotFound, "web interface is not installed")
		return
	}
	relativePath := strings.TrimPrefix(filepath.Clean("/"+r.URL.Path), "/")
	path := filepath.Join(s.config.WebDir, relativePath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		http.ServeFile(w, r, path)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.config.WebDir, "index.html"))
}

func validateCreateInstance(request *createInstanceRequest) error {
	if !instanceNamePattern.MatchString(request.Name) {
		return errors.New("instance name must match ^[a-z][a-z0-9-]{2,31}$")
	}
	if request.HostID == "" {
		return errors.New("host_id is required")
	}
	if (request.APIPort == 0) != (request.DashboardPort == 0) {
		return errors.New("api_port and dashboard_port must both be omitted for automatic allocation")
	}
	if request.APIPort != 0 || request.DashboardPort != 0 {
		if request.APIPort < 1024 || request.APIPort > 65535 || request.DashboardPort < 1024 || request.DashboardPort > 65535 {
			return errors.New("ports must be between 1024 and 65535")
		}
		if request.APIPort == request.DashboardPort {
			return errors.New("API and dashboard ports must be different")
		}
	}
	if err := providers.ValidateImageReference(request.Image); err != nil {
		return err
	}
	if err := providers.ValidateRuntimeOrPending(request.Provider, request.Model, request.Reasoning, request.ServiceTier); err != nil {
		return err
	}
	return nil
}

func validateObservations(observations []domain.InstanceObservation, now time.Time, staleAfter time.Duration) error {
	if len(observations) == 0 || len(observations) > 100 {
		return errors.New("observations must contain between 1 and 100 reports")
	}
	validStatuses := map[string]bool{
		domain.ObservationInSync: true, domain.ObservationDegraded: true,
		domain.ObservationMissing: true, domain.ObservationUnknown: true,
	}
	validCheckStatuses := map[string]bool{
		domain.ObservationCheckOK: true, domain.ObservationCheckDrift: true,
		domain.ObservationCheckMissing: true, domain.ObservationCheckUnknown: true,
	}
	validCheckNames := map[string]bool{
		"observation": true, "managed_path": true, "manifest": true, "environment": true,
		"workspace": true, "docker_daemon": true, "data_volume": true, "containers": true,
		"ownership": true, "image": true, "runtime": true, "health_endpoint": true, "runtime_configuration": true, "codex_auth": true,
	}
	seenInstances := make(map[string]bool, len(observations))
	for _, observation := range observations {
		if !observationIdentityPattern.MatchString(observation.InstanceID) || seenInstances[observation.InstanceID] {
			return errors.New("observation instance identity is invalid or duplicated")
		}
		seenInstances[observation.InstanceID] = true
		if _, err := time.Parse(time.RFC3339Nano, observation.TargetGeneration); err != nil {
			return errors.New("observation target generation is invalid")
		}
		if observation.RefreshRequestID != "" && !observationIdentityPattern.MatchString(observation.RefreshRequestID) {
			return errors.New("observation refresh request identity is invalid")
		}
		if !validStatuses[observation.Status] {
			return errors.New("observation status is invalid")
		}
		if strings.TrimSpace(observation.Summary) == "" || len(observation.Summary) > 256 {
			return errors.New("observation summary is required and must not exceed 256 bytes")
		}
		if observation.HermesVersion != "" && !hermesVersionPattern.MatchString(observation.HermesVersion) {
			return errors.New("observation Hermes version is invalid")
		}
		if observation.HermesSource != "" && !hermesSourcePattern.MatchString(observation.HermesSource) {
			return errors.New("observation Hermes source is invalid")
		}
		if len(observation.ModelCatalog) > 64 {
			return errors.New("observation model catalog exceeds 64 entries")
		}
		seenModels := make(map[string]bool, len(observation.ModelCatalog))
		for _, model := range observation.ModelCatalog {
			if seenModels[model] || providers.ValidateRuntime("openai-codex", model, "medium", "normal") != nil {
				return errors.New("observation model catalog is invalid or duplicated")
			}
			seenModels[model] = true
		}
		if observation.RecommendedModel != "" && !seenModels[observation.RecommendedModel] {
			return errors.New("observation recommended model is not in the model catalog")
		}
		if staleAfter <= 0 {
			staleAfter = 2 * time.Minute
		}
		if observation.ObservedAt.IsZero() || observation.ObservedAt.After(now.Add(2*time.Minute)) ||
			now.Sub(observation.ObservedAt) > staleAfter {
			return errors.New("observation timestamp is invalid")
		}
		if len(observation.Checks) == 0 || len(observation.Checks) > 16 {
			return errors.New("observation must contain between 1 and 16 checks")
		}
		seenChecks := make(map[string]bool, len(observation.Checks))
		for _, check := range observation.Checks {
			if !validCheckNames[check.Name] || seenChecks[check.Name] || !validCheckStatuses[check.Status] {
				return errors.New("observation check is invalid or duplicated")
			}
			seenChecks[check.Name] = true
			if strings.TrimSpace(check.Detail) == "" || len(check.Detail) > 512 {
				return errors.New("observation check detail is required and must not exceed 512 bytes")
			}
		}
	}
	return nil
}

func threeIDs() (string, string, string, error) {
	first, err := identity.New()
	if err != nil {
		return "", "", "", err
	}
	second, err := identity.New()
	if err != nil {
		return "", "", "", err
	}
	third, err := identity.New()
	return first, second, third, err
}

func twoIDs() (string, string, error) {
	first, err := identity.New()
	if err != nil {
		return "", "", err
	}
	second, err := identity.New()
	return first, second, err
}

func validWorkflowID(value string) bool {
	return value == "" || observationIdentityPattern.MatchString(value)
}

func operationMetadata(values map[string]any) json.RawMessage {
	encoded, err := json.Marshal(values)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func bearerToken(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}

func decodeJSON(r *http.Request, target any) error {
	body, err := readJSONBody(r)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON request: multiple values are not allowed")
	}
	return nil
}

func decodeOptionalJSON(r *http.Request, target any) error {
	body, err := readJSONBody(r)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON request: multiple values are not allowed")
	}
	return nil
}

func readJSONBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maximumJSONBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("invalid JSON request: read body: %w", err)
	}
	if len(body) > maximumJSONBodyBytes {
		return nil, fmt.Errorf("invalid JSON request: body exceeds %d bytes", maximumJSONBodyBytes)
	}
	return body, nil
}

func actionSummary(action string) string {
	if action == "" {
		return ""
	}
	if action == "repair-runtime" {
		return "Repair and verify runtime"
	}
	action = strings.ReplaceAll(action, "-", " ")
	return strings.ToUpper(action[:1]) + action[1:]
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) writeQueueAdmissionError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrQueueCapacity) {
		return false
	}
	writeError(w, http.StatusTooManyRequests, "the Host Agent work queue is full; retry after active work completes")
	return true
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	count, err := w.ResponseWriter.Write(data)
	w.bytes += count
	return count, err
}

func (w *responseRecorder) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID, err := identity.New()
		if err != nil {
			requestID = fmt.Sprintf("request-%d", started.UTC().UnixNano())
		}
		w.Header().Set("X-Request-ID", requestID)
		recorder := &responseRecorder{ResponseWriter: w}
		s.metrics.RequestStarted()
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		s.metrics.RequestCompleted(status, time.Since(started), mutation)
		if status == http.StatusTooManyRequests {
			s.metrics.QueueRejected()
		}
		if mutation && status >= 200 && status < 300 && !strings.HasSuffix(r.URL.Path, "/heartbeat") &&
			!strings.HasSuffix(r.URL.Path, "/claim") && !strings.HasSuffix(r.URL.Path, "/renew") {
			s.events.Publish(eventTypeForRequest(r), r.PathValue("instanceID"))
		}
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") && !strings.HasSuffix(r.URL.Path, "/claim") &&
			!strings.HasPrefix(r.URL.Path, "/api/v1/agent/observations") {
			s.logger.Info("http request", "request_id", requestID, "method", r.Method, "path", r.URL.Path,
				"status", status, "bytes", recorder.bytes, "duration", time.Since(started))
		}
	})
}

func eventTypeForRequest(r *http.Request) string {
	switch {
	case strings.Contains(r.URL.Path, "/observations"):
		return "instance.observed"
	case strings.Contains(r.URL.Path, "/jobs/"):
		return "operation.updated"
	case strings.Contains(r.URL.Path, "/remote-access"):
		return "remote-access.changed"
	case strings.Contains(r.URL.Path, "/instances/"):
		return "instance.changed"
	default:
		return "fleet.changed"
	}
}

func Shutdown(ctx context.Context, server *http.Server) error {
	return server.Shutdown(ctx)
}
