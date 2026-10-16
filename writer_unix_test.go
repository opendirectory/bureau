//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExistUnreadableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can stat anything")
	}

	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)

	// used to panic with a nil FileInfo
	if exists, _ := FileExist(filepath.Join(dir, "conf")); exists {
		t.Errorf("expected unreadable file to be reported missing")
	}
}

func TestResolvePathUnix(t *testing.T) {
	got, err := ResolvePath("/etc//bureau/../bureau/x.conf")
	if err != nil || got != "/etc/bureau/x.conf" {
		t.Errorf("ResolvePath = %q, %v", got, err)
	}

	// %VAR% is only expanded on Windows
	t.Setenv("BUREAU_TEST", "nope")
	got, _ = ResolvePath("/tmp/%BUREAU_TEST%/x")
	if got != "/tmp/%BUREAU_TEST%/x" {
		t.Errorf("unexpected expansion: %q", got)
	}

	if pathKey("/Tmp/A") == pathKey("/tmp/a") {
		t.Errorf("paths must be case-sensitive outside Windows")
	}
}

func TestPublishFileSetsModeBeforeRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	publish(t, path, "0600", "s3cret")

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %o, want 600", info.Mode().Perm())
	}
}
