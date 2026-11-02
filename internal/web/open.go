package web

import (
	"path/filepath"
	"runtime"

	"whydah/internal/tools"
)

// OpenPath opens a URL, file or folder with the OS default handler.
func OpenPath(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return tools.StartOutside("open", target)
	case "windows":
		return tools.StartOutside("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return tools.StartOutside("xdg-open", target)
	}
}

// RevealFile shows the file selected in Finder/Explorer, or opens its folder on Linux.
func RevealFile(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return tools.StartOutside("open", "-R", path)
	case "windows":
		return tools.StartOutside("explorer", "/select,", path)
	default:
		return OpenPath(filepath.Dir(path))
	}
}
