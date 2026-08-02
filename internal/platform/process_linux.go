//go:build linux

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// StartToken returns the kernel start-time token of a live process:
// /proc/<pid>/stat field 22 (clock ticks since boot). The token is
// stable for the process lifetime and changes when the PID is reused.
func StartToken(pid int) (uint64, error) {
	_, start, err := procStatFacts(pid)
	return start, err
}

// ProcessGroup returns the process-group ID of a live process:
// /proc/<pid>/stat field 5.
func ProcessGroup(pid int) (int, error) {
	pgrp, _, err := procStatFacts(pid)
	return pgrp, err
}

// procStatFacts parses pgrp (field 5) and starttime (field 22) from
// /proc/<pid>/stat. The comm field may contain spaces and parentheses,
// so parsing starts after the last ')'.
func procStatFacts(pid int) (pgrp int, starttime uint64, err error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+1 >= len(s) {
		return 0, 0, fmt.Errorf("platform: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return 0, 0, fmt.Errorf("platform: short /proc/%d/stat", pid)
	}
	pgrp, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, fmt.Errorf("platform: bad pgrp in /proc/%d/stat: %w", pid, err)
	}
	starttime, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("platform: bad starttime in /proc/%d/stat: %w", pid, err)
	}
	return pgrp, starttime, nil
}
