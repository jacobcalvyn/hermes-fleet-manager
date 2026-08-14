package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerRecoveryVolumeRoundTrip(t *testing.T) {
	if os.Getenv("HERMES_FLEET_RECOVERY_INTEGRATION") != "1" {
		t.Skip("set HERMES_FLEET_RECOVERY_INTEGRATION=1 to run the Docker recovery round-trip")
	}
	image := strings.TrimSpace(os.Getenv("HERMES_FLEET_RECOVERY_TEST_IMAGE"))
	if image == "" {
		t.Fatal("HERMES_FLEET_RECOVERY_TEST_IMAGE must name a local image with sh and tar")
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("docker is required for the recovery round-trip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	p := &Provisioner{dockerPath: dockerPath}
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	sourceVolume := "hermes-fleet-recovery-test-source-" + suffix
	targetVolume := "hermes-fleet-recovery-test-target-" + suffix
	for _, volume := range []string{sourceVolume, targetVolume} {
		if output, createErr := p.docker(ctx, "volume", "create", "--label", "io.hermes-fleet.test=true", volume); createErr != nil {
			t.Fatalf("create test volume %s: %s", volume, safeCommandError(createErr, output))
		}
		volume := volume
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			_, _ = p.docker(cleanupCtx, "volume", "rm", "-f", volume)
		})
	}
	if output, seedErr := p.docker(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "sh",
		"-v", sourceVolume+":/source", image, "-c",
		"mkdir -p /source/memories && printf recovery-round-trip > /source/memories/state.txt",
	); seedErr != nil {
		t.Fatalf("seed source volume: %s", safeCommandError(seedErr, output))
	}
	archivePath := filepath.Join(t.TempDir(), "data-volume.tar")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	export := exec.CommandContext(ctx, dockerPath,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "tar",
		"-v", sourceVolume+":/source:ro", image, "-C", "/source", "-cf", "-", ".",
	)
	export.Stdout = archive
	export.Stderr = &stderr
	exportErr := export.Run()
	closeErr := archive.Close()
	if exportErr != nil {
		t.Fatalf("export source volume: %s", safeCommandError(exportErr, stderr.String()))
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := validateVolumeArchive(archivePath, 1<<20); err != nil {
		t.Fatalf("validate Docker volume archive: %v", err)
	}
	if err := p.importVolume(ctx, targetVolume, image, archivePath); err != nil {
		t.Fatal(err)
	}
	if output, verifyErr := p.docker(ctx,
		"run", "--rm", "--network", "none", "--read-only", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--user", "0:0", "--entrypoint", "sh",
		"-v", targetVolume+":/target:ro", image, "-c",
		"test \"$(cat /target/memories/state.txt)\" = recovery-round-trip",
	); verifyErr != nil {
		t.Fatalf("verify restored volume: %s", safeCommandError(verifyErr, output))
	}
}
