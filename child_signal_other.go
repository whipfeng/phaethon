//go:build !linux && !darwin && !windows

package main

import "os"

func gracefulExitSignal() os.Signal {
	return os.Kill
}
