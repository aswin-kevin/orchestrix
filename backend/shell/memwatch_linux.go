//go:build linux

package shell

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// procDir is where Linux exposes per-process statistics.
const procDir = "/proc"

// watchMemory samples the summed resident memory of process group pgid every
// DefaultMemSample and calls onExceed once if it goes above limit. It returns
// when done is closed or after calling onExceed.
func watchMemory(pgid int, limit int64, done <-chan struct{}, onExceed func()) {
	ticker := time.NewTicker(DefaultMemSample)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if groupRSS(pgid) > limit {
				onExceed()
				return
			}
		}
	}
}

// groupRSS returns the summed resident set size, in bytes, of every process
// whose process group is pgid. Processes that vanish mid-scan are skipped.
func groupRSS(pgid int) int64 {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	pageSize := int64(os.Getpagesize())
	var total int64
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue // not a process directory
		}
		dir := procDir + "/" + entry.Name()
		if group, ok := processGroup(dir); ok && group == pgid {
			total += residentPages(dir) * pageSize
		}
	}
	return total
}

// processGroup reads the pgrp field of <dir>/stat. The command name in field 2
// may contain spaces and parentheses, so fields are counted after its last
// ')'.
func processGroup(dir string) (int, bool) {
	data, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return 0, false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, false
	}
	// After the name: state, ppid, pgrp, ...
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 3 {
		return 0, false
	}
	group, err := strconv.Atoi(fields[2])
	return group, err == nil
}

// residentPages reads the resident field (the second) of <dir>/statm.
func residentPages(dir string) int64 {
	data, err := os.ReadFile(dir + "/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(fields[1], 10, 64)
	return pages
}
