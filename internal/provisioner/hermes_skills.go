package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

type hermesSkillDocument struct {
	Name       string `json:"name"`
	Provenance string `json:"provenance"`
}

type hermesSkillContentDocument struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Path    string `json:"path"`
}

type hermesDashboardSkillListDocument struct {
	Skills []hermesSkillDocument `json:"skills"`
	Data   []hermesSkillDocument `json:"data"`
}

func (p *Provisioner) inspectHermesSkillContent(ctx context.Context, payload domain.HermesSkillContentInspectPayload) domain.JobResult {
	if err := domain.ValidateHermesSkillContentInspectPayload(&payload); err != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes skill content inspection request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return p.inspectHermesSkillContentWithSession(ctx, payload, session)
}

func (p *Provisioner) inspectHermesSkillContentWithSession(ctx context.Context, payload domain.HermesSkillContentInspectPayload, session *hermesProfileSession) domain.JobResult {
	query := "?name=" + url.QueryEscape(payload.SkillName) + "&profile=" + url.QueryEscape(payload.Profile)
	content, found, err := p.hermesSkillContent(ctx, payload.DashboardPort, session, query)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill content inspection failed: " + err.Error()}
	}
	if !found {
		return domain.JobResult{Success: false, Error: "Hermes skill does not exist"}
	}
	provenance, err := p.hermesSkillProvenance(ctx, payload.DashboardPort, session, payload.Profile, payload.SkillName)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill provenance inspection failed: " + err.Error()}
	}
	digest := sha256.Sum256([]byte(content))
	return domain.JobResult{
		Success: true, Message: "Hermes skill content inspected",
		HermesSkillContent: &domain.HermesSkillContentSnapshot{
			InstanceID: payload.InstanceID, SkillName: payload.SkillName, Profile: payload.Profile,
			Provenance: provenance, Content: content, Revision: hex.EncodeToString(digest[:]),
		},
	}
}

func (p *Provisioner) syncHermesSkill(ctx context.Context, payload domain.HermesSkillSyncPayload) domain.JobResult {
	if err := domain.ValidateHermesSkillSyncPayload(&payload); err != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes skill synchronization request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return p.syncHermesSkillWithSession(ctx, payload, session)
}

func (p *Provisioner) removeHermesSkill(ctx context.Context, payload domain.HermesSkillRemovePayload) domain.JobResult {
	if err := domain.ValidateHermesSkillRemovePayload(&payload); err != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes skill removal request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return p.removeHermesSkillWithSession(ctx, payload, session)
}

func (p *Provisioner) removeHermesSkillWithSession(ctx context.Context, payload domain.HermesSkillRemovePayload, session *hermesProfileSession) domain.JobResult {
	query := "?name=" + url.QueryEscape(payload.SkillName) + "&profile=" + url.QueryEscape(payload.Profile)
	document, found, err := p.hermesSkillDocument(ctx, payload.DashboardPort, session, query)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill removal preflight failed: " + err.Error()}
	}
	if !found {
		return domain.JobResult{Success: true, Message: "Fleet skill is already absent"}
	}
	if !strings.Contains(document.Content, domain.FleetSkillOwnershipMark) {
		return domain.JobResult{Success: false, Error: "Hermes skill is owned outside Fleet and will not be removed"}
	}
	provenance, err := p.hermesSkillProvenance(ctx, payload.DashboardPort, session, payload.Profile, payload.SkillName)
	if err != nil || provenance != "agent" {
		return domain.JobResult{Success: false, Error: "Hermes skill provenance does not permit Fleet removal"}
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill removal has an unsafe managed path"}
	}
	if _, err := p.compose(
		ctx, managedPath, payload.ProjectName, "exec", "-T", "hermes", "python", "-c",
		fleetSkillRemovalScript, payload.Profile, payload.SkillName, document.Path,
	); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill removal failed: " + err.Error()}
	}
	if _, err := p.compose(
		ctx, managedPath, payload.ProjectName, "up", "-d", "--no-deps", "--force-recreate", "--wait",
		"--wait-timeout", fmt.Sprintf("%d", int(hermesProfileGatewayRestartTimeout.Seconds())), "hermes",
	); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill was removed but the Hermes runtime could not be refreshed"}
	}
	freshSession, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill was removed but final verification could not authenticate"}
	}
	_, found, err = p.hermesSkillDocument(ctx, payload.DashboardPort, freshSession, query)
	if err != nil || found {
		return domain.JobResult{Success: false, Error: "Hermes skill removal could not be verified"}
	}
	return domain.JobResult{Success: true, Message: "Fleet skill removed from Hermes"}
}

