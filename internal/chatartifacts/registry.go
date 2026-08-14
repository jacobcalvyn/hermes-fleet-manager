package chatartifacts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Cursor struct {
	CreatedAt time.Time `json:"created_at"`
	SessionID string    `json:"session_id"`
	ID        string    `json:"id"`
}

type ListOptions struct {
	Limit        int
	Cursor       *Cursor
	Query        string
	InstanceID   string
	SessionID    string
	Status       string
	Kind         string
	CreatedAfter *time.Time
}

type Page struct {
	Items      []Metadata
	NextCursor *Cursor
}

type UsageSnapshot struct {
	TotalBytes       int64            `json:"total_bytes"`
	TotalMaxBytes    int64            `json:"total_max_bytes"`
	SessionMaxBytes  int64            `json:"session_max_bytes"`
	InstanceMaxBytes int64            `json:"instance_max_bytes"`
	RetentionHours   int              `json:"retention_hours"`
	StatusCounts     map[string]int   `json:"status_counts"`
	Instances        map[string]int64 `json:"instances"`
	Sessions         map[string]int64 `json:"sessions"`
}

// Record persists lifecycle metadata emitted by the Host Agent. Content is
// stored only by Put; a ready event without stored content is recorded missing.
func (m *Manager) Record(metadata Metadata) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	metadata.Status = strings.ToLower(strings.TrimSpace(metadata.Status))
	if metadata.Status == "" {
		metadata.Status = StatusPreparing
	}
	if metadata.Status == StatusDeleted || !ValidStatus(metadata.Status) {
		return Metadata{}, fmt.Errorf("%w: lifecycle status is invalid", ErrInvalid)
	}
	metadata.CreatedAt = metadata.CreatedAt.UTC()
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now().UTC()
	}
	if metadata.Status == StatusReady {
		metadata.Status = StatusMissing
		metadata.Error = "The output was reported ready but its content was not stored by Fleet."
	}
	if err := validateStoredMetadata(metadata); err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	directory, err := m.sessionDirectory(metadata.SessionID, true)
	if err != nil {
		return Metadata{}, err
	}
	_, metadataPath := m.paths(directory, metadata.ID)
	existing, _, err := m.getLocked(metadata.SessionID, metadata.ID, time.Now().UTC(), true)
	if err == nil {
		if !compatibleMetadata(existing, metadata) {
			return Metadata{}, fmt.Errorf("%w: identity already contains different content", ErrInvalid)
		}
		if existing.Status == StatusDeleted || existing.Status == StatusReady ||
			(metadata.Status == StatusPreparing && existing.Status != StatusPreparing) {
			return existing, nil
		}
		metadata.CreatedAt = existing.CreatedAt
		if metadata.MediaType == "" {
			metadata.MediaType = existing.MediaType
		}
		if metadata.SizeBytes == 0 {
			metadata.SizeBytes = existing.SizeBytes
		}
		if metadata.SHA256 == "" {
			metadata.SHA256 = existing.SHA256
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}
	if err := writeMetadata(metadataPath, metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) List(options ListOptions, now time.Time) (Page, error) {
	if options.Limit < 1 || options.Limit > 100 ||
		(options.InstanceID != "" && !ValidInstanceID(options.InstanceID)) ||
		(options.SessionID != "" && !ValidSessionID(options.SessionID)) ||
		(options.Status != "" && !ValidStatus(options.Status)) ||
		(options.Kind != "" && !ValidKind(options.Kind)) {
		return Page{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.reconcileLocked(now.UTC()); err != nil {
		return Page{}, err
	}
	items, err := m.allMetadataLocked()
	if err != nil {
		return Page{}, err
	}
	query := strings.ToLower(strings.TrimSpace(options.Query))
	filtered := items[:0]
	for _, item := range items {
		if options.InstanceID != "" && item.InstanceID != options.InstanceID ||
			options.SessionID != "" && item.SessionID != options.SessionID ||
			options.Status != "" && item.Status != options.Status ||
			options.Kind != "" && item.Kind != options.Kind ||
			options.CreatedAfter != nil && item.CreatedAt.Before(options.CreatedAfter.UTC()) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(strings.Join([]string{
			item.Name, item.MediaType, item.ID, item.OperationID, item.Error,
		}, " ")), query) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
		}
		if filtered[i].SessionID != filtered[j].SessionID {
			return filtered[i].SessionID > filtered[j].SessionID
		}
		return filtered[i].ID > filtered[j].ID
	})
	start := 0
	if options.Cursor != nil {
		start = -1
		for index, item := range filtered {
			if item.ID == options.Cursor.ID && item.SessionID == options.Cursor.SessionID && item.CreatedAt.Equal(options.Cursor.CreatedAt) {
				start = index + 1
				break
			}
		}
		if start < 0 {
			return Page{}, ErrInvalid
		}
	}
	end := min(len(filtered), start+options.Limit)
	page := Page{Items: append([]Metadata(nil), filtered[start:end]...)}
	if end < len(filtered) && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, SessionID: last.SessionID, ID: last.ID}
	}
	return page, nil
}

