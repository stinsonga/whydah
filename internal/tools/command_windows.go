package tools

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
)

const createNoWindow = 0x08000000

// Command builds an exec.Cmd that doesn't flash a console window and whose
// cancellation kills the whole process tree. The standalone yt-dlp binary is
// a launcher that runs Python in a child process (which in turn runs ffmpeg),
// so killing only the launcher would leave the download running.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if kill.Run() != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return cmd
}
