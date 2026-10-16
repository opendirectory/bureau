package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

type Kalived struct {
	DN                          string   `ldap:"dn"`
	Path                        string   `ldap:"path"`
	Perm                        string   `ldap:"permissions"`
	Mtime                       string   `ldap:"modifyTimestamp"`
	ServiceName                 string   `ldap:"serviceName"`
	GlobalNotificationEmail     []string `ldap:"globalNotificationEmail"`
	GlobalNotificationEmailFrom string   `ldap:"globalNotificationEmailFrom"`
	GlobalSMTPServer            string   `ldap:"globalSMTPServer"`
	GlobalSMTPConnectTimeout    int      `ldap:"globalSMTPConnectTimeout"`
	GlobalLVSId                 string   `ldap:"globalLVSId"`
	GroupName                   string   `ldap:"groupName"`
	GroupMember                 []string `ldap:"groupMember"`
	NotifyMasterVRRPGroup       string   `ldap:"notifyMasterVRRPGroup"`
	NotifyBackupVRRPGroup       string   `ldap:"notifyBackupVRRPGroup"`
	NotifyFaultVRRPGroup        string   `ldap:"notifyFaultVRRPGroup"`
	InstanceName                string   `ldap:"instanceName"`
	/* Need to treat next value as string since
	entry.Unmarshal method doesn't support bool */
	SMTPAlert                string   `ldap:"smtpAlert"`
	AuthType                 string   `ldap:"authType"`
	AuthPass                 string   `ldap:"authPass"`
	VirtualIPAddress         []string `ldap:"virtualIPAddress"`
	VirtualIPAddressExcluded []string `ldap:"virtualIPAddressExcluded"`
	State                    string   `ldap:"state"`
	Interface                string   `ldap:"interface"`
	McastSrcIP               string   `ldap:"mcastSrcIP"`
	LVSSyncDaemonInterface   string   `ldap:"lvsSyncDaemonInterface"`
	VirtualRouterID          int      `ldap:"virtualRouterID"`
	Priority                 int      `ldap:"priority"`
	AdvertInt                int      `ldap:"advertInt"`
	UnicastSrcIP             string   `ldap:"unicastSrcIP"`
	UnicastPeer              []string `ldap:"unicastPeer"`
	NotifyMasterVRRPInstance string   `ldap:"notifyMasterVRRPInstance"`
	NotifyBackupVRRPInstance string   `ldap:"notifyBackupVRRPInstance"`
	NotifyFaultVRRPInstance  string   `ldap:"notifyFaultVRRPInstance"`
}

// keepalivedTemplates maps each keepalived objectClass (lowercased) to the
// template used to render it.
var keepalivedTemplates = map[string]string{
	"keepalivedglobalconfig":       "kglobal",
	"keepalivedvrrpgroupconfig":    "kgroup",
	"keepalivedvrrpinstanceconfig": "kinstance",
}

// IsKeepalived reports whether any of the objectClasses is a keepalived one.
func IsKeepalived(objectClasses []string) bool {
	_, err := keepalivedTemplate(objectClasses)
	return err == nil
}

func keepalivedTemplate(objectClasses []string) (string, error) {
	for _, oc := range objectClasses {
		if tmpl, ok := keepalivedTemplates[strings.ToLower(oc)]; ok {
			return tmpl, nil
		}
	}

	return "", fmt.Errorf("no keepalived template for objectClasses %v", objectClasses)
}

// templateDirs returns the locations searched for keepalived templates, in
// order of precedence. The working directory alone isn't enough, since
// under systemd it is "/".
func templateDirs() []string {
	dirs := []string{"templates/keepalived"}

	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "templates", "keepalived"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "bureau", "templates", "keepalived"))
	}

	if runtime.GOOS == "windows" {
		if dir := os.Getenv("ProgramData"); dir != "" {
			dirs = append(dirs, filepath.Join(dir, "bureau", "templates", "keepalived"))
		}
		return dirs
	}

	return append(dirs,
		"/etc/bureau/templates/keepalived",
		"/usr/share/bureau/templates/keepalived",
	)
}

func loadKeepalivedTemplates() (*template.Template, error) {
	for _, dir := range templateDirs() {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.tmpl"))
		if len(matches) == 0 {
			continue
		}

		Logger(nil, "Loading keepalived templates from "+dir, "DEBUG")
		return template.ParseFiles(matches...)
	}

	return nil, fmt.Errorf("no keepalived templates found in %v", templateDirs())
}

// FormatKeepalived renders a keepalived entry with the template matching its
// objectClass and returns the resulting config text.
func FormatKeepalived(f Kalived, objectClasses []string) (string, error) {

	class, err := keepalivedTemplate(objectClasses)
	if err != nil {
		return "", err
	}

	if Tpl == nil || Tpl.Lookup(class+".tmpl") == nil {
		Tpl, err = loadKeepalivedTemplates()
		if err != nil {
			return "", err
		}
	}

	var configData bytes.Buffer
	if err = Tpl.ExecuteTemplate(&configData, class+".tmpl", f); err != nil {
		return "", fmt.Errorf("rendering %s template: %w", class, err)
	}

	// keep sections separated when several entries share a file
	out := configData.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	return out, nil
}
