package main

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// LDAPTLSConfig builds the TLS settings used for both ldaps:// and
// StartTLS. The certificate is verified against the system trust store and
// the server's host name unless tls_skip_verify is set.
func LDAPTLSConfig(server string) (*tls.Config, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", server, err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("server URL %q has no host", server)
	}

	return &tls.Config{
		// required for hostname verification; StartTLS doesn't set it itself
		ServerName:         u.Hostname(),
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: C.TLSSkipVerify,
	}, nil
}

func LDAPConnect() (*ldap.Conn, error) {

	tlsConfig, err := LDAPTLSConfig(C.Server)
	if err != nil {
		return nil, err
	}

	l, err := ldap.DialURL(C.Server, ldap.DialWithTLSConfig(tlsConfig))
	if err != nil {
		return nil, err
	}

	// ldaps:// is already encrypted; StartTLS would fail on it
	if !strings.HasPrefix(strings.ToLower(C.Server), "ldaps://") {
		err = l.StartTLS(tlsConfig)
		if err != nil {
			l.Close()
			return nil, err
		}
	}

	if err = l.Bind(C.Binddn, C.Password); err != nil {
		l.Close()
		return nil, fmt.Errorf("Bind Error: %s", err)
	}

	return l, nil
}

// LDAPSearch returns all configFile entries in the subtree under base. An
// empty result is not an error.
func LDAPSearch(l *ldap.Conn, base string, attr []string) (*ldap.SearchResult, error) {
	return ldapSearch(l, base, ldap.ScopeWholeSubtree, attr)
}

// LDAPGetEntry returns exactly the configFile entry at dn.
func LDAPGetEntry(l *ldap.Conn, dn string, attr []string) (*ldap.Entry, error) {
	result, err := ldapSearch(l, dn, ldap.ScopeBaseObject, attr)
	if err != nil {
		return nil, err
	}

	if len(result.Entries) == 0 {
		return nil, fmt.Errorf("Entry not found: %s", dn)
	}

	return result.Entries[0], nil
}

func ldapSearch(l *ldap.Conn, base string, scope int, attr []string) (*ldap.SearchResult, error) {

	searchReq := ldap.NewSearchRequest(
		base,
		scope,
		ldap.NeverDerefAliases,
		0,
		0,
		false,
		"(objectClass=configFile)",
		attr,
		nil,
	)
	result, err := l.Search(searchReq)
	if err != nil {
		return nil, fmt.Errorf("Search Error: %s", err)
	}

	return result, nil
}
