//go:build darwin

package tun

import "os/exec"

// CleanupResidual removes leftover routes from a previous abnormal exit.
func CleanupResidual() {
	exec.Command("route", "-n", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("route", "-n", "delete", "-net", "128.0.0.0/1").Run()
}
