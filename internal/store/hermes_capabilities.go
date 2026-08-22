package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) HermesCapabilityInventory(ctx context.Context, instanceID string) (domain.HermesCapabilityInventory, error) {
	inventory := domain.HermesCapabilityInventory{
		InstanceID: instanceID,
		Features:   map[string]bool{},
		Skills:     []domain.HermesSkillCapability{},
		Toolsets:   []domain.HermesToolsetCapability{},
	}
	var encoded []byte
	err := s.db.QueryRowContext(ctx, `
SELECT inventory, observed_at FROM hermes_capability_inventories WHERE instance_id=?`, instanceID).
		Scan(&encoded, &inventory.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, instanceErr := s.GetInstance(ctx, instanceID); instanceErr != nil {
			return inventory, instanceErr
		}
		return inventory, nil
	}
	if err != nil {
		return inventory, err
	}
	observedAt := inventory.ObservedAt
	if err := json.Unmarshal(encoded, &inventory); err != nil {
		return inventory, fmt.Errorf("decode Hermes capability inventory: %w", err)
	}
	inventory.InstanceID = instanceID
	inventory.ObservedAt = observedAt
	if inventory.Features == nil {
		inventory.Features = map[string]bool{}
	}
	if inventory.Skills == nil {
		inventory.Skills = []domain.HermesSkillCapability{}
	}
	if inventory.Toolsets == nil {
		inventory.Toolsets = []domain.HermesToolsetCapability{}
	}
	return inventory, nil
}