func (m *Manager) Usage(now time.Time) (UsageSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.reconcileLocked(now.UTC()); err != nil {
		return UsageSnapshot{}, err
	}
	items, err := m.allMetadataLocked()
	if err != nil {
		return UsageSnapshot{}, err
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Status]++
	}
	return UsageSnapshot{
		TotalBytes: m.usage.total, TotalMaxBytes: m.config.TotalMaxBytes,
		SessionMaxBytes: m.config.SessionMaxBytes, InstanceMaxBytes: m.config.InstanceMaxBytes,
		RetentionHours: int(m.config.Retention / time.Hour), StatusCounts: counts,
		Instances: cloneUsageMap(m.usage.instances), Sessions: cloneUsageMap(m.usage.sessions),
	}, nil
}

func (m *Manager) DeleteArtifact(artifactID string, now time.Time) (Metadata, error) {
	if !ValidArtifactID(artifactID) {
		return Metadata{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.reconcileLocked(now.UTC()); err != nil {
		return Metadata{}, err
	}
	items, err := m.allMetadataLocked()
	if err != nil {
		return Metadata{}, err
	}
	var match *Metadata
	for index := range items {
		if items[index].ID != artifactID {
			continue
		}
		if match != nil {
			return Metadata{}, errors.New("chat artifact identity is ambiguous")
		}
		copy := items[index]
		match = &copy
	}
	if match == nil {
		return Metadata{}, ErrNotFound
	}
	if match.Status == StatusDeleted {
		return *match, nil
	}
	directory, err := m.sessionDirectory(match.SessionID, false)
	if err != nil {
		return Metadata{}, err
	}
	dataPath, metadataPath := m.paths(directory, match.ID)
	if match.Status == StatusReady {
		m.removeUsageLocked(*match)
	}
	if err := os.Remove(dataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Metadata{}, err
	}
	deletedAt := now.UTC()
	match.Status = StatusDeleted
	match.Error = ""
	match.DeletedAt = &deletedAt
	if err := writeMetadata(metadataPath, *match); err != nil {
		return Metadata{}, err
	}
	return *match, nil
}

func (m *Manager) allMetadataLocked() ([]Metadata, error) {
	var items []Metadata
	sessions, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if !session.IsDir() || !ValidSessionID(session.Name()) {
			continue
		}
		directory := filepath.Join(m.root, session.Name())
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			metadata, _, err := m.readMetadata(filepath.Join(directory, entry.Name()))
			if err == nil && metadata.SessionID == session.Name() && metadata.ID == strings.TrimSuffix(entry.Name(), ".json") {
				items = append(items, metadata)
			}
		}
	}
	return items, nil
}

func cloneUsageMap(source map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
