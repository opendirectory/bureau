package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

func TestLDAPTLSConfig(t *testing.T) {
	t.Cleanup(func() { C.TLSSkipVerify = false })

	cases := map[string]string{
		"ldap://10.10.0.94":            "10.10.0.94",
		"ldaps://ldap.example.com:636": "ldap.example.com",
		"ldap://[::1]:389":             "::1",
	}
	for server, want := range cases {
		cfg, err := LDAPTLSConfig(server)
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		if cfg.ServerName != want {
			t.Errorf("%s: ServerName = %q, want %q", server, cfg.ServerName, want)
		}
		if cfg.InsecureSkipVerify {
			t.Errorf("%s: certificate verification must be on by default", server)
		}
	}

	C.TLSSkipVerify = true
	cfg, _ := LDAPTLSConfig("ldap://ldap.example.com")
	if !cfg.InsecureSkipVerify {
		t.Errorf("tls_skip_verify not applied")
	}

	if _, err := LDAPTLSConfig("ldap://"); err == nil {
		t.Errorf("expected error for URL without host")
	}
}

// The same TLS config must be used for ldaps://, which previously ignored it
func TestLDAPSVerification(t *testing.T) {
	t.Cleanup(func() { C.TLSSkipVerify = false })

	// self-signed certificate for 127.0.0.1
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	server := "ldaps://" + strings.TrimPrefix(srv.URL, "https://")

	dial := func() error {
		cfg, err := LDAPTLSConfig(server)
		if err != nil {
			return err
		}
		l, err := ldap.DialURL(server, ldap.DialWithTLSConfig(cfg))
		if err == nil {
			l.Close()
		}
		return err
	}

	C.TLSSkipVerify = false
	if err := dial(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("expected certificate verification error, got %v", err)
	}

	C.TLSSkipVerify = true
	if err := dial(); err != nil {
		t.Errorf("tls_skip_verify should accept self-signed certificate: %v", err)
	}
}
