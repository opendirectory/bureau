//go:build !windows

package main

import "os"

// normalizeOSPath is a no-op outside Windows.
func normalizeOSPath(p string) string {
	return p
}

// replaceFile atomically moves tmp over dst. The mode is applied before the
// rename so the file is never visible with the wrong permissions.
func replaceFile(tmp, dst string, mode os.FileMode) error {
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}

	return os.Rename(tmp, dst)
}
