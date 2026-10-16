package main

import (
	"log"
	"text/template"
	"time"
)

var Tpl *template.Template

type File struct {
	DN          string   `ldap:"dn"`
	Path        string   `ldap:"path"`
	Description string   `ldap:"description"`
	CN          string   `ldap:"cn"`
	ObjectClass []string `ldap:"objectClass"`
	Data        string   `ldap:"data"`
	Perm        string   `ldap:"permissions"`
	Mtime       string   `ldap:"modifyTimestamp"`
}

// Logger logs msg at the given level.
//   - FATAL: exits if err is non-nil
//   - WARN:  logs if err is non-nil
//   - DEBUG: logs (with err, if any) only when debug mode is enabled
func Logger(err error, msg string, level string) {
	switch level {
	case "FATAL":
		if err != nil {
			log.Fatalf("[%s] %s: %v", level, msg, err)
		}
	case "WARN":
		if err != nil {
			log.Printf("[%s] %s: %v", level, msg, err)
		}
	case "DEBUG":
		if !C.Debug {
			return
		}
		if err != nil {
			log.Printf("[%s] %s: %v", level, msg, err)
		} else {
			log.Printf("[%s] %s", level, msg)
		}
	}
}

func ConvertLDAPtoRFC3339(mtime string) (time.Time, error) {
	const ldapLayout = "20060102150405Z"
	t, err := time.Parse(ldapLayout, mtime)

	return t, err
}
