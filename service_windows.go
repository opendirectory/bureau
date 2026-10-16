//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName        = "bureau"
	serviceDisplayName = "Bureau configuration agent"
	serviceDescription = "Synchronizes configuration files from LDAP."

	serviceUsage = `Service commands (run from an elevated prompt):
  install    register bureau as a Windows service that starts automatically;
             -config, if given, is saved in the service's command line
  uninstall  stop and remove the service
  start      start the service
  stop       stop the service
`

	// event IDs (EventCreate.exe supports 1-1000)
	eventIDInfo    = 1
	eventIDWarning = 2
	eventIDError   = 3

	// service-specific exit codes
	exitCodeUnexpected = 1
	exitCodeConfig     = 2
)

// Overridable in tests
var (
	loadConfig = LoadConfig

	// how long a running sync may delay a stop request
	serviceStopTimeout = 15 * time.Second
	// how long start/stop/uninstall wait for the service to change state
	serviceCommandTimeout = 30 * time.Second
)

func isWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

// runService is called when bureau is started by the service control
// manager. Logs go to the Application event log, since a service has no
// console.
func runService() error {
	if el, err := eventlog.Open(serviceName); err == nil {
		defer el.Close()
		log.SetFlags(0) // the event log records the time itself
		log.SetOutput(&eventLogWriter{el: el})
	}

	return svc.Run(serviceName, &bureauService{})
}

type eventLogger interface {
	Info(eid uint32, msg string) error
	Warning(eid uint32, msg string) error
	Error(eid uint32, msg string) error
}

// eventLogWriter sends each log line to the event log, mapping the Logger
// level to the event type.
type eventLogWriter struct {
	el eventLogger
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")

	var err error
	switch {
	case strings.HasPrefix(msg, "[FATAL]"):
		err = w.el.Error(eventIDError, msg)
	case strings.HasPrefix(msg, "[WARN]"):
		err = w.el.Warning(eventIDWarning, msg)
	default:
		err = w.el.Info(eventIDInfo, msg)
	}
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

type bureauService struct{}

// Execute implements svc.Handler.
func (s *bureauService) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// Loaded here rather than before svc.Run, so a slow or broken config
	// can't make the service manager time out waiting for the service. A
	// failure is reported as a stop with an error code, which `start`
	// shows and the recovery actions retry.
	if err := loadConfig(); err != nil {
		Logger(err, "Error reading config file", "WARN")
		return true, exitCodeConfig
	}
	if !C.Daemon {
		Logger(errors.New("daemon: false"), "Ignoring setting when running as a service", "WARN")
		C.Daemon = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	Logger(nil, "Service started", "DEBUG")

	for {
		select {
		case err := <-done:
			// in daemon mode Run only returns once cancelled
			if err == nil {
				err = errors.New("sync loop exited")
			}
			Logger(err, "Service stopped unexpectedly", "WARN")
			// a non-zero exit code triggers the recovery actions
			return true, exitCodeUnexpected

		case c := <-requests:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus

			case svc.Stop, svc.Shutdown:
				waitHint := serviceStopTimeout + 5*time.Second
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(waitHint / time.Millisecond)}
				cancel()

				select {
				case <-done:
				case <-time.After(serviceStopTimeout):
					// files are replaced atomically and stale tmp files are
					// cleaned up on the next sync, so it's safe to exit
					Logger(errors.New("sync still running"), "Stopping without waiting for the current sync", "WARN")
				}
				Logger(nil, "Service stopped", "DEBUG")
				return false, 0

			default:
				Logger(fmt.Errorf("unexpected control request %d", c.Cmd), "Service", "WARN")
			}
		}
	}
}

func serviceCommand(name string) error {
	switch name {
	case "install":
		return installService()
	case "uninstall":
		return uninstallService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	}

	return fmt.Errorf("unknown command %q\n\n%s", name, serviceUsage)
}

func connectManager() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, fmt.Errorf("connecting to the service manager (run from an elevated prompt): %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to the service manager: %w", err)
	}

	return m, nil
}

