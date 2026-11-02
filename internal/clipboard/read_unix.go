//go:build !windows

package clipboard

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// read shells out to the platform clipboard tool: pbpaste on macOS,
// wl-paste / xclip / xsel on Linux.
func read() (string, error) {
	var candidates [][]string
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"pbpaste"}}
	} else {
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-paste", "--no-newline"})
		}
		candidates = append(candidates,
			[]string{"xclip", "-selection", "clipboard", "-o"},
			[]string{"xsel", "--clipboard", "--output"},
		)
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(path, c[1:]...).Output()
		if err != nil {
			// Empty or non-text clipboards make these tools exit non-zero.
			return "", nil
		}
		return string(out), nil
	}
	return "", errors.New("no clipboard tool found (install wl-clipboard, xclip or xsel)")
}
