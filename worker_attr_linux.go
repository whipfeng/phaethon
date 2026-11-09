//go:build linux

package main

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"phaethon/util"
)

// enableParentDeathSignal lets this worker receive SIGTERM when the watchdog
// dies, so a kill -9 of the watchdog never leaves an orphan holding the TUN
// device, routes or the database.
//
// PR_SET_PDEATHSIG is tied to the parent thread that forked this process, not
// to the parent process as a whole. runWatchdogMode pins itself with
// runtime.LockOSThread so that thread stays alive for the watchdog's lifetime.
//
// The parent may already have died before this function runs; the kernel does
// not deliver the signal retroactively, so check getppid() around the prctl
// and exit if we have been re-parented to init.
func enableParentDeathSignal() {
	runtime.LockOSThread() // intentionally never unlocked: main goroutine lives on

	if os.Getppid() == 1 {
		util.LogModuleWarn("main", "worker: watchdog exited before pdeathsig was armed, exiting")
		os.Exit(0)
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		util.LogModuleWarn("main", "worker: set parent-death signal failed: %v", err)
		return
	}
	if os.Getppid() == 1 {
		util.LogModuleWarn("main", "worker: watchdog exited while arming pdeathsig, exiting")
		os.Exit(0)
	}
	util.LogModuleInfo("main", "worker: parent-death signal armed (SIGTERM on watchdog exit)")
}
