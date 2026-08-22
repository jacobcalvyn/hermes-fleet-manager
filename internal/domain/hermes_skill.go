package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	JobSyncHermesSkill           = "instance.skills.sync"
	JobRemoveHermesSkill         = "instance.skills.remove"
	JobInspectHermesSkillContent = "instance.skills.content.inspect"
	FleetSkillOwnershipMark      = "<!-- managed-by: hermes-fleet -->"
	FleetSkillOriginCreated      = "fleet_created"
	FleetSkillOriginInstance     = "copied_from_instance"
	MaximumFleetSkillBytes       = 100_000
)

var (
	hermesSkillNamePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hermesSkillCategoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

type FleetSkill struct {
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	Category           string    `json:"category,omitempty"`
	Content            string    `json:"content"`
	Revision           string    `json:"revision"`
	OriginType         string    `json:"origin_type"`
	SourceInstanceID   string    `json:"source_instance_id,omitempty"`
	SourceInstanceName string    `json:"source_instance_name,omitempty"`
	SourceProfile      string    `json:"source_profile,omitempty"`
	SourceRevision     string    `json:"source_revision,omitempty"`
	SourceProvenance   string    `json:"source_provenance,omitempty"`
	SourceObservedAt   time.Time `json:"source_observed_at,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type HermesSkillSyncPayload struct {
	HermesProfileInspectPayload
	SkillName string `json:"skill_name"`
	Category  string `json:"category,omitempty"`
	Profile   string `json:"profile"`
	Content   string `json:"content"`
	Revision  string `json:"revision"`
}

type HermesSkillRemovePayload struct {
	HermesProfileInspectPayload
	SkillName string `json:"skill_name"`
	Profile   string `json:"profile"`
}

type HermesSkillContentInspectPayload struct {
	HermesProfileInspectPayload
	SkillName string `json:"skill_name"`
	Profile   string `json:"profile"`
}

type HermesSkillContentSnapshot struct {
	InstanceID string    `json:"instance_id"`
	SkillName  string    `json:"skill_name"`
	Profile    string    `json:"profile"`
	Provenance string    `json:"provenance,omitempty"`
	Content    string    `json:"content"`
	Revision   string    `json:"revision"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

func ValidateFleetSkill(skill *FleetSkill) error {
	if skill == nil {
		return errors.New("Fleet skill is required")
	}
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
	skill.Category = strings.TrimSpace(skill.Category)
	skill.Content = strings.TrimSpace(skill.Content)
	skill.OriginType = strings.TrimSpace(skill.OriginType)
	if skill.OriginType == "" {
		skill.OriginType = FleetSkillOriginCreated
	}
	if !hermesSkillNamePattern.MatchString(skill.Name) || len(skill.Description) > 1000 ||
		(skill.Category != "" && !hermesSkillCategoryPattern.MatchString(skill.Category)) ||
		skill.Content == "" || len(skill.Content) > MaximumFleetSkillBytes || !utf8.ValidString(skill.Content) ||
		strings.ContainsRune(skill.Content, 0) {
		return errors.New("Fleet skill metadata or content is invalid")
	}
	if skill.OriginType != FleetSkillOriginCreated && skill.OriginType != FleetSkillOriginInstance {
		return errors.New("Fleet skill origin is invalid")
	}
	if skill.OriginType == FleetSkillOriginInstance {
		skill.SourceInstanceName = strings.TrimSpace(skill.SourceInstanceName)
		if strings.TrimSpace(skill.SourceInstanceID) == "" || skill.SourceInstanceName == "" || len(skill.SourceInstanceName) > 128 ||
			ValidateHermesProfileReference(skill.SourceProfile) != nil ||
			!isSHA256(skill.SourceRevision) || len(strings.TrimSpace(skill.SourceProvenance)) > 64 || skill.SourceObservedAt.IsZero() {
			return errors.New("Fleet skill source instance metadata is invalid")
		}
	} else {
		skill.SourceInstanceID, skill.SourceInstanceName, skill.SourceProfile, skill.SourceRevision, skill.SourceProvenance = "", "", "", "", ""
		skill.SourceObservedAt = time.Time{}
	}
	lines := strings.Split(skill.Content, "\n")
	if len(lines) < 4 || strings.TrimSpace(lines[0]) != "---" {
		return errors.New("Fleet skill content must start with YAML frontmatter")
	}
	frontmatterEnd := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			frontmatterEnd = index
			break
		}
	}
	if frontmatterEnd < 3 {
		return errors.New("Fleet skill YAML frontmatter is incomplete")
	}
	hasName, hasDescription := false, false
	for _, line := range lines[1:frontmatterEnd] {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			hasName = strings.Trim(strings.TrimSpace(value), `"'`) == skill.Name
		case "description":
			hasDescription = strings.TrimSpace(value) != ""
		}
	}
	if !hasName || !hasDescription {
		return errors.New("Fleet skill frontmatter must contain the matching name and a description")
	}
	if !strings.Contains(skill.Content, FleetSkillOwnershipMark) {
		lines = append(lines[:frontmatterEnd+1], append([]string{FleetSkillOwnershipMark}, lines[frontmatterEnd+1:]...)...)
		skill.Content = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	digest := sha256.Sum256([]byte(skill.Content))
	skill.Revision = hex.EncodeToString(digest[:])
	return nil
}

