package provisioner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestSyncHermesSkillIsIdempotent(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	payload := validHermesSkillSyncPayload(t)
	requests := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/api/skills/content" ||
			request.URL.Query().Get("name") != payload.SkillName || request.URL.Query().Get("profile") != payload.Profile {
			t.Fatalf("unexpected skill request: %s %s", request.Method, request.URL.String())
		}
		body, err := json.Marshal(hermesSkillContentDocument{Name: payload.SkillName, Content: payload.Content})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	result := p.syncHermesSkillWithSession(context.Background(), payload, &hermesProfileSession{})
	if !result.Success || requests != 1 || result.Message != "Fleet skill is already synchronized" {
		t.Fatalf("result=%+v requests=%d", result, requests)
	}
}

func TestSyncHermesSkillRefusesUnownedCustomSkill(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	payload := validHermesSkillSyncPayload(t)
	p.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body, err := json.Marshal(hermesSkillContentDocument{
			Name: "browser-report", Content: "---\nname: browser-report\ndescription: User skill\n---\nKeep me",
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	result := p.syncHermesSkillWithSession(context.Background(), payload, &hermesProfileSession{})
	if result.Success || !strings.Contains(result.Error, "owned outside Fleet") {
		t.Fatalf("result=%+v", result)
	}
}

func TestRemoveHermesSkillRefusesUnownedCustomSkill(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	payload := domain.HermesSkillRemovePayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/managed/nara", DashboardPort: 19130,
		},
		SkillName: "browser-report", Profile: "default_profile",
	}
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/skills/content" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		body, marshalErr := json.Marshal(hermesSkillContentDocument{
			Name: "browser-report", Content: "---\nname: browser-report\ndescription: User skill\n---\nKeep me",
			Path: "/data/profiles/default_profile/skills/browser-report/SKILL.md",
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	result := p.removeHermesSkillWithSession(context.Background(), payload, &hermesProfileSession{})
	if result.Success || !strings.Contains(result.Error, "owned outside Fleet") {
		t.Fatalf("result=%+v", result)
	}
}

func TestInspectHermesSkillContentReturnsContentAndProvenance(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	content := "---\nname: browser-report\ndescription: Browser report\n---\nUse Chromium."
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		switch request.URL.Path {
		case "/api/skills/content":
			body, err = json.Marshal(hermesSkillContentDocument{Name: "browser-report", Content: content})
		case "/api/skills":
			body, err = json.Marshal([]hermesSkillDocument{{Name: "browser-report", Provenance: "agent"}})
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	payload := domain.HermesSkillContentInspectPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/managed/nara", DashboardPort: 19130,
		},
		SkillName: "browser-report", Profile: "default",
	}
	result := p.inspectHermesSkillContentWithSession(context.Background(), payload, &hermesProfileSession{})
	if !result.Success || result.HermesSkillContent == nil || result.HermesSkillContent.Content != content ||
		result.HermesSkillContent.Provenance != "agent" || len(result.HermesSkillContent.Revision) != 64 {
		t.Fatalf("result=%+v", result)
	}
}

func validHermesSkillSyncPayload(t *testing.T) domain.HermesSkillSyncPayload {
	t.Helper()
	skill := domain.FleetSkill{
		Name: "browser-report", Description: "Create a browser report",
		Content: "---\nname: browser-report\ndescription: Create a browser report\n---\nUse Chromium.",
	}
	if err := domain.ValidateFleetSkill(&skill); err != nil {
		t.Fatal(err)
	}
	return domain.HermesSkillSyncPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/tmp/fleet-nara", DashboardPort: 19130,
		},
		SkillName: skill.Name, Profile: "default", Content: skill.Content, Revision: skill.Revision,
	}
}
