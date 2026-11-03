//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restart replaces this process with the executable at exe (the updated
// one), with the same arguments.
func restart(exe string) error {
	return syscall.Exec(exe, os.Args, append(os.Environ(), restartedEnv+"=1"))
}
