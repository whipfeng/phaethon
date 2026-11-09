//go:build !linux && !darwin && !windows

package main

import "fmt"

func takeoverInstanceLock(string) error {
	return fmt.Errorf("--takeover is not supported on this platform")
}
