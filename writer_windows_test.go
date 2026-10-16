//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolvePathWindows(t *testing.T) {
	t.Setenv("SystemDrive", "D:")
	t.Setenv("BUREAU_TEST", `C:\Users\me\AppData\Roaming`)

	cases := map[string]string{
		// rooted paths without a drive go to the system drive, not the cwd's
		"/tmp/bureau.txt":          `D:\tmp\bureau.txt`,
		`\ProgramData\app\a.ini`:   `D:\ProgramData\app\a.ini`,
		"C:/ProgramData/app/a.ini": `C:\ProgramData\app\a.ini`,
		`%BUREAU_TEST%\app\a.ini`:  `C:\Users\me\AppData\Roaming\app\a.ini`,
		`\\server\share\app\a.ini`: `\\server\share\app\a.ini`,
		`C:\a\..\b\.\c.ini`:        `C:\b\c.ini`,
	}
	for in, want := range cases {
		got, err := ResolvePath(in)
		if err != nil || got != want {
			t.Errorf("ResolvePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	// drive-relative paths depend on the cwd of that drive
	if _, err := ResolvePath(`C:bureau.txt`); err == nil {
		t.Errorf("drive-relative path should be rejected")
	}

	if pathKey(`C:\App\A.ini`) != pathKey(`c:\app\a.ini`) {
		t.Errorf("paths must be case-insensitive on Windows")
	}
}

func TestReplaceFileRetriesWhileLocked(t *testing.T) {
	replaceBackoff = 20 * time.Millisecond
	defer func() { replaceBackoff = 200 * time.Millisecond }()

	path := filepath.Join(t.TempDir(), "conf")
	publish(t, path, "0644", "old")

	// os.Open doesn't use FILE_SHARE_DELETE, so the target can't be
	// replaced while it's held open
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.Close()
	}()

	publish(t, path, "0644", "new")
	if got := readFile(t, path); got != "new" {
		t.Errorf("content = %q, want new", got)
	}
}

func TestReplaceFileGivesUpWhenLocked(t *testing.T) {
	replaceRetries, replaceBackoff = 1, time.Millisecond
	defer func() { replaceRetries, replaceBackoff = 5, 200*time.Millisecond }()

	path := filepath.Join(t.TempDir(), "conf")
	publish(t, path, "0444", "old")

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ResetTmpFile(path)
	if err := WriteFile(path, "new"); err != nil {
		t.Fatal(err)
	}
	err = PublishFile(path, "0444")
	if err == nil {
		t.Skip("filesystem allows replacing open files")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file: %v", err)
	}

	// the read-only attribute must be restored on failure
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0200 != 0 {
		t.Errorf("read-only attribute not restored: %v", info.Mode())
	}
}
