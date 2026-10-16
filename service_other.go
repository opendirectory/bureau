//go:build !windows

package main

import "fmt"

const serviceUsage = "On Linux, use the systemd unit (bureau.service) to run bureau as a service.\n"

func isWindowsService() (bool, error) {
	return false, nil
}

func runService() error {
	return fmt.Errorf("Windows services are not supported on this platform")
}

func serviceCommand(name string) error {
	return fmt.Errorf("unknown command %q (service commands are only available on Windows)", name)
}
