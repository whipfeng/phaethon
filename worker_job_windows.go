//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"phaethon/util"
)

// workerJob binds worker processes to the watchdog's lifetime.
//
// A job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE terminates every assigned
// process when the last job handle closes — which happens when the watchdog
// dies, crashes or panics. This is the only kernel-level guarantee that a
// worker orphaned by a watchdog failure cannot keep holding the instance lock;
// without it a failed handoff left an orphan worker behind and the rollback
// worker exited with instanceConflictExitCode, stopping the node for good.
//
// The watchdog creates one job per process and assigns only its own worker
// children. The handoff watchdog is deliberately NOT assigned: it is started by
// the old watchdog and must outlive it, so putting it in the old job would kill
// it the moment the old watchdog exits.
type workerJob struct {
	handle windows.Handle
}

// createWorkerJob creates the per-watchdog job. Any failure is non-fatal: the
// watchdog keeps supervising without the extra OS-level guarantee, exactly as
// before, and only logs a warning.
func createWorkerJob() *workerJob {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		util.LogModuleWarn("main", "watchdog: create job object failed, worker lifetime not bound to watchdog: %v", err)
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		util.LogModuleWarn("main", "watchdog: configure job object failed, worker lifetime not bound to watchdog: %v", err)
		_ = windows.CloseHandle(handle)
		return nil
	}
	return &workerJob{handle: handle}
}

// assign puts pid into the job. A process already in a non-nestable job (old
// Windows without nested jobs) cannot be assigned; that is logged and tolerated.
func (j *workerJob) assign(pid int) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open worker pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(j.handle, handle); err != nil {
		return fmt.Errorf("assign worker pid %d to job: %w", pid, err)
	}
	return nil
}

// close releases the job handle. Closing the last handle triggers
// KILL_ON_JOB_CLOSE for every worker still assigned.
func (j *workerJob) close() {
	if j == nil || j.handle == 0 {
		return
	}
	_ = windows.CloseHandle(j.handle)
	j.handle = 0
}
