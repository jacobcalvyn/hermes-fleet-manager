package runtimeassets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBuildIDMatchesSetupIdentityScript(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", filepath.Join(root, "scripts", "runtime-identity.sh"), "--root", root, "build-id")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime identity script failed: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != BuildID() {
		t.Fatalf("setup build ID = %s, embedded build ID = %s", got, BuildID())
	}
}

func TestBuildIDCoversEveryEmbeddedAssetInStableOrder(t *testing.T) {
	digest := sha256.New()
	for _, asset := range [][]byte{dockerfile, entrypoint, configure} {
		_, _ = digest.Write(asset)
	}
	if got, want := BuildID(), hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("BuildID() = %s, want %s", got, want)
	}
}

func TestBuildIDChangesWhenRuntimeAssetChanges(t *testing.T) {
	changedDockerfile := append([]byte(nil), dockerfile...)
	changedDockerfile = append(changedDockerfile, '\n')
	digest := sha256.New()
	for _, asset := range [][]byte{changedDockerfile, entrypoint, configure} {
		_, _ = digest.Write(asset)
	}
	if changed := hex.EncodeToString(digest.Sum(nil)); changed == BuildID() {
		t.Fatal("runtime asset mutation did not change the build identity")
	}
}

func TestDockerfileDeclaresRuntimeConfigSchemaV2(t *testing.T) {
	if !strings.Contains(
		string(dockerfile),
		`io.hermes-fleet.runtime-config-schema="2"`,
	) {
		t.Fatal("runtime image does not advertise readiness schema 2")
	}
}

func TestDockerfileProvidesDeterministicBrowserRuntime(t *testing.T) {
	contents := string(dockerfile)
	for _, fragment := range []string{
		`ln -s "$browser_path" /usr/local/bin/hermes-chromium`,
		`HERMES_FLEET_BROWSER_REQUIRED=true`,
		`AGENT_BROWSER_EXECUTABLE_PATH=/usr/local/bin/hermes-chromium`,
		`io.hermes-fleet.browser-runtime="playwright-chromium.v1"`,
	} {
		if !strings.Contains(contents, fragment) {
			t.Fatalf("runtime Dockerfile is missing browser runtime declaration %q", fragment)
		}
	}
}

func TestDockerfilePinsImmutableNodeBase(t *testing.T) {
	contents := string(dockerfile)
	if !regexp.MustCompile(`(?m)^FROM node:22-bookworm-slim@sha256:[a-f0-9]{64}$`).MatchString(contents) {
		t.Fatal("runtime Dockerfile does not pin node:22-bookworm-slim by digest")
	}
	if strings.Contains(contents, "FROM node:22-bookworm-slim\n") {
		t.Fatal("runtime Dockerfile still uses a floating node:22-bookworm-slim tag")
	}
}

func TestDockerfileInstallsUvSeparatelyWithRetry(t *testing.T) {
	contents := string(dockerfile)
	aptIdx := strings.Index(contents, "RUN apt-get")
	if aptIdx < 0 {
		t.Fatal("runtime Dockerfile is missing the apt-get layer")
	}
	aptBlock := contents[aptIdx:]
	if next := strings.Index(aptBlock[4:], "\nRUN "); next >= 0 {
		aptBlock = aptBlock[:next+4]
	}
	if strings.Contains(aptBlock, "pip install") {
		t.Fatal("uv install still shares the apt-get layer")
	}
	for _, fragment := range []string{
		`until python3 -m pip install --break-system-packages --no-cache-dir uv`,
		`if [ "$n" -ge 3 ]; then exit 1; fi`,
	} {
		if !strings.Contains(contents, fragment) {
			t.Fatalf("runtime Dockerfile is missing uv install retry %q", fragment)
		}
	}
}

func TestDockerfileFetchesPinnedHermesCommit(t *testing.T) {
	contents := string(dockerfile)
	if strings.Contains(contents, `git clone --depth 1 "${HERMES_REPO}"`) {
		t.Fatal("runtime Dockerfile clones the default branch instead of the pinned commit")
	}
	for _, fragment := range []string{
		`fetch --depth 1 origin "${HERMES_REF}"`,
		`until GIT_TERMINAL_PROMPT=0 git -C /opt/hermes-agent fetch --depth 1 origin "${HERMES_REF}"`,
		`if [ "$n" -ge 3 ]; then exit 1; fi`,
		`checkout --detach FETCH_HEAD`,
		`test "$(git -C /opt/hermes-agent rev-parse HEAD)" = "${HERMES_REF}"`,
	} {
		if !strings.Contains(contents, fragment) {
			t.Fatalf("runtime Dockerfile is missing pinned checkout %q", fragment)
		}
	}
}

func TestWriteBuildContextPreservesAssetsAndModes(t *testing.T) {
	destination := t.TempDir()
	if err := WriteBuildContext(destination); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{
		"Dockerfile": dockerfile, "entrypoint.sh": entrypoint, "configure.py": configure,
	} {
		path := filepath.Join(destination, "runtime", name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s differs from embedded asset", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		wantMode := os.FileMode(0o600)
		if name == "entrypoint.sh" {
			wantMode = 0o700
		}
		if gotMode := info.Mode().Perm(); gotMode != wantMode {
			t.Fatalf("%s mode = %o, want %o", name, gotMode, wantMode)
		}
	}
}

func TestImageReferenceIncludesRuntimeIdentity(t *testing.T) {
	const commit = "ABCDEF0123456789abcdef0123456789abcdef01"
	got := ImageReference("0.19.0", commit)
	want := "local/hermes-fleet-runtime:0.19.0-abcdef012345-" + BuildID()[:12]
	if got != want {
		t.Fatalf("ImageReference() = %s, want %s", got, want)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", filepath.Join(root, "scripts", "runtime-identity.sh"), "--root", root, "image", "0.19.0", commit)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime identity script failed: %v\n%s", err, output)
	}
	if shellReference := strings.TrimSpace(string(output)); shellReference != got {
		t.Fatalf("setup image reference = %s, embedded image reference = %s", shellReference, got)
	}
}
