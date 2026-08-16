package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) HermesProfileInventory(ctx context.Context, instanceID string) (domain.HermesProfileInventory, error) {
	inventory := domain.HermesProfileInventory{InstanceID: instanceID, Profiles: []domain.HermesProfile{}}
	var encoded []byte
	err := s.db.QueryRowContext(ctx, `
SELECT profiles, observed_at FROM hermes_profile_inventories WHERE instance_id=?`, instanceID).
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
	if err := json.Unmarshal(encoded, &inventory.Profiles); err != nil {
		return inventory, fmt.Errorf("decode Hermes profile inventory: %w", err)
	}
	return inventory, nil
}
