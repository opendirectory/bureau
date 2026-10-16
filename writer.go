package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FileExist reports whether path exists and its mtime. Any stat error
// (not just "not exist") is treated as missing so the file is regenerated.
func FileExist(path string) (bool, time.Time) {
	info, err := os.Stat(path)
	if err != nil {
		Logger(err, "Cannot stat file "+path, "DEBUG")
		return false, time.Time{}
	}

	return true, info.ModTime()
}

// ParsePerm parses an octal permission string such as "0644", defaulting
// to 0600 when it is empty. On Windows only the owner write bit (0200) has
// an effect: without it the file is marked read-only.
func ParsePerm(perm string) (os.FileMode, error) {
	if perm == "" {
		return 0600, nil
	}

	mode, err := strconv.ParseUint(perm, 8, 32)
	if err != nil || mode > 07777 {
		return 0, fmt.Errorf("invalid permissions %q", perm)
	}

	// Go represents the special bits as separate flags, not as 04000 etc.
	fm := os.FileMode(mode & 0777)
	if mode&04000 != 0 {
		fm |= os.ModeSetuid
	}
	if mode&02000 != 0 {
		fm |= os.ModeSetgid
	}
	if mode&01000 != 0 {
		fm |= os.ModeSticky
	}

	return fm, nil
}

var percentEnvVar = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_()]*)%`)

// expandPercentEnv expands Windows-style %VAR% references. Unknown
// variables are left untouched.
func expandPercentEnv(p string, lookup func(string) (string, bool)) string {
	return percentEnvVar.ReplaceAllStringFunc(p, func(m string) string {
		if v, ok := lookup(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
}

// ExpandPath expands a leading "~" to the user's home directory and, on
// Windows, %VAR% environment variables (e.g. %APPDATA%\app\config.ini).
func ExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}

	if runtime.GOOS == "windows" {
		p = expandPercentEnv(p, os.LookupEnv)
	}

	return p
}

// ResolvePath turns the LDAP path attribute into a clean, absolute local
// path. Relative paths are rejected, since they would depend on the working
// directory ("/" under systemd, C:\Windows\System32 for Windows services).
func ResolvePath(p string) (string, error) {
	local := filepath.Clean(normalizeOSPath(ExpandPath(p)))

	if !filepath.IsAbs(local) {
		return "", fmt.Errorf("path %q is not absolute", p)
	}

	return local, nil
}

// pathKey identifies a file: paths are case-insensitive on Windows.
func pathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// WriteFile appends data to path's tmp file. Multiple LDAP entries can
// target the same path, so their data is concatenated; ResetTmpFile must be
// called once per path before the first write of a sync. Permissions are
// applied by PublishFile.
func WriteFile(path string, data string) error {
	f, err := os.OpenFile(path+".tmp", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening tmp file: %w", err)
	}
	defer f.Close()

	if _, err = f.WriteString(data); err != nil {
		return fmt.Errorf("writing tmp file: %w", err)
	}

	return f.Close()
}

// ResetTmpFile removes a leftover tmp file (e.g. from an interrupted sync
// or a failed write) so stale data isn't appended to or left behind.
func ResetTmpFile(path string) {
	err := os.Remove(path + ".tmp")
	if err != nil && !os.IsNotExist(err) {
		Logger(err, "Error removing tmp file "+path+".tmp", "WARN")
	}
}

// PublishFile atomically replaces path with its tmp file and applies the
// requested permissions.
func PublishFile(path string, perm string) error {
	mode, err := ParsePerm(perm)
	if err != nil {
		return err
	}

	if err = replaceFile(path+".tmp", path, mode); err != nil {
		return fmt.Errorf("publishing %s: %w", path, err)
	}

	Logger(nil, "Published "+path, "DEBUG")
	return nil
}
