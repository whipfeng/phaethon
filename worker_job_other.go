//go:build !windows

package main

// workerJob is a no-op off Windows. Unix workers already detect a dead
// watchdog through the stdout protocol pipe (heartbeat write failure) and
// enableParentDeathSignal's parent-pid poll, so no OS job object is needed.
type workerJob struct{}

func createWorkerJob() *workerJob { return nil }

func (j *workerJob) assign(int) error { return nil }

func (j *workerJob) close() {}