func ValidateHermesSkillRemovePayload(payload *HermesSkillRemovePayload) error {
	if payload == nil || payload.InstanceID == "" || payload.Name == "" || payload.ProjectName == "" ||
		payload.ManagedPath == "" || payload.DashboardPort < 1 || payload.DashboardPort > 65535 ||
		!hermesSkillNamePattern.MatchString(payload.SkillName) || ValidateHermesProfileReference(payload.Profile) != nil {
		return errors.New("Hermes skill removal payload is invalid")
	}
	return nil
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidateHermesSkillSyncPayload(payload *HermesSkillSyncPayload) error {
	if payload == nil || payload.InstanceID == "" || payload.Name == "" || payload.ProjectName == "" ||
		payload.ManagedPath == "" || payload.DashboardPort < 1 || payload.DashboardPort > 65535 ||
		!hermesSkillNamePattern.MatchString(payload.SkillName) ||
		(payload.Category != "" && !hermesSkillCategoryPattern.MatchString(payload.Category)) ||
		ValidateHermesProfileReference(payload.Profile) != nil || len(payload.Content) > MaximumFleetSkillBytes ||
		!utf8.ValidString(payload.Content) || !strings.Contains(payload.Content, FleetSkillOwnershipMark) {
		return errors.New("Hermes skill synchronization payload is invalid")
	}
	digest := sha256.Sum256([]byte(payload.Content))
	if payload.Revision != hex.EncodeToString(digest[:]) {
		return errors.New("Hermes skill synchronization revision is invalid")
	}
	return nil
}

func ValidateHermesSkillContentInspectPayload(payload *HermesSkillContentInspectPayload) error {
	if payload == nil || payload.InstanceID == "" || payload.Name == "" || payload.ProjectName == "" ||
		payload.ManagedPath == "" || payload.DashboardPort < 1 || payload.DashboardPort > 65535 ||
		!hermesSkillNamePattern.MatchString(payload.SkillName) || ValidateHermesProfileReference(payload.Profile) != nil {
		return errors.New("Hermes skill content inspection payload is invalid")
	}
	return nil
}

func ValidateHermesSkillContentSnapshot(snapshot *HermesSkillContentSnapshot) error {
	if snapshot == nil || snapshot.InstanceID == "" || !hermesSkillNamePattern.MatchString(snapshot.SkillName) ||
		ValidateHermesProfileReference(snapshot.Profile) != nil || len(snapshot.Provenance) > 64 ||
		snapshot.Content == "" || len(snapshot.Content) > MaximumFleetSkillBytes || !utf8.ValidString(snapshot.Content) ||
		strings.ContainsRune(snapshot.Content, 0) {
		return errors.New("Hermes skill content snapshot is invalid")
	}
	digest := sha256.Sum256([]byte(snapshot.Content))
	if snapshot.Revision != hex.EncodeToString(digest[:]) {
		return errors.New("Hermes skill content snapshot revision is invalid")
	}
	return nil
}

func FleetSkillFromContent(name, content string) (FleetSkill, error) {
	skill := FleetSkill{Name: strings.TrimSpace(name), Content: content}
	lines := strings.Split(content, "\n")
	if len(lines) < 4 || strings.TrimSpace(lines[0]) != "---" {
		return skill, errors.New("Hermes skill content must contain YAML frontmatter")
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "description":
			skill.Description = value
		case "category":
			skill.Category = value
		}
	}
	if err := ValidateFleetSkill(&skill); err != nil {
		return skill, err
	}
	return skill, nil
}
