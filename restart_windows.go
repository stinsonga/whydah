package main

import (
	"os"
	"os/exec"

	"whydah/internal/tools"
)

// restart starts the executable at exe (the updated one) with the same
// arguments, for this process to exit. Windows has no exec, and the new
// process mustn't be in this one's job, which ends with it.
func restart(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), restartedEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return tools.StartOutside(cmd)
}
