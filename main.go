package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var (
	C Config

	// explicit config file (-config); empty = search the standard locations
	configPath string

	// -service: run under the Windows service manager (set by install)
	serviceFlag bool

	// Overridable in tests
	syncOnce = Summon
)

func main() {
	flag.StringVar(&configPath, "config", "", "path to bureau.yaml (default: search the standard locations)")
	flag.BoolVar(&serviceFlag, "service", false, "run as a Windows service (added by install)")
	flag.Usage = usage
	flag.Parse()

	// started by the Windows service control manager: either via the flag
	// stored by install, or detected for services registered by hand
	isService := serviceFlag
	var err error
	if !isService {
		isService, err = isWindowsService()
		Logger(err, "Failed to detect whether running as a service", "FATAL")
	}
	if isService {
		err = runService()
		Logger(err, "Service failed", "FATAL")
		return
	}

	switch flag.NArg() {
	case 0:
	case 1:
		err = serviceCommand(flag.Arg(0))
		Logger(err, flag.Arg(0)+" failed", "FATAL")
		return
	default:
		flag.Usage()
		os.Exit(2)
	}

	err = LoadConfig()
	Logger(err, "Error reading config file", "FATAL")

	// stop cleanly on Ctrl+C / SIGTERM; a second signal exits immediately
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	err = Run(ctx)
	Logger(err, "Sync failed", "FATAL")
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "Usage: %s [-config path] [command]\n\n", filepath.Base(os.Args[0]))
	fmt.Fprintln(out, "Without a command, bureau syncs once, or continuously in daemon mode.")
	fmt.Fprintln(out)
	fmt.Fprint(out, serviceUsage)
	fmt.Fprintln(out, "\nOptions:")
	flag.PrintDefaults()
}

// LoadConfig reads the bureau config into C.
func LoadConfig() error {
	var err error
	C, err = ConfigInit()
	return err
}

// Run syncs once or, in daemon mode, every update_interval seconds until ctx
// is cancelled. A sync in progress is allowed to finish.
func Run(ctx context.Context) error {
	for {
		err := syncOnce()

		// Cleanup
		Tpl = nil

		if !C.Daemon {
			return err
		}

		// in daemon mode transient errors are retried
		Logger(err, "Sync failed", "WARN")

		timer := time.NewTimer(time.Duration(C.Update) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			Logger(nil, "Stopping", "DEBUG")
			return nil
		case <-timer.C:
		}
	}
}
