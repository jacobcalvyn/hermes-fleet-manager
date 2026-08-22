package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) CreateFleetSkill(ctx context.Context, skill domain.FleetSkill) error {
	if err := domain.ValidateFleetSkill(&skill); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO fleet_skill_catalog (
  name, description, category, content, revision, origin_type, source_instance_id, source_instance_name,
  source_profile, source_revision, source_provenance, source_observed_at, created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, skill.Name, skill.Description, skill.Category, []byte(skill.Content),
		skill.Revision, skill.OriginType, skill.SourceInstanceID, skill.SourceInstanceName, skill.SourceProfile, skill.SourceRevision,
		skill.SourceProvenance, skill.SourceObservedAt, skill.CreatedAt, skill.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create Fleet skill: %w", err)
	}
	return nil
}

func (s *Store) UpdateFleetSkill(ctx context.Context, skill domain.FleetSkill) error {
	if err := domain.ValidateFleetSkill(&skill); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE fleet_skill_catalog SET
  description=?, category=?, content=?, revision=?, origin_type=?, source_instance_id=?, source_instance_name=?, source_profile=?,
  source_revision=?, source_provenance=?, source_observed_at=?, updated_at=?
WHERE name=?`,
		skill.Description, skill.Category, []byte(skill.Content), skill.Revision, skill.OriginType,
		skill.SourceInstanceID, skill.SourceInstanceName, skill.SourceProfile, skill.SourceRevision, skill.SourceProvenance,
		skill.SourceObservedAt, skill.UpdatedAt, skill.Name)
	if err != nil {
		return fmt.Errorf("update Fleet skill: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FleetSkill(ctx context.Context, name string) (domain.FleetSkill, error) {
	var skill domain.FleetSkill
	err := s.db.QueryRowContext(ctx, `
SELECT name, description, category, content, revision, origin_type, source_instance_id, source_instance_name, source_profile,
       source_revision, source_provenance, source_observed_at, created_at, updated_at
FROM fleet_skill_catalog WHERE name=?`, name).
		Scan(&skill.Name, &skill.Description, &skill.Category, &skill.Content, &skill.Revision,
			&skill.OriginType, &skill.SourceInstanceID, &skill.SourceInstanceName, &skill.SourceProfile, &skill.SourceRevision,
			&skill.SourceProvenance, &skill.SourceObservedAt, &skill.CreatedAt, &skill.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return skill, ErrNotFound
	}
	return skill, err
}

func (s *Store) ListFleetSkills(ctx context.Context) ([]domain.FleetSkill, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, description, category, content, revision, origin_type, source_instance_id, source_instance_name, source_profile,
       source_revision, source_provenance, source_observed_at, created_at, updated_at
FROM fleet_skill_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.FleetSkill, 0)
	for rows.Next() {
		var skill domain.FleetSkill
		if err := rows.Scan(&skill.Name, &skill.Description, &skill.Category, &skill.Content, &skill.Revision,
			&skill.OriginType, &skill.SourceInstanceID, &skill.SourceInstanceName, &skill.SourceProfile, &skill.SourceRevision,
			&skill.SourceProvenance, &skill.SourceObservedAt, &skill.CreatedAt, &skill.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, skill)
	}
	return items, rows.Err()
}

func (s *Store) DeleteFleetSkill(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM fleet_skill_catalog WHERE name=?`, name)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}
