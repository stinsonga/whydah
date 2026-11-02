package web

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// OpenPath opens a URL, file or folder with the OS default handler.
func OpenPath(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

// RevealFile shows the file selected in Finder/Explorer, or opens its folder on Linux.
func RevealFile(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	default:
		return OpenPath(filepath.Dir(path))
	}
}
