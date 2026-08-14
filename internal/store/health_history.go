package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const fleetHealthIncidentRetention = 200

type FleetHealthState struct {
	Component     string     `json:"component"`
	Status        string     `json:"status"`
	Detail        string     `json:"detail"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
}

type FleetHealthIncident struct {
	ID             int64     `json:"id"`
	Component      string    `json:"component"`
	PreviousStatus string    `json:"previous_status,omitempty"`
	Status         string    `json:"status"`
	Detail         string    `json:"detail"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// RecordFleetHealth stores the latest component state and appends history only
// when the state changes. Repeated polling therefore remains bounded and does
// not turn the health monitor into an unbounded event stream.
func (s *Store) RecordFleetHealth(ctx context.Context, component, status, detail string, observedAt time.Time) error {
	if component == "" || (status != "healthy" && status != "degraded") {
		return errors.New("Fleet health requires a component and healthy or degraded status")
	}
	observedAt = observedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	previousStatus := ""
	var previousSuccess sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT status, last_success_at FROM fleet_health_state WHERE component=?`, component).Scan(&previousStatus, &previousSuccess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var lastSuccess any
	if status == "healthy" {
		lastSuccess = observedAt
	} else if previousSuccess.Valid {
		lastSuccess = previousSuccess.Time
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO fleet_health_state(component, status, detail, updated_at, last_success_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(component) DO UPDATE SET
  status=excluded.status,
  detail=excluded.detail,
  updated_at=excluded.updated_at,
  last_success_at=excluded.last_success_at`, component, status, detail, observedAt, lastSuccess); err != nil {
		return err
	}
	if previousStatus != status {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO fleet_health_incidents(component, previous_status, status, detail, occurred_at)
VALUES(?, ?, ?, ?, ?)`, component, previousStatus, status, detail, observedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM fleet_health_incidents
WHERE id NOT IN (
  SELECT id FROM fleet_health_incidents ORDER BY occurred_at DESC, id DESC LIMIT ?
)`, fleetHealthIncidentRetention); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListFleetHealthStates(ctx context.Context) ([]FleetHealthState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT component, status, detail, updated_at, last_success_at
FROM fleet_health_state ORDER BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []FleetHealthState{}
	for rows.Next() {
		var state FleetHealthState
		var lastSuccess sql.NullTime
		if err := rows.Scan(&state.Component, &state.Status, &state.Detail, &state.UpdatedAt, &lastSuccess); err != nil {
			return nil, err
		}
		if lastSuccess.Valid {
			value := lastSuccess.Time
			state.LastSuccessAt = &value
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (s *Store) ListFleetHealthIncidents(ctx context.Context, limit int) ([]FleetHealthIncident, error) {
	if limit < 1 || limit > fleetHealthIncidentRetention {
		return nil, fmt.Errorf("Fleet health incident limit must be between 1 and %d", fleetHealthIncidentRetention)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, component, previous_status, status, detail, occurred_at
FROM fleet_health_incidents
ORDER BY occurred_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	incidents := []FleetHealthIncident{}
	for rows.Next() {
		var incident FleetHealthIncident
		if err := rows.Scan(&incident.ID, &incident.Component, &incident.PreviousStatus, &incident.Status, &incident.Detail, &incident.OccurredAt); err != nil {
			return nil, err
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}
