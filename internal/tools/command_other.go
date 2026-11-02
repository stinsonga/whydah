//go:build !windows

package tools

import (
	"context"
	"os/exec"
	"syscall"
)

// Command builds an exec.Cmd whose cancellation kills the whole process tree.
// The standalone yt-dlp binary is a launcher that runs Python in a child
// process (which in turn runs ffmpeg), so killing only the launcher would
// leave the download running.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