func openService(m *mgr.Mgr) (*mgr.Service, error) {
	s, err := m.OpenService(serviceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, fmt.Errorf("service %s is not installed", serviceName)
	}
	if err != nil {
		return nil, fmt.Errorf("opening service %s: %w", serviceName, err)
	}

	return s, nil
}

// serviceArgs returns the command-line arguments stored with the service.
// -service is explicit rather than relying only on svc.IsWindowsService,
// which guesses from the parent process and session.
func serviceArgs() ([]string, error) {
	args := []string{"-service"}
	if configPath == "" {
		return args, nil
	}

	// the service runs in C:\Windows\System32, so relative paths won't work
	conf, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(conf); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}

	return append(args, "-config", conf), nil
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}

	args, err := serviceArgs()
	if err != nil {
		return err
	}

	m, err := connectManager()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed (run uninstall first)", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	}, args...)
	if err != nil {
		return fmt.Errorf("creating service: %w", err)
	}
	defer s.Close()

	// Start once the network is more likely to be up. Set separately (not
	// via mgr.Config) since CreateService would otherwise delete the service
	// on platforms that don't support it.
	delayed := windows.SERVICE_DELAYED_AUTO_START_INFO{IsDelayedAutoStartUp: 1}
	if err = windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_DELAYED_AUTO_START_INFO, (*byte)(unsafe.Pointer(&delayed))); err != nil {
		fmt.Fprintf(os.Stderr, "warning: delayed start not supported, using automatic start: %v\n", err)
	}

	// restart on failure, like Restart=always in the systemd unit
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, uint32((24 * time.Hour).Seconds()))
	if err == nil {
		err = s.SetRecoveryActionsOnNonCrashFailures(true)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: the service won't be restarted automatically if it fails: setting recovery actions: %v\n", err)
	}

	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		s.Delete()
		return fmt.Errorf("registering event log source: %w", err)
	}

	fmt.Printf("Installed service %s: %s %s\n", serviceName, exe, strings.Join(args, " "))
	fmt.Printf("Start it with: %s start\n", filepath.Base(exe))
	return nil
}

func uninstallService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()

	// a service that can't be stopped (e.g. hung while starting) is still
	// marked for deletion and removed once its process exits
	if err = stop(s); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; the service will be removed once it stops\n", err)
	}

	if err = s.Delete(); err != nil {
		return fmt.Errorf("removing service: %w", err)
	}

	if err = eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing event log source: %v\n", err)
	}

	fmt.Printf("Removed service %s\n", serviceName)
	return nil
}

func startService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()

	err = s.Start()
	if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		fmt.Printf("Service %s is already running\n", serviceName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("starting service: %w", err)
	}

	if err = waitForState(s, svc.Running); err != nil {
		return err
	}

	fmt.Printf("Started service %s\n", serviceName)
	return nil
}

func stopService() error {
	m, err := connectManager()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := openService(m)
	if err != nil {
		return err
	}
	defer s.Close()

	return stop(s)
}

// stop stops the service if it's running and waits until it has stopped.
func stop(s *mgr.Service) error {
	_, err := s.Control(svc.Stop)
	if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stopping service: %w", err)
	}

	if err = waitForState(s, svc.Stopped); err != nil {
		return err
	}

	fmt.Printf("Stopped service %s\n", serviceName)
	return nil
}

func waitForState(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(serviceCommandTimeout)

	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying service: %w", err)
		}

		if st.State == want {
			return nil
		}

		// stopped by itself while starting, e.g. because of a config error
		if want == svc.Running && st.State == svc.Stopped {
			reason := fmt.Sprintf("exit code %d/%d", st.Win32ExitCode, st.ServiceSpecificExitCode)
			if st.ServiceSpecificExitCode == exitCodeConfig {
				reason = "invalid or unreadable config file"
			}
			return fmt.Errorf("service stopped during startup (%s); check the Application event log for source %q",
				reason, serviceName)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service to be %s (currently %s)", stateName(want), stateName(st.State))
		}

		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}

	return fmt.Sprintf("in state %d", s)
}
