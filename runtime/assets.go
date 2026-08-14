package runtimeassets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed Dockerfile
var dockerfile []byte

//go:embed entrypoint.sh
var entrypoint []byte

//go:embed configure.py
var configure []byte

// BuildID identifies the exact Fleet-owned runtime wrapper used for an image.
func BuildID() string {
	digest := sha256.New()
	for _, asset := range [][]byte{dockerfile, entrypoint, configure} {
		_, _ = digest.Write(asset)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// ImageReference binds a Hermes source release to the Fleet runtime wrapper.
// The suffix prevents a wrapper rebuild from moving an existing version and
// source-qualified tag to a different immutable image.
func ImageReference(version, commit string) string {
	version = strings.TrimSpace(version)
	commit = strings.ToLower(strings.TrimSpace(commit))
	buildID := BuildID()
	if len(commit) < 12 || len(buildID) < 12 {
		return ""
	}
	return fmt.Sprintf("local/hermes-fleet-runtime:%s-%s-%s", version, commit[:12], buildID[:12])
}

// WriteBuildContext creates the smallest build context needed for a Fleet-owned
// Hermes runtime image. The caller owns and must remove destination.
func WriteBuildContext(destination string) error {
	runtimeDir := filepath.Join(destination, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return fmt.Errorf("create runtime build context: %w", err)
	}
	for name, asset := range map[string][]byte{
		"Dockerfile":    dockerfile,
		"entrypoint.sh": entrypoint,
		"configure.py":  configure,
	} {
		mode := os.FileMode(0o600)
		if name == "entrypoint.sh" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, name), asset, mode); err != nil {
			return fmt.Errorf("write runtime build asset %s: %w", name, err)
		}
	}
	return nil
}
