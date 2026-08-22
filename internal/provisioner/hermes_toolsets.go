package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

type hermesDashboardToolset struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type hermesDashboardToolsetList struct {
	Toolsets []hermesDashboardToolset `json:"toolsets"`
	Data     []hermesDashboardToolset `json:"data"`
}

func (p *Provisioner) setHermesToolset(ctx context.Context, payload domain.HermesToolsetMutationPayload) domain.JobResult {
	if err := domain.ValidateHermesToolsetMutationPayload(&payload); err != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes toolset mutation request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return p.setHermesToolsetWithSession(ctx, payload, session)
}

func (p *Provisioner) setHermesToolsetWithSession(ctx context.Context, payload domain.HermesToolsetMutationPayload, session *hermesProfileSession) domain.JobResult {
	toolsets, err := p.hermesDashboardToolsets(ctx, payload.DashboardPort, session, payload.Profile)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes toolset mutation preflight failed: " + err.Error()}
	}
	current, found := dashboardToolsetByName(toolsets, payload.ToolsetName)
	if !found {
		return domain.JobResult{Success: false, Error: "Hermes toolset does not exist"}
	}
	if current.Enabled == payload.Enabled {
		return domain.JobResult{Success: true, Message: "Hermes toolset already has the requested state"}
	}
	body, err := json.Marshal(map[string]any{"profile": payload.Profile, "enabled": payload.Enabled})
	if err != nil {
		return domain.JobResult{Success: false, Error: "encode Hermes toolset mutation request"}
	}
	requestPath := "/api/tools/toolsets/" + url.PathEscape(payload.ToolsetName)
	if _, err := p.hermesProfileRequest(ctx, payload.DashboardPort, session, http.MethodPut, requestPath, body); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes toolset mutation failed: " + err.Error()}
	}
	toolsets, err = p.hermesDashboardToolsets(ctx, payload.DashboardPort, session, payload.Profile)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes toolset changed but verification failed"}
	}
	verified, found := dashboardToolsetByName(toolsets, payload.ToolsetName)
	if !found || verified.Enabled != payload.Enabled {
		return domain.JobResult{Success: false, Error: "Hermes did not persist the requested toolset state"}
	}
	return domain.JobResult{Success: true, Message: "Hermes toolset state updated"}
}

func (p *Provisioner) hermesDashboardToolsets(ctx context.Context, port int, session *hermesProfileSession, profile string) ([]hermesDashboardToolset, error) {
	response, err := p.hermesProfileHTTPRequest(ctx, port, session, http.MethodGet, "/api/tools/toolsets?profile="+url.QueryEscape(profile), nil)
	if err != nil {
		return nil, err
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return nil, fmt.Errorf("Hermes dashboard returned HTTP %d", response.statusCode)
	}
	var direct []hermesDashboardToolset
	if err := json.Unmarshal(response.body, &direct); err == nil && direct != nil {
		return direct, nil
	}
	var wrapped hermesDashboardToolsetList
	if err := json.Unmarshal(response.body, &wrapped); err != nil {
		return nil, fmt.Errorf("Hermes returned an invalid toolset inventory")
	}
	if wrapped.Toolsets != nil {
		return wrapped.Toolsets, nil
	}
	if wrapped.Data != nil {
		return wrapped.Data, nil
	}
	return nil, fmt.Errorf("Hermes returned an invalid toolset inventory")
}

func dashboardToolsetByName(toolsets []hermesDashboardToolset, name string) (hermesDashboardToolset, bool) {
	for _, toolset := range toolsets {
		if strings.TrimSpace(toolset.Name) == name {
			return toolset, true
		}
	}
	return hermesDashboardToolset{}, false
}
