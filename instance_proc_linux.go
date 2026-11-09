//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProcessExe returns the executable path of pid with the " (deleted)"
// suffix stripped, or "" if it cannot be read (gone, permission, hidepid).
func readProcessExe(pid int) string {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, " (deleted)")
}

// readProcessParent returns the parent pid of pid, or 0 on failure.
func readProcessParent(pid int) int {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The comm field may contain spaces and parentheses; everything after the
	// last ')' is "state ppid pgrp ...".
	s := string(stat)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}