const fleetSkillRemovalScript = `
import pathlib
import re
import shutil
import sys

profile, name, reported_path = sys.argv[1:4]
if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", profile) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", name):
    raise SystemExit("invalid skill reference")
skill_md = pathlib.Path(reported_path).resolve()
roots = [pathlib.Path("/data/skills").resolve(), pathlib.Path("/data/profiles") / profile / "skills"]
allowed = any(skill_md.is_relative_to(root.resolve()) for root in roots) and skill_md.name == "SKILL.md" and skill_md.parent.name == name
if not allowed or not skill_md.is_file():
    raise SystemExit("skill path is outside Fleet-removable roots")
content = skill_md.read_text(encoding="utf-8")
if "<!-- managed-by: hermes-fleet -->" not in content:
    raise SystemExit("skill is not Fleet-owned")
shutil.rmtree(skill_md.parent)
`

func (p *Provisioner) syncHermesSkillWithSession(ctx context.Context, payload domain.HermesSkillSyncPayload, session *hermesProfileSession) domain.JobResult {
	query := "?name=" + url.QueryEscape(payload.SkillName) + "&profile=" + url.QueryEscape(payload.Profile)
	current, exists, err := p.hermesSkillContent(ctx, payload.DashboardPort, session, query)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill synchronization preflight failed: " + err.Error()}
	}
	if exists {
		if current == payload.Content {
			return domain.JobResult{Success: true, Message: "Fleet skill is already synchronized"}
		}
		if !strings.Contains(current, domain.FleetSkillOwnershipMark) {
			return domain.JobResult{Success: false, Error: "Hermes skill is owned outside Fleet and will not be overwritten"}
		}
	}

	var method, requestPath string
	var body []byte
	if exists {
		method, requestPath = http.MethodPut, "/api/skills/content"
		body, err = json.Marshal(map[string]string{
			"name": payload.SkillName, "profile": payload.Profile, "content": payload.Content,
		})
	} else {
		method, requestPath = http.MethodPost, "/api/skills"
		body, err = json.Marshal(map[string]string{
			"name": payload.SkillName, "profile": payload.Profile, "category": payload.Category, "content": payload.Content,
		})
	}
	if err != nil {
		return domain.JobResult{Success: false, Error: "encode Hermes skill synchronization request"}
	}
	if _, err := p.hermesProfileRequest(ctx, payload.DashboardPort, session, method, requestPath, body); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill synchronization failed: " + err.Error()}
	}
	verified, found, err := p.hermesSkillContent(ctx, payload.DashboardPort, session, query)
	if err != nil || !found || verified != payload.Content {
		return domain.JobResult{Success: false, Error: "Hermes did not persist the Fleet skill exactly"}
	}
	return domain.JobResult{Success: true, Message: "Fleet skill synchronized"}
}

func (p *Provisioner) hermesSkillContent(ctx context.Context, port int, session *hermesProfileSession, query string) (string, bool, error) {
	document, found, err := p.hermesSkillDocument(ctx, port, session, query)
	return document.Content, found, err
}

func (p *Provisioner) hermesSkillDocument(ctx context.Context, port int, session *hermesProfileSession, query string) (hermesSkillContentDocument, bool, error) {
	response, err := p.hermesProfileHTTPRequest(ctx, port, session, http.MethodGet, "/api/skills/content"+query, nil)
	if err != nil {
		return hermesSkillContentDocument{}, false, err
	}
	if response.statusCode == http.StatusNotFound {
		return hermesSkillContentDocument{}, false, nil
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return hermesSkillContentDocument{}, false, fmt.Errorf("Hermes dashboard returned HTTP %d", response.statusCode)
	}
	var document hermesSkillContentDocument
	if err := json.Unmarshal(response.body, &document); err != nil || strings.TrimSpace(document.Name) == "" {
		return hermesSkillContentDocument{}, false, fmt.Errorf("Hermes returned an invalid skill document")
	}
	return document, true, nil
}

func (p *Provisioner) hermesSkillProvenance(ctx context.Context, port int, session *hermesProfileSession, profile, name string) (string, error) {
	response, err := p.hermesProfileHTTPRequest(ctx, port, session, http.MethodGet, "/api/skills?profile="+url.QueryEscape(profile), nil)
	if err != nil {
		return "", err
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return "", fmt.Errorf("Hermes dashboard returned HTTP %d", response.statusCode)
	}
	var direct []hermesSkillDocument
	if err := json.Unmarshal(response.body, &direct); err != nil || direct == nil {
		var wrapped hermesDashboardSkillListDocument
		if err := json.Unmarshal(response.body, &wrapped); err != nil {
			return "", fmt.Errorf("Hermes returned an invalid skill inventory")
		}
		direct = wrapped.Skills
		if direct == nil {
			direct = wrapped.Data
		}
	}
	for _, skill := range direct {
		if strings.TrimSpace(skill.Name) == name {
			return strings.TrimSpace(skill.Provenance), nil
		}
	}
	return "", fmt.Errorf("Hermes skill inventory did not contain the requested skill")
}
