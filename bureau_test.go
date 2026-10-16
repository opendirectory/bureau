package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepalivedTemplateSelection(t *testing.T) {
	cases := map[string]string{
		"keepalivedGlobalConfig":       "global_defs {",
		"keepalivedVRRPGroupConfig":    "vrrp_sync_group VG1 {",
		"keepalivedVRRPInstanceConfig": "vrrp_instance VI1 {",
	}

	k := Kalived{GroupName: "VG1", InstanceName: "VI1"}
	for oc, want := range cases {
		Tpl = nil
		out, err := FormatKeepalived(k, []string{"top", "configFile", oc})
		if err != nil {
			t.Fatalf("%s: %v", oc, err)
		}
		if !strings.HasPrefix(out, want) {
			t.Errorf("%s rendered with wrong template:\n%s", oc, out)
		}
	}

	if IsKeepalived([]string{"top", "configFile"}) {
		t.Errorf("plain configFile detected as keepalived")
	}
}

func TestKeepalivedInstanceTemplate(t *testing.T) {
	Tpl = nil
	k := Kalived{
		InstanceName:             "VI1",
		State:                    "MASTER",
		Interface:                "eth0",
		VirtualIPAddress:         []string{"10.0.0.1"},
		VirtualIPAddressExcluded: []string{"10.0.0.2"},
		NotifyMasterVRRPInstance: "/bin/master.sh",
		NotifyBackupVRRPInstance: "/bin/backup.sh",
		NotifyFaultVRRPInstance:  "/bin/fault.sh",
	}

	out, err := FormatKeepalived(k, []string{"keepalivedVRRPInstanceConfig"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Errorf("unbalanced braces:\n%s", out)
	}
	for _, line := range []string{
		"virtual_ipaddress_excluded {",
		"notify_master /bin/master.sh\n",
		"notify_backup /bin/backup.sh\n",
		"notify_fault /bin/fault.sh\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
}

func TestGlobalTemplateEmailFrom(t *testing.T) {
	Tpl = nil
	out, err := FormatKeepalived(Kalived{GlobalNotificationEmailFrom: "a@b.c"}, []string{"keepalivedGlobalConfig"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notification_email_from a@b.c") {
		t.Errorf("notification_email_from missing:\n%s", out)
	}
}

// publish writes the given sections to path the way Summon does
func publish(t *testing.T, path, perm string, sections ...string) {
	t.Helper()

	ResetTmpFile(path)
	for _, s := range sections {
		if err := WriteFile(path, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := PublishFile(path, perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteAndPublishFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf")

	// stale tmp data from an interrupted run must not leak into the file
	if err := os.WriteFile(path+".tmp", []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}

	// read-only files built from several sections
	publish(t, path, "0444", "a\n", "b\n")
	if got := readFile(t, path); got != "a\nb\n" {
		t.Errorf("unexpected content %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0444 {
		t.Errorf("permissions = %o, want 444", info.Mode().Perm())
	}

	// replacing a read-only file must work (fails with MoveFileEx on Windows
	// unless the attribute is cleared)
	publish(t, path, "0644", "c\n")
	if got := readFile(t, path); got != "c\n" {
		t.Errorf("unexpected content after replace %q", got)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm()&0200 == 0 {
		t.Errorf("file still read-only after publishing with 0644: %v", info.Mode())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file left behind: %v", err)
	}

	if err := PublishFile(path, "rwx"); err == nil {
		t.Errorf("expected error for invalid permissions")
	}
}

func TestParsePerm(t *testing.T) {
	cases := map[string]os.FileMode{
		"":      0600,
		"0644":  0644,
		"755":   0755,
		"4755":  os.ModeSetuid | 0755,
		"2755":  os.ModeSetgid | 0755,
		"1777":  os.ModeSticky | 0777,
		"06750": os.ModeSetuid | os.ModeSetgid | 0750,
	}
	for in, want := range cases {
		got, err := ParsePerm(in)
		if err != nil || got != want {
			t.Errorf("ParsePerm(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for _, bad := range []string{"rwx", "0999", "17777", "-1"} {
		if _, err := ParsePerm(bad); err == nil {
			t.Errorf("ParsePerm(%q) should fail", bad)
		}
	}
}

func TestResolvePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	got, err := ResolvePath("~/.bashrc")
	if err != nil || got != filepath.Join(home, ".bashrc") {
		t.Errorf("ResolvePath(~/.bashrc) = %q, %v", got, err)
	}

	for _, rel := range []string{"bureau.txt", "./conf/bureau.txt", "../x"} {
		if _, err := ResolvePath(rel); err == nil {
			t.Errorf("ResolvePath(%q) should reject relative paths", rel)
		}
	}
}

func TestExpandPercentEnv(t *testing.T) {
	env := map[string]string{"APPDATA": `C:\Users\me\AppData\Roaming`, "ProgramFiles(x86)": `C:\Program Files (x86)`}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	cases := map[string]string{
		`%APPDATA%\app\config.ini`:      `C:\Users\me\AppData\Roaming\app\config.ini`,
		`%ProgramFiles(x86)%\app\a.ini`: `C:\Program Files (x86)\app\a.ini`,
		`%UNSET%\a.ini`:                 `%UNSET%\a.ini`,
		`C:\100%\a.ini`:                 `C:\100%\a.ini`,
	}
	for in, want := range cases {
		if got := expandPercentEnv(in, lookup); got != want {
			t.Errorf("expandPercentEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
