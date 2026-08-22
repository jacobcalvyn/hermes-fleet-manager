package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/api"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/capacity"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflareoauth"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/reliability"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/remoteaccess"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/security"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config := loadConfig(logger)
	if err := os.MkdirAll(filepath.Dir(config.dbPath), 0o700); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	dataStore, err := store.Open(config.dbPath)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()
	backupManager, err := backup.New(config.backupDir, dataStore, config.backupRetention)
	if err != nil {
		logger.Error("configure backups", "error", err)
		os.Exit(1)
	}
	recoveryManager, err := recovery.New(config.recoveryDir, config.recoveryEncryptionKey, config.recoveryRetention, config.recoveryMaxBytes)
	if err != nil {
		logger.Error("configure instance recovery points", "error", err)
		os.Exit(1)
	}
	chatArtifactManager, err := chatartifacts.New(filepath.Join(filepath.Dir(config.dbPath), "chat-artifacts"), chatartifacts.Config{
		SessionMaxBytes: config.chatArtifactSessionMaxBytes, InstanceMaxBytes: config.chatArtifactInstanceMaxBytes,
		TotalMaxBytes: config.chatArtifactTotalMaxBytes, Retention: config.chatArtifactRetention,
	})
	if err != nil {
		logger.Error("configure chat artifact storage", "error", err)
		os.Exit(1)
	}
	sealer, err := security.NewSealer(config.secretEncryptionKey)
	if err != nil {
		logger.Error("configure secret encryption", "error", err)
		os.Exit(1)
	}
	releaseCatalog, err := releases.LoadCatalog(config.hermesReleaseCatalogPath)
	if err != nil {
		logger.Error("load bootstrap Hermes release catalog", "error", err)
		os.Exit(1)
	}
	releaseCachePath := filepath.Join(filepath.Dir(config.dbPath), "hermes-releases.json")
	if cachedCatalog, cacheErr := releases.LoadCatalog(releaseCachePath); cacheErr == nil && cachedCatalog.CheckedAt.After(releaseCatalog.CheckedAt) {
		releaseCatalog = cachedCatalog
	} else if cacheErr != nil && !errors.Is(cacheErr, os.ErrNotExist) {
		logger.Warn("ignore invalid durable Hermes release cache", "error", cacheErr)
	}
	releaseSource, err := releases.NewManagedSource(
		releases.NewClient(&http.Client{Timeout: 10 * time.Second}, time.Hour),
		releaseCatalog,
		releaseCachePath,
	)
	if err != nil {
		logger.Error("configure managed Hermes release catalog", "error", err)
		os.Exit(1)
	}
	reliabilityManager, err := reliability.New(filepath.Join(filepath.Dir(config.dbPath), "reliability"), backupManager, recoveryManager, dataStore.ListInstances)
	if err != nil {
		logger.Error("configure recovery drills", "error", err)
		os.Exit(1)
	}
	cloudflareAccess, err := cloudflare.New(config.cloudflare, dataStore.ListInstances, &http.Client{Timeout: 15 * time.Second}, logger)
	if err != nil {
		logger.Error("configure Cloudflare remote access", "error", err)
		os.Exit(1)
	}
	cloudflareAccess.SetOwnershipStore(dataStore)
	if !config.cloudflareOAuthManagedExternally {
		storedOAuthClient, readErr := dataStore.GetCloudflareOAuthClient(context.Background())
		if readErr == nil {
			config.cloudflareOAuth.ClientID = storedOAuthClient.ClientID
		} else if !errors.Is(readErr, store.ErrNotFound) {
			logger.Error("read stored Cloudflare OAuth client", "error", readErr)
			os.Exit(1)
		}
	}
	cloudflareOAuth, err := cloudflareoauth.New(config.cloudflareOAuth)
	if err != nil {
		logger.Error("configure Cloudflare OAuth", "error", err)
		os.Exit(1)
	}
	remoteAccess, err := remoteaccess.New(cloudflareAccess, dataStore.ListInstances)
	if err != nil {
		logger.Error("configure remote access", "error", err)
		os.Exit(1)
	}
	remoteAccessContext, stopRemoteAccess := context.WithCancel(context.Background())
	defer stopRemoteAccess()
	if !config.recoveryIsolated {
		remoteAccess.Start(remoteAccessContext)
		go func() {
			if record, recordErr := dataStore.GetRemoteAccessConfig(remoteAccessContext); recordErr == nil {
				opened, openErr := sealer.Open(record.Ciphertext, "system-cloudflare-remote-access:v1")
				var stored remoteaccess.Config
				if openErr != nil {
					logger.Error("decrypt stored remote access configuration", "error", openErr)
				} else if decoded, decodeErr := remoteaccess.DecodeConfig(opened); decodeErr != nil {
					logger.Error("decode stored remote access configuration", "error", decodeErr)
				} else {
					stored = decoded
					configureContext, cancel := context.WithTimeout(remoteAccessContext, 45*time.Second)
					configureErr := remoteAccess.Configure(configureContext, stored)
					cancel()
					if configureErr != nil {
						remoteAccess.RecordConfigurationFailure(stored, configureErr)
						logger.Error("restore remote access", "error", configureErr)
					}
				}
			} else if !errors.Is(recordErr, store.ErrNotFound) && !errors.Is(recordErr, context.Canceled) {
				logger.Error("read stored Cloudflare configuration", "error", recordErr)
			}
		}()
	} else {
		logger.Warn("clean-host recovery isolation is active; Cloudflare remote access is disabled")
		remoteAccess = nil
	}

	application := api.New(api.Config{
		AdminToken: config.adminToken, EnrollmentToken: config.enrollmentToken,
		Address: config.address, OperatorURL: config.operatorURL, DatabasePath: config.dbPath, BackupRetention: config.backupRetention,
		DataDirectory: filepath.Dir(config.dbPath), ReleaseCatalogPath: releaseCachePath, CapacityPolicy: config.capacityPolicy,
		HermesCatalog:       releaseCatalog,
		HermesReleaseSource: releaseSource,
		WebDir:              config.webDir, OfflineAfter: 30 * time.Second, Sealer: sealer,
		Backups: backupManager, RecoveryPoints: recoveryManager, ChatArtifacts: chatArtifactManager,
		MaxRecoveryPointBytes:                  config.recoveryMaxBytes,
		RemoteAccess:                           remoteAccess,
		CloudflareOAuth:                        cloudflareOAuth,
		CloudflareOAuthClientManagedExternally: config.cloudflareOAuthManagedExternally,
		Reliability:                            reliabilityManager,
	}, dataStore, logger)
	cloudflareAccess.SetConfigPersister(application.PersistCloudflareRuntimeConfig)
	maintenanceContext, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	application.Start(maintenanceContext)
	httpServer := &http.Server{
		Addr: config.address, Handler: application.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}

	go func() {
		logger.Info("control plane ready", "address", config.address)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control plane failed", "error", err)
			os.Exit(1)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	stopMaintenance()
	stopRemoteAccess()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := api.Shutdown(ctx, httpServer); err != nil {
		logger.Error("control plane shutdown", "error", err)
	}
}

