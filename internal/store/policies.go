package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) CreatePolicy(ctx context.Context, policy domain.FleetPolicy) error {
	return s.writePolicy(ctx, policy, true)
}

func (s *Store) UpdatePolicy(ctx context.Context, policy domain.FleetPolicy) error {
	return s.writePolicy(ctx, policy, false)
}

func (s *Store) writePolicy(ctx context.Context, policy domain.FleetPolicy, create bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if create {
		_, err = tx.ExecContext(ctx, `
INSERT INTO fleet_policies (id, name, description, status, desired_hermes, strategy, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, policy.ID, policy.Name, policy.Description, policy.Status,
			policy.DesiredHermes, policy.Strategy, policy.CreatedAt, policy.UpdatedAt)
	} else {
		var active int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM operations
WHERE type='ROLLOUT_POLICY' AND status IN (?, ?) AND json_extract(metadata, '$.policy_id')=?`,
			domain.OperationPending, domain.OperationRunning, policy.ID).Scan(&active); err != nil {
			return fmt.Errorf("inspect active policy rollout: %w", err)
		}
		if active > 0 {
			return errors.New("policy cannot be changed while a rollout is active")
		}
		result, updateErr := tx.ExecContext(ctx, `
UPDATE fleet_policies SET name=?, description=?, status=?, desired_hermes=?, strategy=?, updated_at=? WHERE id=?`,
			policy.Name, policy.Description, policy.Status, policy.DesiredHermes, policy.Strategy, policy.UpdatedAt, policy.ID)
		err = updateErr
		if err == nil {
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrNotFound
			}
		}
	}
	if err != nil {
		return fmt.Errorf("write Fleet policy: %w", err)
	}
	if !create {
		if _, err := tx.ExecContext(ctx, `DELETE FROM fleet_policy_scope WHERE policy_id=?`, policy.ID); err != nil {
			return fmt.Errorf("replace Fleet policy scope: %w", err)
		}
	}
	for _, instanceID := range policy.ScopeInstanceIDs {
		result, err := tx.ExecContext(ctx, `
INSERT INTO fleet_policy_scope (policy_id, instance_id)
SELECT ?, id FROM instances WHERE id=? AND status<>?`, policy.ID, instanceID, domain.InstanceDeleted)
		if err != nil {
			return fmt.Errorf("write Fleet policy scope: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return fmt.Errorf("policy scope contains an unavailable instance: %s", instanceID)
		}
	}
	return tx.Commit()
}

func (s *Store) ListPolicies(ctx context.Context) ([]domain.FleetPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT p.id, p.name, p.description, p.status, p.desired_hermes, p.strategy,
       p.created_at, p.updated_at, COALESCE(s.instance_id, '')
FROM fleet_policies p
LEFT JOIN fleet_policy_scope s ON s.policy_id=p.id
ORDER BY p.created_at, p.id, s.instance_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.FleetPolicy, 0)
	indexes := make(map[string]int)
	for rows.Next() {
		var policy domain.FleetPolicy
		var instanceID string
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.Description, &policy.Status, &policy.DesiredHermes,
			&policy.Strategy, &policy.CreatedAt, &policy.UpdatedAt, &instanceID); err != nil {
			return nil, err
		}
		index, ok := indexes[policy.ID]
		if !ok {
			policy.ScopeInstanceIDs = []string{}
			items = append(items, policy)
			index = len(items) - 1
			indexes[policy.ID] = index
		}
		if instanceID != "" {
			items[index].ScopeInstanceIDs = append(items[index].ScopeInstanceIDs, instanceID)
		}
	}
	return items, rows.Err()
}

func (s *Store) GetPolicy(ctx context.Context, id string) (domain.FleetPolicy, error) {
	items, err := s.ListPolicies(ctx)
	if err != nil {
		return domain.FleetPolicy{}, err
	}
	for _, policy := range items {
		if policy.ID == id {
			return policy, nil
		}
	}
	return domain.FleetPolicy{}, ErrNotFound
}

