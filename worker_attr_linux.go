//go:build linux

package main

import (
	"os"
	"time"

	"phaethon/util"
)

// enableParentDeathSignal guards against an orphaned worker.
//
// Preferred path: the watchdog spawns us with stdout as a pipe whose read end
// it holds, so a dead watchdog makes the periodic heartbeat write fail and
// startWorkerHeartbeat exits (event-driven, no polling, thread-independent).
//
// Fallback path: if stdout is NOT a pipe, we were not spawned by a watchdog —
// e.g. someone ran ./phaethon inside a built-in SSH session that inherited
// PHAETHON_WORKER=1. In that case the heartbeat write never fails, so poll
// getppid() and exit once we get re-parented (pid 1). This is exactly the
// failure that left jf running an unsupervised worker.
//
// PR_SET_PDEATHSIG is deliberately not used: the signal is bound to the parent
// *thread* that forked this process, and Go recycles threads (a healthy worker
// was SIGTERMed ~12s after start on jf).
func enableParentDeathSignal() {
	ppid := os.Getppid()
	if ppid <= 1 {
		util.LogModuleWarn("main", "worker: no watchdog parent at startup (ppid=%d), exiting", ppid)
		os.Exit(0)
	}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
		util.LogModuleInfo("main", "worker: watchdog pipe detected, parent death via heartbeat")
		return
	}
	util.LogModuleWarn("main", "worker: stdout is not a watchdog pipe, monitoring parent pid %d", ppid)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != ppid {
				util.LogModuleWarn("main", "worker: watchdog (pid %d) exited, shutting down", ppid)
				os.Exit(0)
			}
		}
	}()
}
