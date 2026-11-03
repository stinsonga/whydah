package web

import (
	"os/exec"
	"path/filepath"
	"runtime"

	"whydah/internal/tools"
)

// OpenPath opens a URL, file or folder with the OS default handler.
func OpenPath(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return tools.StartOutside(exec.Command("open", target))
	case "windows":
		return tools.StartOutside(exec.Command("rundll32", "url.dll,FileProtocolHandler", target))
	default:
		return tools.StartOutside(exec.Command("xdg-open", target))
	}
}

// RevealFile shows the file selected in Finder/Explorer, or opens its folder on Linux.
func RevealFile(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return tools.StartOutside(exec.Command("open", "-R", path))
	case "windows":
		return tools.StartOutside(exec.Command("explorer", "/select,", path))
	default:
		return OpenPath(filepath.Dir(path))
	}
}