func (s *Store) DeletePolicy(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
DELETE FROM fleet_policies
WHERE id=? AND NOT EXISTS (
  SELECT 1 FROM operations
  WHERE type='ROLLOUT_POLICY' AND status IN (?, ?) AND json_extract(metadata, '$.policy_id')=?
)`, id, domain.OperationPending, domain.OperationRunning, id)
	if err != nil {
		return fmt.Errorf("delete Fleet policy: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreatePolicyRollout(ctx context.Context, operation domain.Operation, policyID string, instanceIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM operations
WHERE type='ROLLOUT_POLICY' AND status IN (?, ?) AND json_extract(metadata, '$.policy_id')=?`,
		domain.OperationPending, domain.OperationRunning, policyID).Scan(&active); err != nil {
		return fmt.Errorf("inspect active policy rollout: %w", err)
	}
	if active > 0 {
		return errors.New("policy already has an active rollout")
	}
	if err := insertControlPlaneOperation(ctx, tx, operation); err != nil {
		return err
	}
	for _, instanceID := range instanceIDs {
		_, err := tx.ExecContext(ctx, `
INSERT INTO policy_rollout_targets
  (rollout_id, policy_id, instance_id, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`, operation.ID, policyID, instanceID, domain.PolicyTargetPending,
			operation.CreatedAt, operation.UpdatedAt)
		if err != nil {
			return fmt.Errorf("create policy rollout target: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) ListActivePolicyRollouts(ctx context.Context) ([]domain.Operation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM operations WHERE type='ROLLOUT_POLICY' AND status IN (?, ?) ORDER BY created_at`,
		domain.OperationPending, domain.OperationRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
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
	operations := make([]domain.Operation, 0, len(ids))
	for _, id := range ids {
		operation, err := s.GetOperation(ctx, id)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, nil
}

func (s *Store) ListPolicyRolloutTargets(ctx context.Context, rolloutID string) ([]domain.PolicyRolloutTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.rollout_id, t.policy_id, t.instance_id, i.name, COALESCE(t.child_operation_id, ''),
       t.status, t.detail, t.created_at, t.updated_at
FROM policy_rollout_targets t
JOIN instances i ON i.id=t.instance_id
WHERE t.rollout_id=? ORDER BY t.created_at, i.name, t.instance_id`, rolloutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []domain.PolicyRolloutTarget
	for rows.Next() {
		var target domain.PolicyRolloutTarget
		if err := rows.Scan(&target.RolloutID, &target.PolicyID, &target.InstanceID, &target.InstanceName, &target.ChildOperationID,
			&target.Status, &target.Detail, &target.CreatedAt, &target.UpdatedAt); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// UpdatePolicyRolloutControl persists operator control in operation metadata.
// expectedControl is a fencing token so concurrent pause, resume, and cancel
// requests cannot silently overwrite one another.
func (s *Store) UpdatePolicyRolloutControl(
	ctx context.Context,
	rolloutID string,
	expectedControl string,
	metadata []byte,
	progress domain.JobProgress,
	at time.Time,
) error {
	encodedProgress, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("encode policy rollout progress: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE operations
SET metadata=?, progress=?, updated_at=?
WHERE id=? AND type='ROLLOUT_POLICY' AND status IN (?, ?)
  AND COALESCE(json_extract(metadata, '$.control_state'), ?)=?`, metadata, encodedProgress, at,
		rolloutID, domain.OperationPending, domain.OperationRunning,
		domain.PolicyRolloutControlRunning, expectedControl)
	if err != nil {
		return fmt.Errorf("update policy rollout control: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return nil
}

func (s *Store) BlockPendingPolicyRolloutTargets(ctx context.Context, rolloutID, detail string, at time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE policy_rollout_targets
SET status=?, detail=?, updated_at=?
WHERE rollout_id=? AND status=?`, domain.PolicyTargetBlocked, detail, at, rolloutID, domain.PolicyTargetPending)
	if err != nil {
		return 0, fmt.Errorf("block pending policy rollout targets: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read blocked policy rollout target count: %w", err)
	}
	return count, nil
}

func (s *Store) UpdatePolicyRolloutTarget(ctx context.Context, rolloutID, instanceID, childOperationID, status, detail string, at time.Time) error {
	var childOperation any
	if childOperationID != "" {
		childOperation = childOperationID
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE policy_rollout_targets
SET child_operation_id=COALESCE(?, child_operation_id), status=?, detail=?, updated_at=?
WHERE rollout_id=? AND instance_id=?`, childOperation, status, detail, at, rolloutID, instanceID)
	if err != nil {
		return fmt.Errorf("update policy rollout target: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}
