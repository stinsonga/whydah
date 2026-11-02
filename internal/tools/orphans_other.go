//go:build !windows

package tools

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapOrphans kills helpers left running by a Whydah that crashed or was
// force-quit. A helper only notices its parent is gone when it next writes
// output, so one that's quiet for a while (yt-dlp waiting on an ffmpeg merge,
// a headless browser) lives on, and a resumed job could race it for its files.
// One is recognised as a process group leader (Command starts each helper
// in its own group) whose parent is gone and that runs one of our helpers,
// or a browser with the crow's nest's throwaway profile: the browser's own
// path won't do, since the user's browser looks the same from here.
func reapOrphans(ctx context.Context, helpers []string) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,command=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "1" || f[0] != f[2] {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		if cmd := strings.Join(f[3:], " "); ours(cmd, helpers) {
			slog.Info("stopping helper left from an earlier run", "pid", pid, "command", cmd)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			if dir := profileDir.FindStringSubmatch(cmd); dir != nil {
				go removeProfile(dir[1])
			}
		}
	}
}

// profileDir finds a crow's nest browser's throwaway profile (paths in the
// temp folder have no spaces on macOS or Linux).
var profileDir = regexp.MustCompile(`--user-data-dir=(\S*` + regexp.QuoteMeta(string(filepath.Separator)+BrowserProfilePrefix) + `\S*)`)

// removeProfile deletes a leftover browser profile once its processes have let go.
func removeProfile(dir string) {
	if !strings.HasPrefix(filepath.Base(dir), BrowserProfilePrefix) {
		return
	}
	for range 10 {
		time.Sleep(300 * time.Millisecond)
		if os.RemoveAll(dir) == nil {
			return
		}
	}
}

func ours(cmd string, helpers []string) bool {
	if strings.Contains(cmd, "--user-data-dir=") && strings.Contains(cmd, string(filepath.Separator)+BrowserProfilePrefix) {
		return true
	}
	for _, h := range helpers {
		if h != "" && strings.HasPrefix(cmd, h) {
			return true
		}
	}
	return false
}
