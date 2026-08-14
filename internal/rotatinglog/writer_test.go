package rotatinglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterRotatesAndRetainsBoundedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-agent.log")
	writer, err := Open(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"first\n", "second\n", "third\n", "fourth\n"} {
		if _, err := writer.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("stat %s: %v", suffix, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("unexpected mode for %s: %v", suffix, info.Mode())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected retention boundary, got %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "fourth") {
		t.Fatalf("current log does not contain the latest entry: %q", current)
	}
}

func TestWriterRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "host-agent.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 1024, 2); err == nil {
		t.Fatal("expected symlink rejection")
	}
}
