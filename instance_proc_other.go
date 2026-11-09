//go:build !linux

package main

// readProcessExe is unavailable without /proc. Callers fall back to the path
// recorded in the instance lock file.
func readProcessExe(int) string { return "" }

// readProcessParent is unavailable without /proc.
func readProcessParent(int) int { return 0 }
