package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	runtimeassets "github.com/jacobcalvyn/hermes-fleet-manager/runtime"
)

func TestClientReturnsThreeOfficialStableReleasesAndCachesThem(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/releases":
			fmt.Fprint(w, `[
  {"name":"Hermes Agent v0.18.1 (2026.7.7)","tag_name":"v2026.7.7","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7","published_at":"2026-07-08T01:15:00Z"},
  {"name":"Hermes Agent v0.18.2 (2026.7.7.2)","tag_name":"v2026.7.7.2","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7.2","published_at":"2026-07-08T03:11:00Z"},
  {"name":"","tag_name":"v0.18.0","html_url":"https://github.com/NousResearch/hermes-agent/releases/tag/v0.18.0","published_at":"2026-07-01T20:08:00Z"}
]`)
		case "/git/ref/tags/v2026.7.7.2":
			fmt.Fprint(w, `{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"commit"}}`)
		case "/git/ref/tags/v2026.7.7":
			fmt.Fprintf(w, `{"object":{"sha":"%s","type":"commit"}}`, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		case "/git/ref/tags/v0.18.0":
			fmt.Fprintf(w, `{"object":{"sha":"%s","type":"commit"}}`, "cccccccccccccccccccccccccccccccccccccccc")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClientForTests(server.Client(), server.URL, time.Hour, func() time.Time {
		return time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	})

	catalog, err := client.List(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(catalog.Releases); got != 3 {
		t.Fatalf("release count = %d, want 3", got)
	}
	if catalog.Releases[0].Version != "0.18.2" || catalog.Releases[0].Image != runtimeassets.ImageReference("0.18.2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("unexpected latest release: %+v", catalog.Releases[0])
	}
	if catalog.Releases[1].Version != "0.18.1" || catalog.Releases[2].Version != "0.18.0" ||
		catalog.Releases[2].Tag != "v0.18.0" {
		t.Fatalf("releases were not sorted before limiting or tag fallback failed: %+v", catalog.Releases)
	}
	requestCount := requests.Load()
	if _, err := client.List(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != requestCount {
		t.Fatalf("cached lookup made new requests: before=%d after=%d", requestCount, requests.Load())
	}
}

func TestLoadCatalogBindsLegacyPortableCacheWithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	portable := Catalog{
		Source:    officialSource,
		CheckedAt: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
		Releases: []Release{
			testRelease("0.18.2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Date(2026, 7, 8, 3, 11, 0, 0, time.UTC)),
			testRelease("0.18.1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", time.Date(2026, 7, 8, 1, 15, 0, 0, time.UTC)),
			testRelease("0.18.0", "cccccccccccccccccccccccccccccccccccccccc", time.Date(2026, 7, 1, 20, 8, 0, 0, time.UTC)),
		},
	}
	encoded, err := json.Marshal(portable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loaded.Releases[0].Image, runtimeassets.ImageReference(portable.Releases[0].Version, portable.Releases[0].Commit); got != want {
		t.Fatalf("bound image = %s, want %s", got, want)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(encoded) {
		t.Fatal("LoadCatalog modified the portable cache")
	}
	if got := PortableCatalog(loaded).Releases[0].Image; got != portable.Releases[0].Image {
		t.Fatalf("portable image = %s, want %s", got, portable.Releases[0].Image)
	}
}

func TestLoadPortableCatalogValidationParityFixtures(t *testing.T) {
	catalog := Catalog{
		Source:    officialSource,
		CheckedAt: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
		Releases: []Release{
			testRelease("0.18.2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Date(2026, 7, 8, 3, 11, 0, 0, time.UTC)),
			testRelease("0.18.1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", time.Date(2026, 7, 8, 1, 15, 0, 0, time.UTC)),
			testRelease("0.18.0", "cccccccccccccccccccccccccccccccccccccccc", time.Date(2026, 7, 1, 20, 8, 0, 0, time.UTC)),
		},
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	releases := decoded["releases"].([]any)

	dateOnly := cloneJSONFixture(t, decoded)
	dateOnly["checked_at"] = "2026-07-22"
	naive := cloneJSONFixture(t, decoded)
	naiveReleases := naive["releases"].([]any)
	naiveReleases[0].(map[string]any)["published_at"] = "2026-07-08T03:11:00"
	timestampTrailing := cloneJSONFixture(t, decoded)
	timestampTrailing["checked_at"] = "2026-07-22T00:00:00Z trailing"
	malformedTimestamp := cloneJSONFixture(t, decoded)
	malformedTimestampReleases := malformedTimestamp["releases"].([]any)
	malformedTimestampReleases[0].(map[string]any)["published_at"] = "2026-13-08T03:11:00Z"
	twoReleases := cloneJSONFixture(t, decoded)
	twoReleases["releases"] = releases[:2]

	fixtures := []struct {
		name      string
		data      []byte
		wantValid bool
	}{
		{name: "valid-minified", data: encoded, wantValid: true},
		{name: "valid-pretty", data: marshalJSONFixture(t, decoded, true), wantValid: true},
		{name: "date-only", data: marshalJSONFixture(t, dateOnly, false)},
		{name: "naive", data: marshalJSONFixture(t, naive, false)},
		{name: "timestamp-trailing", data: marshalJSONFixture(t, timestampTrailing, false)},
		{name: "malformed-timestamp", data: marshalJSONFixture(t, malformedTimestamp, false)},
		{name: "two-releases", data: marshalJSONFixture(t, twoReleases, false)},
		{name: "trailing", data: append(append([]byte(nil), encoded...), []byte(`{"unexpected":true}`)...)},
		{name: "oversized", data: make([]byte, maxResponseSize+1)},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fixture.name+".json")
			if err := os.WriteFile(path, fixture.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPortableCatalog(path)
			if fixture.wantValid && err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			if !fixture.wantValid && err == nil {
				t.Fatal("invalid fixture accepted")
			}
		})
	}
}

func cloneJSONFixture(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func marshalJSONFixture(t *testing.T, value map[string]any, pretty bool) []byte {
	t.Helper()
	var (
		encoded []byte
		err     error
	)
	if pretty {
		encoded, err = json.MarshalIndent(value, "", "  ")
	} else {
		encoded, err = json.Marshal(value)
	}
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestValidateCatalogRejectsForeignRuntimeSuffix(t *testing.T) {
	release := testRelease("0.18.2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now().UTC())
	release.Image += "-ffffffffffff"
	catalog := Catalog{Source: officialSource, CheckedAt: time.Now().UTC(), Releases: []Release{release}}
	if err := ValidateCatalog(catalog, 1); err == nil {
		t.Fatal("expected foreign runtime suffix to be rejected")
	}
}

func TestFindByRuntimeImageRequiresKnownReleaseIdentity(t *testing.T) {
	release := testRelease("0.18.2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Now().UTC())
	catalog := Catalog{Releases: []Release{release}}
	for _, image := range []string{
		release.Image,
		release.Image + "-bbbbbbbbbbbb",
	} {
		resolved, ok := FindByRuntimeImage(catalog, image)
		if !ok || resolved.Version != release.Version || resolved.Commit != release.Commit {
			t.Fatalf("FindByRuntimeImage(%q) = %+v, %v", image, resolved, ok)
		}
	}
	for _, image := range []string{
		"local/hermes-fleet-runtime:0.18.2",
		"local/hermes-fleet-runtime:0.18.2-cccccccccccc",
		"local/hermes-fleet-runtime:0.18.2-aaaaaaaaaaaa-not-a-build",
		"registry.example/hermes-fleet-runtime:0.18.2-aaaaaaaaaaaa-bbbbbbbbbbbb",
	} {
		if resolved, ok := FindByRuntimeImage(catalog, image); ok {
			t.Fatalf("FindByRuntimeImage(%q) unexpectedly resolved %+v", image, resolved)
		}
	}
}

func TestClientRejectsNonSuccessResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()
	client := newClientForTests(server.Client(), server.URL, time.Hour, time.Now)
	if _, err := client.List(context.Background(), 3); err == nil {
		t.Fatal("expected release lookup to fail")
	}
}

func testRelease(version, commit string, publishedAt time.Time) Release {
	return Release{
		Version:     version,
		Tag:         "v" + version,
		Commit:      commit,
		Image:       "local/hermes-fleet-runtime:" + version + "-" + commit[:12],
		URL:         "https://github.com/NousResearch/hermes-agent/releases/tag/v" + version,
		PublishedAt: publishedAt,
	}
}

func TestValidateCatalogRejectsUntrustedProvenance(t *testing.T) {
	catalog := Catalog{
		Source: officialSource, CheckedAt: time.Now().UTC(),
		Releases: []Release{
			{Version: "0.18.2", Tag: "v2026.7.7.2", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Image: "local/hermes-fleet-runtime:0.18.2-aaaaaaaaaaaa", URL: "https://example.test/release", PublishedAt: time.Now().UTC()},
		},
	}
	if err := ValidateCatalog(catalog, 1); err == nil {
		t.Fatal("expected untrusted release URL to be rejected")
	}
}

type managedSourceStub struct {
	catalog Catalog
	err     error
}

func (source managedSourceStub) List(context.Context, int) (Catalog, error) {
	return source.catalog, source.err
}

func TestManagedSourcePublishesFreshCatalogAndFallsBackDurably(t *testing.T) {
	checkedAt := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	fallback := Catalog{Source: officialSource, CheckedAt: checkedAt.Add(-time.Hour), Releases: []Release{
		testRelease("0.20.0", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", checkedAt.Add(-time.Hour)),
		testRelease("0.19.1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", checkedAt.Add(-2*time.Hour)),
		testRelease("0.19.0", "cccccccccccccccccccccccccccccccccccccccc", checkedAt.Add(-3*time.Hour)),
	}}
	fresh := Catalog{Source: officialSource, CheckedAt: checkedAt, Releases: []Release{
		testRelease("0.21.0", "dddddddddddddddddddddddddddddddddddddddd", checkedAt),
		testRelease("0.20.0", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", checkedAt.Add(-time.Hour)),
		testRelease("0.19.1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", checkedAt.Add(-2*time.Hour)),
	}}
	cachePath := filepath.Join(t.TempDir(), "hermes-releases.json")
	source, err := NewManagedSource(managedSourceStub{catalog: fresh}, fallback, cachePath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := source.List(context.Background(), 3)
	if err != nil || catalog.Releases[0].Version != "0.21.0" || catalog.Releases[0].Image != runtimeassets.ImageReference("0.21.0", fresh.Releases[0].Commit) {
		t.Fatalf("fresh catalog=%+v error=%v", catalog, err)
	}
	persisted, err := LoadCatalog(cachePath)
	if err != nil || persisted.Releases[0].Version != "0.21.0" {
		t.Fatalf("persisted catalog=%+v error=%v", persisted, err)
	}
	source.primary = managedSourceStub{err: errors.New("offline")}
	catalog, err = source.List(context.Background(), 3)
	if err != nil || catalog.Releases[0].Version != "0.21.0" || !catalog.CheckedAt.Equal(checkedAt) || !catalog.Stale {
		t.Fatalf("fallback catalog=%+v error=%v", catalog, err)
	}
}