type runtimeConfig struct {
	address                          string
	operatorURL                      string
	dbPath                           string
	adminToken                       string
	enrollmentToken                  string
	hermesReleaseCatalogPath         string
	webDir                           string
	secretEncryptionKey              string
	backupDir                        string
	backupRetention                  int
	recoveryEncryptionKey            string
	recoveryDir                      string
	recoveryRetention                int
	recoveryMaxBytes                 int64
	recoveryIsolated                 bool
	chatArtifactSessionMaxBytes      int64
	chatArtifactInstanceMaxBytes     int64
	chatArtifactTotalMaxBytes        int64
	chatArtifactRetention            time.Duration
	capacityPolicy                   capacity.Policy
	cloudflare                       cloudflare.Config
	cloudflareOAuth                  cloudflareoauth.Config
	cloudflareOAuthManagedExternally bool
}

func loadConfig(logger *slog.Logger) runtimeConfig {
	dbPath := envOr("FLEET_DATABASE_PATH", "/var/lib/hermes-fleet/fleet.db")
	config := runtimeConfig{
		address:                      envOr("FLEET_ADDRESS", "127.0.0.1:9180"),
		operatorURL:                  envOr("FLEET_CONTROL_PLANE_URL", "http://127.0.0.1:9180"),
		dbPath:                       dbPath,
		adminToken:                   os.Getenv("FLEET_ADMIN_TOKEN"),
		enrollmentToken:              os.Getenv("FLEET_ENROLLMENT_TOKEN"),
		hermesReleaseCatalogPath:     envOr("FLEET_HERMES_RELEASE_CATALOG_PATH", "/etc/hermes-fleet/hermes-releases.json"),
		webDir:                       envOr("FLEET_WEB_DIR", "/app/web"),
		secretEncryptionKey:          os.Getenv("FLEET_SECRET_ENCRYPTION_KEY"),
		backupDir:                    filepath.Join(filepath.Dir(dbPath), "backups"),
		backupRetention:              20,
		recoveryEncryptionKey:        os.Getenv("FLEET_RECOVERY_ENCRYPTION_KEY"),
		recoveryDir:                  filepath.Join(filepath.Dir(dbPath), "recovery-points"),
		recoveryRetention:            20,
		recoveryMaxBytes:             50 << 30,
		recoveryIsolated:             strings.EqualFold(strings.TrimSpace(os.Getenv("FLEET_RECOVERY_ISOLATED")), "true"),
		chatArtifactSessionMaxBytes:  chatartifacts.DefaultSessionMaxBytes,
		chatArtifactInstanceMaxBytes: chatartifacts.DefaultInstanceMaxBytes,
		chatArtifactTotalMaxBytes:    chatartifacts.DefaultTotalMaxBytes,
		chatArtifactRetention:        chatartifacts.DefaultRetention,
		capacityPolicy: capacity.Policy{
			MinimumFreeBytes: 1 << 30, MinimumFreePercent: 5, MinimumFreeInodes: 1000,
		},
		cloudflare: cloudflare.Config{
			CredentialsDirectory:         "/etc/hermes-fleet-cloudflare-credentials",
			AdminConnectorConfigPath:     "/var/lib/hermes-fleet-cloudflare/admin/config.yml",
			InstancesConnectorConfigPath: "/var/lib/hermes-fleet-cloudflare/instances/config.yml",
			AdminConnectorTokenPath:      "/var/lib/hermes-fleet-cloudflare/admin/token",
			InstancesConnectorTokenPath:  "/var/lib/hermes-fleet-cloudflare/instances/token",
			AdminConnectorHealthURL:      "http://cloudflare-admin:9081/healthz",
			InstancesConnectorHealthURL:  "http://cloudflare-instances:9081/healthz",
		},
	}
	redirectURL := strings.TrimSpace(os.Getenv("FLEET_CLOUDFLARE_OAUTH_REDIRECT_URL"))
	if redirectURL == "" {
		redirectURL = strings.TrimRight(config.operatorURL, "/") + "/api/v1/system/remote-access/cloudflare/oauth/callback"
	}
	scopes := strings.Fields(os.Getenv("FLEET_CLOUDFLARE_OAUTH_SCOPES"))
	if len(scopes) == 0 {
		scopes = cloudflareoauth.DefaultScopes()
	}
	clientID := strings.TrimSpace(os.Getenv("FLEET_CLOUDFLARE_OAUTH_CLIENT_ID"))
	config.cloudflareOAuth = cloudflareoauth.Config{ClientID: clientID, RedirectURL: redirectURL, Scopes: scopes}
	config.cloudflareOAuthManagedExternally = clientID != ""
	if value := os.Getenv("FLEET_BACKUP_RETENTION"); value != "" {
		retention, err := strconv.Atoi(value)
		if err != nil || retention < 1 || retention > 100 {
			logger.Error("FLEET_BACKUP_RETENTION must be an integer between 1 and 100")
			os.Exit(1)
		}
		config.backupRetention = retention
	}
	if value := os.Getenv("FLEET_RECOVERY_RETENTION"); value != "" {
		retention, err := strconv.Atoi(value)
		if err != nil || retention < 1 || retention > 100 {
			logger.Error("FLEET_RECOVERY_RETENTION must be an integer between 1 and 100")
			os.Exit(1)
		}
		config.recoveryRetention = retention
	}
	if value := os.Getenv("FLEET_RECOVERY_MAX_BYTES"); value != "" {
		maximum, err := strconv.ParseInt(value, 10, 64)
		if err != nil || maximum < 1 {
			logger.Error("FLEET_RECOVERY_MAX_BYTES must be a positive integer")
			os.Exit(1)
		}
		config.recoveryMaxBytes = maximum
	}
	artifactLimits := []struct {
		name   string
		target *int64
	}{
		{"FLEET_CHAT_ARTIFACT_SESSION_MAX_BYTES", &config.chatArtifactSessionMaxBytes},
		{"FLEET_CHAT_ARTIFACT_INSTANCE_MAX_BYTES", &config.chatArtifactInstanceMaxBytes},
		{"FLEET_CHAT_ARTIFACT_TOTAL_MAX_BYTES", &config.chatArtifactTotalMaxBytes},
	}
	for _, limit := range artifactLimits {
		if value := strings.TrimSpace(os.Getenv(limit.name)); value != "" {
			maximum, err := strconv.ParseInt(value, 10, 64)
			if err != nil || maximum < chatartifacts.MaximumBytes {
				logger.Error(limit.name + " must allow at least one 25 MiB artifact")
				os.Exit(1)
			}
			*limit.target = maximum
		}
	}
	if config.chatArtifactInstanceMaxBytes < config.chatArtifactSessionMaxBytes ||
		config.chatArtifactTotalMaxBytes < config.chatArtifactInstanceMaxBytes {
		logger.Error("chat artifact limits must increase from session to instance to Fleet total")
		os.Exit(1)
	}
	if value := strings.TrimSpace(os.Getenv("FLEET_CHAT_ARTIFACT_RETENTION_HOURS")); value != "" {
		hours, err := strconv.ParseInt(value, 10, 64)
		if err != nil || hours < 1 || hours > 24*3650 {
			logger.Error("FLEET_CHAT_ARTIFACT_RETENTION_HOURS must be between 1 and 87600")
			os.Exit(1)
		}
		config.chatArtifactRetention = time.Duration(hours) * time.Hour
	}
	if value := strings.TrimSpace(os.Getenv("FLEET_MIN_FREE_BYTES")); value != "" {
		minimum, err := strconv.ParseUint(value, 10, 64)
		if err != nil || minimum < 64<<20 {
			logger.Error("FLEET_MIN_FREE_BYTES must be an integer of at least 67108864")
			os.Exit(1)
		}
		config.capacityPolicy.MinimumFreeBytes = minimum
	}
	if value := strings.TrimSpace(os.Getenv("FLEET_MIN_FREE_PERCENT")); value != "" {
		minimum, err := strconv.ParseFloat(value, 64)
		if err != nil || minimum < 0 || minimum > 50 {
			logger.Error("FLEET_MIN_FREE_PERCENT must be between 0 and 50")
			os.Exit(1)
		}
		config.capacityPolicy.MinimumFreePercent = minimum
	}
	if value := strings.TrimSpace(os.Getenv("FLEET_MIN_FREE_INODES")); value != "" {
		minimum, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			logger.Error("FLEET_MIN_FREE_INODES must be a non-negative integer")
			os.Exit(1)
		}
		config.capacityPolicy.MinimumFreeInodes = minimum
	}
	if len(config.adminToken) < 32 || len(config.enrollmentToken) < 32 {
		logger.Error("FLEET_ADMIN_TOKEN and FLEET_ENROLLMENT_TOKEN must each contain at least 32 characters")
		os.Exit(1)
	}
	if len(config.secretEncryptionKey) != 64 {
		logger.Error("FLEET_SECRET_ENCRYPTION_KEY must contain exactly 64 hexadecimal characters")
		os.Exit(1)
	}
	if len(config.recoveryEncryptionKey) != 64 {
		logger.Error("FLEET_RECOVERY_ENCRYPTION_KEY must contain exactly 64 hexadecimal characters")
		os.Exit(1)
	}
	return config
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
