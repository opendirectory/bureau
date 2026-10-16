package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// stubSync replaces Summon and restores the config afterwards
func stubSync(t *testing.T, fn func() error) {
	t.Helper()

	origSync, origC := syncOnce, C
	t.Cleanup(func() { syncOnce, C = origSync, origC })
	syncOnce = fn
}

func TestRunOnce(t *testing.T) {
	var calls atomic.Int32
	wantErr := errors.New("ldap down")
	stubSync(t, func() error { calls.Add(1); return wantErr })
	C = Config{Daemon: false, Update: 1}

	if err := Run(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("Run() = %v, want %v", err, wantErr)
	}
	if calls.Load() != 1 {
		t.Errorf("synced %d times, want 1", calls.Load())
	}
}

func TestRunDaemonStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	stubSync(t, func() error {
		// errors must not end the loop in daemon mode
		if calls.Add(1) == 1 {
			cancel()
		}
		return errors.New("transient")
	})
	C = Config{Daemon: true, Update: 3600}

	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel; it must not wait for update_interval")
	}
	if calls.Load() != 1 {
		t.Errorf("synced %d times, want 1", calls.Load())
	}
}

func TestRunDaemonRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	stubSync(t, func() error {
		if calls.Add(1) == 2 {
			cancel()
		}
		return nil
	})
	C = Config{Daemon: true, Update: 1}

	if err := Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("synced %d times, want 2", calls.Load())
	}
}

func TestConfigPathFlag(t *testing.T) {
	origC, origPath := C, configPath
	t.Cleanup(func() { C, configPath = origC, origPath })

	configPath = filepath.Join(t.TempDir(), "custom.yaml")
	data := "server: ldaps://ldap.example.com\nbase: ou=config,dc=example,dc=com\nupdate_interval: 42\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	C = Config{}
	conf, err := ConfigInit()
	if err != nil {
		t.Fatal(err)
	}
	if conf.Server != "ldaps://ldap.example.com" || conf.Update != 42 {
		t.Errorf("config not loaded from -config path: %+v", conf)
	}

	// errors are returned instead of exiting, so the service can report them
	if err := os.WriteFile(configPath, []byte("debug: [not valid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigInit(); err == nil {
		t.Errorf("expected error for invalid YAML")
	}

	configPath = filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := ConfigInit(); err == nil {
		t.Errorf("expected error for missing -config file")
	}
}
