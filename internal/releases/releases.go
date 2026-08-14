package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	runtimeassets "github.com/jacobcalvyn/hermes-fleet-manager/runtime"
)

const (
	officialAPIBase = "https://api.github.com/repos/NousResearch/hermes-agent"
	officialSource  = "NousResearch/hermes-agent GitHub Releases"
	maxResponseSize = 1 << 20
)

var (
	semanticVersionPattern = regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+\.[0-9]+\.[0-9]+)(?:[^0-9]|$)`)
	runtimeImagePattern    = regexp.MustCompile(`^local/hermes-fleet-runtime:([0-9]+\.[0-9]+\.[0-9]+)-([0-9a-fA-F]{12})(?:-([0-9a-fA-F]{12}))?$`)
)

type Release struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	Commit      string    `json:"commit"`
	Image       string    `json:"image"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

type Catalog struct {
	Source    string    `json:"source"`
	CheckedAt time.Time `json:"checked_at"`
	Releases  []Release `json:"releases"`
	Stale     bool      `json:"stale,omitempty"`
}

type Source interface {
	List(context.Context, int) (Catalog, error)
}

// ManagedSource keeps the official release feed and its durable last-known-good
// cache behind one Source. Callers never need to choose between a static setup
// catalog and the live GitHub feed.
type ManagedSource struct {
	primary   Source
	cachePath string

	refreshMu sync.Mutex
	mu        sync.RWMutex
	lastGood  Catalog
}

type Client struct {
	httpClient *http.Client
	apiBase    string
	now        func() time.Time
	ttl        time.Duration

	mu       sync.Mutex
	cached   Catalog
	cachedAt time.Time
}

func NewClient(httpClient *http.Client, ttl time.Duration) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Client{httpClient: httpClient, apiBase: officialAPIBase, now: time.Now, ttl: ttl}
}

func NewManagedSource(primary Source, lastGood Catalog, cachePath string) (*ManagedSource, error) {
	if primary == nil {
		return nil, errors.New("official Hermes release source is required")
	}
	if err := ValidateCatalog(lastGood, 3); err != nil {
		return nil, fmt.Errorf("validate last-known-good Hermes release catalog: %w", err)
	}
	if strings.TrimSpace(cachePath) == "" {
		return nil, errors.New("Hermes release cache path is required")
	}
	source := &ManagedSource{
		primary: primary, cachePath: cachePath, lastGood: BindRuntimeImages(lastGood),
	}
	if err := SavePortableCatalog(cachePath, source.lastGood); err != nil {
		return nil, fmt.Errorf("persist last-known-good Hermes release catalog: %w", err)
	}
	return source, nil
}

