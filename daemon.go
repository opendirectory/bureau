package main

import (
	"fmt"

	"github.com/go-ldap/ldap/v3"
)

// pendingFile collects the output of every LDAP entry that targets one
// local file during a sync.
type pendingFile struct {
	Local     string         // resolved local path
	CM        ConfigMapEntry // full rendered content
	renderErr error          // an entry couldn't be rendered: publish nothing
	diskErr   error          // the tmp file couldn't be written
}

// Summon runs a single sync: it finds outdated config files in LDAP,
// regenerates them, and publishes them to disk (and Kubernetes if enabled).
func Summon() error {

	// LDAP connect
	l, err := LDAPConnect()
	if err != nil {
		return fmt.Errorf("connection error: %w", err)
	}
	defer l.Close()

	// Search for files that need updating
	result, err := LDAPSearch(l, C.HostDN, []string{"modifyTimestamp", "path"})
	if err != nil {
		return fmt.Errorf("LDAP search error: %w", err)
	}

	// Several entries may target the same path; their output is concatenated
	// in LDAP order. If any of them changed, the whole file is regenerated
	// from all of them, otherwise the other sections would be lost.
	type entryRef struct{ DN, Local, Key string }
	var entries []entryRef
	outdated := make(map[string]bool)

	for _, entry := range result.Entries {

		// Import to struct
		f := File{}
		if err = entry.Unmarshal(&f); err != nil {
			Logger(err, "Unmarshal error for "+entry.DN, "WARN")
			continue
		}
		if f.Path == "" {
			Logger(fmt.Errorf("missing path attribute"), "Skipping "+f.DN, "WARN")
			continue
		}

		local, err := ResolvePath(f.Path)
		if err != nil {
			Logger(err, "Skipping "+f.DN, "WARN")
			continue
		}
		key := pathKey(local)
		entries = append(entries, entryRef{DN: f.DN, Local: local, Key: key})

		if outdated[key] {
			continue
		}

		// Check for file and get mtime
		exists, fileOnDiskMtime := FileExist(local)
		if !exists {
			outdated[key] = true
			continue
		}

		// Get ldap mtime
		ldapMtime, err := ConvertLDAPtoRFC3339(f.Mtime)
		if err != nil {
			Logger(err, "Failed converting LDAP time to RFC3339", "WARN")
			continue
		}

		// Compare LDAP and file mtime
		if ldapMtime.After(fileOnDiskMtime) {
			Logger(nil, "Queueing outdated file: "+local, "DEBUG")
			outdated[key] = true
		}
	}

	files := make(map[string]*pendingFile)
	var order []string

	// Grab file data from LDAP for files needing update
	for _, e := range entries {
		if !outdated[e.Key] {
			continue
		}

		p, ok := files[e.Key]
		if !ok {
			p = &pendingFile{Local: e.Local}
			files[e.Key] = p
			order = append(order, e.Key)

			// Start every outdated file from scratch
			ResetTmpFile(e.Local)
		}
		if p.renderErr != nil {
			continue
		}

		f, data, err := RenderEntry(l, e.DN)
		if err == nil {
			_, err = ParsePerm(f.Perm)
		}
		if err != nil {
			p.renderErr = fmt.Errorf("%s: %w", e.DN, err)
			continue
		}

		// The first entry names the ConfigMap and the first non-empty
		// permissions win
		if p.CM.DN == "" {
			p.CM.DN = f.DN
			p.CM.Path = f.Path
		}
		if p.CM.Perm == "" {
			p.CM.Perm = f.Perm
		}
		if f.Mtime > p.CM.Mtime {
			p.CM.Mtime = f.Mtime
		}
		p.CM.Data += data

		if p.diskErr == nil {
			Logger(nil, "Writing "+f.DN+" to "+e.Local, "DEBUG")
			p.diskErr = WriteFile(e.Local, data)
		}
	}

	for _, key := range order {
		p := files[key]

		switch {
		case p.renderErr != nil:
			// don't publish a partial file anywhere
			Logger(p.renderErr, "Error generating "+p.Local, "WARN")
			ResetTmpFile(p.Local)
			continue

		case p.diskErr != nil:
			Logger(p.diskErr, "Error writing "+p.Local, "WARN")
			ResetTmpFile(p.Local)

		default:
			if err := PublishFile(p.Local, p.CM.Perm); err != nil {
				Logger(err, "Error publishing file", "WARN")
				ResetTmpFile(p.Local)
			}
		}

		// The content is complete, so the ConfigMap is published even if the
		// local filesystem isn't writable (e.g. inside a pod)
		QueueConfigMap(p.CM)
	}

	// Create/update ConfigMaps for synced LDAP objects (if enabled)
	err = PublishConfigMaps()
	Logger(err, "ConfigMap publish error", "WARN")

	return nil
}

// RenderEntry fetches the full LDAP entry at dn and returns it together with
// the file content it should produce, formatted according to its objectClass.
func RenderEntry(l *ldap.Conn, dn string) (File, string, error) {

	// Request remaining attributes from LDAP and add to struct
	entry, err := LDAPGetEntry(l, dn, []string{"*", "+"})
	if err != nil {
		return File{}, "", err
	}

	f := File{}
	if err = entry.Unmarshal(&f); err != nil {
		return f, "", fmt.Errorf("unmarshal error: %w", err)
	}

	// Format data based on objectClass
	switch {
	case IsKeepalived(f.ObjectClass):
		k := Kalived{}
		if err = entry.Unmarshal(&k); err != nil {
			return f, "", fmt.Errorf("unmarshal error: %w", err)
		}

		data, err := FormatKeepalived(k, f.ObjectClass)
		return f, data, err

	default:
		return f, f.Data, nil
	}
}
