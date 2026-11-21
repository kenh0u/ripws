//go:build openbsd

package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func isRIPWSProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return filepath.Base(strings.TrimSpace(string(out))) == "ripws"
}
