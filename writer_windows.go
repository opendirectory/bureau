//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	errSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	errLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// Overridable in tests
var (
	replaceRetries = 5
	replaceBackoff = 200 * time.Millisecond
)

// normalizeOSPath maps rooted paths without a drive letter, such as
// "/tmp/bureau.txt", onto the system drive (C:\tmp\bureau.txt) rather than
// whatever drive the working directory happens to be on.
func normalizeOSPath(p string) string {
	if filepath.VolumeName(p) == "" && (strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/")) {
		drive := os.Getenv("SystemDrive")
		if drive == "" {
			drive = "C:"
		}
		return drive + p
	}

	return p
}

// isTransientReplaceError reports whether a rename failed because another
// process (editor, antivirus, search indexer, ...) has the file open.
func isTransientReplaceError(err error) bool {
	return errors.Is(err, errSharingViolation) ||
		errors.Is(err, errLockViolation) ||
		errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}

// replaceFile moves tmp over dst. os.Rename uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which is atomic on NTFS but refuses to replace
// read-only files and files that are open without FILE_SHARE_DELETE. The
// read-only attribute is cleared first and the rename is retried briefly.
func replaceFile(tmp, dst string, mode os.FileMode) error {
	clearedReadOnly := false
	if info, err := os.Lstat(dst); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0200 == 0 {
		if err := os.Chmod(dst, 0666); err != nil {
			return err
		}
		clearedReadOnly = true
	}

	var err error
	for attempt := 0; attempt <= replaceRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(replaceBackoff * time.Duration(attempt))
		}
		if err = os.Rename(tmp, dst); err == nil || !isTransientReplaceError(err) {
			break
		}
	}
	if err != nil {
		if clearedReadOnly {
			os.Chmod(dst, 0444)
		}
		return err
	}

	// the read-only attribute is applied after the move so it can't block it
	return os.Chmod(dst, mode)
}
