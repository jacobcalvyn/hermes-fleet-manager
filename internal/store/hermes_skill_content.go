package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) HermesSkillContentSnapshot(ctx context.Context, instanceID, profile, skillName string) (domain.HermesSkillContentSnapshot, error) {
	var snapshot domain.HermesSkillContentSnapshot
	err := s.db.QueryRowContext(ctx, `
SELECT instance_id, profile, skill_name, provenance, content, revision, observed_at
FROM hermes_skill_content_snapshots WHERE instance_id=? AND profile=? AND skill_name=?`,
		instanceID, profile, skillName).Scan(
		&snapshot.InstanceID, &snapshot.Profile, &snapshot.SkillName, &snapshot.Provenance,
		&snapshot.Content, &snapshot.Revision, &snapshot.ObservedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrNotFound
	}
	return snapshot, err
}
