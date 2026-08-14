package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) CreateCampaign(ctx context.Context, operation domain.Operation, instanceIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, instanceID := range instanceIDs {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM instances WHERE id=?`, instanceID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("campaign contains an unavailable instance: %s", instanceID)
			}
			return fmt.Errorf("inspect campaign instance: %w", err)
		}
		if status == domain.InstanceDeleted || status == domain.InstanceDeleting {
			return fmt.Errorf("campaign contains an unavailable instance: %s", instanceID)
		}
		var active int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM campaign_targets t
JOIN operations o ON o.id=t.campaign_id
WHERE t.instance_id=? AND o.type=? AND o.status IN (?, ?)`, instanceID, domain.CampaignOperationType,
			domain.OperationPending, domain.OperationRunning).Scan(&active); err != nil {
			return fmt.Errorf("inspect active campaign target: %w", err)
		}
		if active > 0 {
			return fmt.Errorf("instance already belongs to an active campaign: %s", instanceID)
		}
	}
	if err := insertControlPlaneOperation(ctx, tx, operation); err != nil {
		return err
	}
	for _, instanceID := range instanceIDs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO campaign_targets
  (campaign_id, instance_id, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, operation.ID, instanceID, domain.CampaignTargetPending,
			operation.CreatedAt, operation.UpdatedAt); err != nil {
			return fmt.Errorf("create campaign target: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) ListCampaignOperations(ctx context.Context, limit int, activeOnly bool) ([]domain.Operation, error) {
	if limit <= 0 {
		return []domain.Operation{}, nil
	}
	query := `SELECT id FROM operations WHERE type=?`
	args := []any{domain.CampaignOperationType}
	if activeOnly {
		query += ` AND status IN (?, ?)`
		args = append(args, domain.OperationPending, domain.OperationRunning)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := make([]domain.Operation, 0, len(ids))
	for _, id := range ids {
		operation, err := s.GetOperation(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, operation)
	}
	return items, nil
}

func (s *Store) ListCampaignTargets(ctx context.Context, campaignID string) ([]domain.CampaignTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.campaign_id, t.instance_id, i.name, t.request_id, t.status, t.detail,
       t.requested_at, t.completed_at, t.created_at, t.updated_at
FROM campaign_targets t
JOIN instances i ON i.id=t.instance_id
WHERE t.campaign_id=?
ORDER BY t.created_at, i.name, t.instance_id`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []domain.CampaignTarget{}
	for rows.Next() {
		var target domain.CampaignTarget
		var requestedAt, completedAt sql.NullTime
		if err := rows.Scan(&target.CampaignID, &target.InstanceID, &target.InstanceName, &target.RequestID,
			&target.Status, &target.Detail, &requestedAt, &completedAt, &target.CreatedAt, &target.UpdatedAt); err != nil {
			return nil, err
		}
		if requestedAt.Valid {
			value := requestedAt.Time
			target.RequestedAt = &value
		}
		if completedAt.Valid {
			value := completedAt.Time
			target.CompletedAt = &value
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) StartCampaignObservation(ctx context.Context, campaignID, instanceID, requestID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var targetStatus string
	if err := tx.QueryRowContext(ctx, `
SELECT status FROM campaign_targets WHERE campaign_id=? AND instance_id=?`, campaignID, instanceID).Scan(&targetStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if targetStatus != domain.CampaignTargetPending {
		return ErrStateChanged
	}
	var hostID, instanceStatus, projectName, dataVolume, managedPath string
	if err := tx.QueryRowContext(ctx, `
SELECT host_id, status, project_name, data_volume, managed_path FROM instances WHERE id=?`, instanceID).
		Scan(&hostID, &instanceStatus, &projectName, &dataVolume, &managedPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !isObservableInstanceStatus(instanceStatus) || projectName == "" || dataVolume == "" || managedPath == "" {
		return ErrObservationNotReady
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO observation_requests (instance_id, host_id, request_id, requested_at)
VALUES (?, ?, ?, ?)`, instanceID, hostID, requestID, at); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrObservationBusy
		}
		return fmt.Errorf("request campaign observation: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE campaign_targets
SET request_id=?, status=?, detail=?, requested_at=?, completed_at=NULL, updated_at=?
WHERE campaign_id=? AND instance_id=? AND status=?`, requestID, domain.CampaignTargetRunning,
		"Waiting for a fresh Host Agent observation", at, at, campaignID, instanceID, domain.CampaignTargetPending)
	if err != nil {
		return fmt.Errorf("start campaign target: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return tx.Commit()
}

func (s *Store) FinishCampaignTarget(ctx context.Context, campaignID, instanceID, requestID, status, detail string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if requestID != "" && status != domain.CampaignTargetSucceeded {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM observation_requests WHERE instance_id=? AND request_id=?`, instanceID, requestID); err != nil {
			return fmt.Errorf("clear failed campaign observation request: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE campaign_targets
SET status=?, detail=?, completed_at=?, updated_at=?
WHERE campaign_id=? AND instance_id=? AND status IN (?, ?)`, status, detail, at, at, campaignID, instanceID,
		domain.CampaignTargetPending, domain.CampaignTargetRunning)
	if err != nil {
		return fmt.Errorf("finish campaign target: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return tx.Commit()
}
