//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

type fakeEventLog struct {
	events []string
}

func (f *fakeEventLog) Info(eid uint32, msg string) error {
	f.events = append(f.events, fmt.Sprintf("info/%d/%s", eid, msg))
	return nil
}

func (f *fakeEventLog) Warning(eid uint32, msg string) error {
	f.events = append(f.events, fmt.Sprintf("warning/%d/%s", eid, msg))
	return nil
}

func (f *fakeEventLog) Error(eid uint32, msg string) error {
	f.events = append(f.events, fmt.Sprintf("error/%d/%s", eid, msg))
	return nil
}

func TestEventLogWriterLevels(t *testing.T) {
	el := &fakeEventLog{}
	w := &eventLogWriter{el: el}

	for _, line := range []string{
		"[FATAL] Error reading config: boom\n",
		"[WARN] Sync failed: ldap down\n",
		"[DEBUG] Writing file\n",
	} {
		if n, err := w.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = %d, %v", line, n, err)
		}
	}

	want := []string{
		"error/3/[FATAL] Error reading config: boom",
		"warning/2/[WARN] Sync failed: ldap down",
		"info/1/[DEBUG] Writing file",
	}
	if strings.Join(el.events, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(el.events, "\n"), strings.Join(want, "\n"))
	}
}

// startTestService runs Execute with a stubbed config and sync and returns
// once the service reports Running.
func startTestService(t *testing.T, sync func() error) (chan svc.ChangeRequest, chan svc.Status, chan [2]any) {
	t.Helper()

	origLoad := loadConfig
	t.Cleanup(func() { loadConfig = origLoad })
	loadConfig = func() error { C = Config{Daemon: false, Update: 3600}; return nil }
	stubSync(t, sync)

	requests := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 10)
	result := make(chan [2]any, 1)
	go func() {
		ssec, code := (&bureauService{}).Execute(nil, requests, status)
		result <- [2]any{ssec, code}
	}()

	for _, want := range []svc.State{svc.StartPending, svc.Running} {
		select {
		case st := <-status:
			if st.State != want {
				t.Fatalf("state = %s, want %s", stateName(st.State), stateName(want))
			}
			if want == svc.Running && st.Accepts&svc.AcceptStop == 0 {
				t.Errorf("service must accept stop requests")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", stateName(want))
		}
	}

	return requests, status, result
}

func TestServiceStop(t *testing.T) {
	var calls atomic.Int32
	requests, status, result := startTestService(t, func() error {
		calls.Add(1)
		return errors.New("ldap down")
	})

	// daemon: false must be overridden, or the service would exit after one sync
	if !C.Daemon {
		t.Errorf("daemon mode not forced")
	}

	requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	if st := <-status; st.State != svc.Running {
		t.Errorf("interrogate returned %s", stateName(st.State))
	}

	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if st := <-status; st.State != svc.StopPending || st.WaitHint == 0 {
		t.Errorf("got %+v, want stop pending with a wait hint", st)
	}

	select {
	case r := <-result:
		if r[0] != false || r[1] != uint32(0) {
			t.Errorf("Execute returned %v, want clean exit", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}

	if calls.Load() != 1 {
		t.Errorf("synced %d times, want 1", calls.Load())
	}
}

func TestServiceStopDoesNotWaitForeverOnSync(t *testing.T) {
	serviceStopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { serviceStopTimeout = 15 * time.Second })

	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	var once atomic.Bool
	requests, _, result := startTestService(t, func() error {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		<-release // stuck, e.g. on an unresponsive LDAP server
		return nil
	})
	<-started

	requests <- svc.ChangeRequest{Cmd: svc.Shutdown}
	select {
	case r := <-result:
		if r[0] != false || r[1] != uint32(0) {
			t.Errorf("Execute returned %v, want clean exit", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop waited for the stuck sync")
	}
}

func TestServiceConfigError(t *testing.T) {
	origLoad := loadConfig
	t.Cleanup(func() { loadConfig = origLoad })
	loadConfig = func() error { return errors.New("parsing bureau.yaml: bad yaml") }
	stubSync(t, func() error {
		t.Error("must not sync without a config")
		return nil
	})

	status := make(chan svc.Status, 10)
	ssec, code := (&bureauService{}).Execute(nil, make(chan svc.ChangeRequest), status)

	// a service-specific exit code, rather than exiting the process, so the
	// SCM sees a stopped service and `start` can report why
	if !ssec || code != exitCodeConfig {
		t.Errorf("Execute = %v, %d; want service-specific exit code %d", ssec, code, exitCodeConfig)
	}
}

func TestServiceArgs(t *testing.T) {
	orig := configPath
	t.Cleanup(func() { configPath = orig })

	configPath = ""
	if args, err := serviceArgs(); err != nil || strings.Join(args, " ") != "-service" {
		t.Errorf("serviceArgs() = %v, %v; want [-service]", args, err)
	}

	dir := t.TempDir()
	conf := filepath.Join(dir, "bureau.yaml")
	if err := os.WriteFile(conf, []byte("debug: true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// relative paths are made absolute, since services start in System32
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(dir)
	configPath = "bureau.yaml"
	args, err := serviceArgs()
	if err != nil || len(args) != 3 || args[0] != "-service" || args[1] != "-config" || !strings.EqualFold(args[2], conf) {
		t.Errorf("serviceArgs() = %v, %v; want [-service -config %s]", args, err, conf)
	}

	configPath = filepath.Join(dir, "missing.yaml")
	if _, err := serviceArgs(); err == nil {
		t.Errorf("expected error for missing config file")
	}
}