func (s *ManagedSource) List(ctx context.Context, limit int) (Catalog, error) {
	if limit < 1 || limit > 3 {
		return Catalog{}, errors.New("managed Hermes release limit must be between 1 and 3")
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	catalog, err := s.primary.List(ctx, 3)
	if err == nil {
		catalog = BindRuntimeImages(catalog)
		if validateErr := ValidateCatalog(catalog, 3); validateErr != nil {
			err = validateErr
		} else if persistErr := SavePortableCatalog(s.cachePath, catalog); persistErr != nil {
			err = persistErr
		} else {
			s.mu.Lock()
			s.lastGood = cloneCatalog(catalog, len(catalog.Releases))
			s.mu.Unlock()
			return cloneCatalog(catalog, limit), nil
		}
	}

	s.mu.RLock()
	fallback := cloneCatalog(s.lastGood, limit)
	s.mu.RUnlock()
	if len(fallback.Releases) == limit {
		fallback.Stale = true
		return fallback, nil
	}
	return Catalog{}, fmt.Errorf("refresh Hermes release catalog: %w", err)
}

func newClientForTests(httpClient *http.Client, apiBase string, ttl time.Duration, now func() time.Time) *Client {
	client := NewClient(httpClient, ttl)
	client.apiBase = strings.TrimRight(apiBase, "/")
	client.now = now
	return client
}

func (c *Client) List(ctx context.Context, limit int) (Catalog, error) {
	if limit < 1 || limit > 10 {
		return Catalog{}, errors.New("release limit must be between 1 and 10")
	}
	now := c.now().UTC()
	c.mu.Lock()
	if len(c.cached.Releases) >= limit && now.Sub(c.cachedAt) < c.ttl {
		catalog := cloneCatalog(c.cached, limit)
		c.mu.Unlock()
		return catalog, nil
	}
	c.mu.Unlock()

	catalog, err := c.fetch(ctx, limit, now)
	if err != nil {
		return Catalog{}, err
	}
	catalog = BindRuntimeImages(catalog)
	c.mu.Lock()
	c.cached = cloneCatalog(catalog, len(catalog.Releases))
	c.cachedAt = now
	c.mu.Unlock()
	return catalog, nil
}

func (c *Client) fetch(ctx context.Context, limit int, checkedAt time.Time) (Catalog, error) {
	var payload []struct {
		Name        string `json:"name"`
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := c.getJSON(ctx, c.apiBase+"/releases?per_page=10", &payload); err != nil {
		return Catalog{}, fmt.Errorf("fetch official Hermes releases: %w", err)
	}
	type candidate struct {
		version     string
		tag         string
		url         string
		publishedAt time.Time
	}
	candidatesByVersion := make(map[string]candidate, len(payload))
	for _, item := range payload {
		if item.Draft || item.Prerelease {
			continue
		}
		version := releaseVersion(item.Name)
		if version == "" {
			version = releaseTagVersion(item.TagName)
		}
		if version == "" {
			continue
		}
		publishedAt, err := time.Parse(time.RFC3339, item.PublishedAt)
		if err != nil {
			continue
		}
		candidate := candidate{
			version: version, tag: item.TagName, url: item.HTMLURL, publishedAt: publishedAt.UTC(),
		}
		existing, exists := candidatesByVersion[version]
		if !exists || candidate.publishedAt.After(existing.publishedAt) {
			candidatesByVersion[version] = candidate
		}
	}
	candidates := make([]candidate, 0, len(candidatesByVersion))
	for _, item := range candidatesByVersion {
		candidates = append(candidates, item)
	}
	sort.Slice(candidates, func(left, right int) bool {
		return compareVersions(candidates[left].version, candidates[right].version) > 0
	})
	if len(candidates) < limit {
		return Catalog{}, fmt.Errorf("official Hermes release feed returned %d stable semantic releases, need %d", len(candidates), limit)
	}
	catalog := Catalog{Source: officialSource, CheckedAt: checkedAt}
	for _, item := range candidates[:limit] {
		commit, err := c.resolveCommit(ctx, item.tag)
		if err != nil {
			return Catalog{}, fmt.Errorf("resolve Hermes %s source commit: %w", item.version, err)
		}
		catalog.Releases = append(catalog.Releases, Release{
			Version: item.version, Tag: item.tag, Commit: commit,
			Image: "local/hermes-fleet-runtime:" + item.version + "-" + commit[:12],
			URL:   item.url, PublishedAt: item.publishedAt,
		})
	}
	if err := validatePortableCatalog(catalog, limit); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c *Client) resolveCommit(ctx context.Context, tag string) (string, error) {
	var reference struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.getJSON(ctx, c.apiBase+"/git/ref/tags/"+url.PathEscape(tag), &reference); err != nil {
		return "", err
	}
	sha := reference.Object.SHA
	objectType := reference.Object.Type
	if objectType == "tag" {
		var annotated struct {
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if err := c.getJSON(ctx, c.apiBase+"/git/tags/"+sha, &annotated); err != nil {
			return "", err
		}
		sha = annotated.Object.SHA
		objectType = annotated.Object.Type
	}
	if objectType != "commit" || !isCommit(sha) {
		return "", errors.New("release tag does not resolve to a 40-character commit")
	}
	return strings.ToLower(sha), nil
}

func (c *Client) getJSON(ctx context.Context, url string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "hermes-fleet-manager")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("official release API returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseSize+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("official release API returned an oversized or malformed response")
	}
	return nil
}

func LoadCatalog(path string) (Catalog, error) {
	catalog, err := LoadPortableCatalog(path)
	if err != nil {
		return Catalog{}, err
	}
	return BindRuntimeImages(catalog), nil
}

// LoadPortableCatalog reads the on-disk release schema without binding it to a
// particular Fleet runtime wrapper. Keeping the cache portable allows an older
// Fleet binary to read it during rollback.
func LoadPortableCatalog(path string) (Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return Catalog{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Catalog{}, err
	}
	if info.Size() > maxResponseSize {
		return Catalog{}, errors.New("Hermes release catalog exceeds the size limit")
	}
	var catalog Catalog
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Catalog{}, errors.New("Hermes release catalog contains trailing or malformed data")
	}
	if err := validatePortableCatalog(catalog, 3); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// SavePortableCatalog atomically publishes a validated release cache without
// binding it to the current Fleet runtime wrapper.
func SavePortableCatalog(path string, catalog Catalog) error {
	portable := PortableCatalog(catalog)
	if err := validatePortableCatalog(portable, 3); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(portable, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Hermes release cache must not be a symbolic link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".hermes-releases-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func ValidateCatalog(catalog Catalog, expected int) error {
	if err := validateCatalogIdentity(catalog, expected); err != nil {
		return err
	}
	for index, release := range catalog.Releases {
		portable := portableImageReference(release)
		bound := runtimeassets.ImageReference(release.Version, release.Commit)
		if release.Image != portable && release.Image != bound {
			return fmt.Errorf("Hermes release catalog entry %d has an unexpected image", index)
		}
	}
	return nil
}

func validatePortableCatalog(catalog Catalog, expected int) error {
	if err := validateCatalogIdentity(catalog, expected); err != nil {
		return err
	}
	for index, release := range catalog.Releases {
		if release.Image != portableImageReference(release) {
			return fmt.Errorf("Hermes release catalog entry %d has an unexpected portable image", index)
		}
	}
	return nil
}

func validateCatalogIdentity(catalog Catalog, expected int) error {
	if catalog.Source != officialSource {
		return errors.New("Hermes release catalog source is not the official GitHub Releases feed")
	}
	if catalog.CheckedAt.IsZero() {
		return errors.New("Hermes release catalog has no checked_at timestamp")
	}
	if len(catalog.Releases) != expected {
		return fmt.Errorf("Hermes release catalog must contain exactly %d releases", expected)
	}
	seen := make(map[string]bool, len(catalog.Releases))
	for index, release := range catalog.Releases {
		if !isSemanticVersion(release.Version) || release.Tag == "" || !isCommit(release.Commit) {
			return fmt.Errorf("Hermes release catalog entry %d has an invalid identity", index)
		}
		if !strings.HasPrefix(release.URL, "https://github.com/NousResearch/hermes-agent/releases/") || release.PublishedAt.IsZero() {
			return fmt.Errorf("Hermes release catalog entry %d has invalid provenance", index)
		}
		if seen[release.Version] {
			return fmt.Errorf("Hermes release catalog repeats version %s", release.Version)
		}
		seen[release.Version] = true
		if index > 0 && compareVersions(catalog.Releases[index-1].Version, release.Version) <= 0 {
			return errors.New("Hermes release catalog must be ordered newest first")
		}
	}
	return nil
}

// BindRuntimeImages returns a copy whose image references include the current
// Fleet runtime wrapper identity. It never changes the portable on-disk cache.
func BindRuntimeImages(catalog Catalog) Catalog {
	bound := cloneCatalog(catalog, len(catalog.Releases))
	for index := range bound.Releases {
		bound.Releases[index].Image = runtimeassets.ImageReference(
			bound.Releases[index].Version,
			bound.Releases[index].Commit,
		)
	}
	return bound
}

// PortableCatalog removes the Fleet runtime suffix before serialization.
func PortableCatalog(catalog Catalog) Catalog {
	portable := cloneCatalog(catalog, len(catalog.Releases))
	portable.Stale = false
	for index := range portable.Releases {
		portable.Releases[index].Image = portableImageReference(portable.Releases[index])
	}
	return portable
}

func portableImageReference(release Release) string {
	if len(strings.TrimSpace(release.Commit)) < 12 {
		return ""
	}
	return "local/hermes-fleet-runtime:" + release.Version + "-" + strings.ToLower(release.Commit[:12])
}

func Find(catalog Catalog, version string) (Release, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	for _, release := range catalog.Releases {
		if release.Version == version {
			return release, true
		}
	}
	return Release{}, false
}

// FindByRuntimeImage resolves both portable release tags and wrapper-qualified
// Fleet runtime tags. It deliberately rejects arbitrary images and mismatched
// commit prefixes so callers never infer a Hermes version from an image name
// alone.
func FindByRuntimeImage(catalog Catalog, image string) (Release, bool) {
	match := runtimeImagePattern.FindStringSubmatch(strings.TrimSpace(image))
	if len(match) != 4 {
		return Release{}, false
	}
	release, ok := Find(catalog, match[1])
	if !ok || len(release.Commit) < 12 || !strings.EqualFold(match[2], release.Commit[:12]) {
		return Release{}, false
	}
	return release, true
}

func Compare(left, right string) int {
	return compareVersions(strings.TrimPrefix(left, "v"), strings.TrimPrefix(right, "v"))
}

func releaseVersion(name string) string {
	match := semanticVersionPattern.FindStringSubmatch(name)
	if len(match) != 2 || !isSemanticVersion(match[1]) {
		return ""
	}
	return match[1]
}

func releaseTagVersion(tag string) string {
	version := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if !isSemanticVersion(version) {
		return ""
	}
	return version
}

func isSemanticVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func compareVersions(left, right string) int {
	leftParts := numericParts(left)
	rightParts := numericParts(right)
	for index := 0; index < 3; index++ {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}

func numericParts(version string) [3]uint64 {
	var result [3]uint64
	parts := strings.Split(version, ".")
	for index := 0; index < len(parts) && index < len(result); index++ {
		result[index], _ = strconv.ParseUint(parts[index], 10, 32)
	}
	return result
}

func isCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func cloneCatalog(catalog Catalog, limit int) Catalog {
	copy := Catalog{Source: catalog.Source, CheckedAt: catalog.CheckedAt, Stale: catalog.Stale}
	limit = min(limit, len(catalog.Releases))
	copy.Releases = append([]Release(nil), catalog.Releases[:limit]...)
	return copy
}
