//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
)

func isRIPWSProcess(pid int) bool {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(comm)) == "ripws"
}
