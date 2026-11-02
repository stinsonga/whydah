package tools

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

// Command builds an exec.Cmd that doesn't flash a console window and whose
// cancellation kills the whole process tree. The standalone yt-dlp binary is
// a launcher that runs Python in a child process (which in turn runs ffmpeg),
// so killing only the launcher would leave the download running.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	joinJob() // so the helper can't outlive Whydah
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

// StartOutside starts a program that should outlive Whydah, such as the
// browser a link opens in: it breaks away from Whydah's job (see joinJob).
// If a job Whydah was started in doesn't allow that, it starts in the job.
func StartOutside(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob}
	err := cmd.Start()
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return exec.Command(name, args...).Start()
	}
	return err
}
