//go:build linux

package main

import (
	"os"

	"phaethon/util"
)

// enableParentDeathSignal is a startup sanity check only. Parent-death
// detection itself is driven by the worker→watchdog heartbeat pipe: the
// watchdog holds the read end, so a dead watchdog makes the periodic heartbeat
// write fail and the worker exits (see startWorkerHeartbeat).
//
// PR_SET_PDEATHSIG is deliberately not used: the signal is bound to the parent
// *thread* that forked this process, and Go recycles threads. On jf a healthy
// worker was SIGTERMed ~12s after start because that thread had been destroyed.
func enableParentDeathSignal() {
	if ppid := os.Getppid(); ppid <= 1 {
		util.LogModuleWarn("main", "worker: watchdog exited before startup completed, exiting")
		os.Exit(0)
	}
}
