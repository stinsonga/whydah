package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"whydah/internal/i18n"
)

// SupportedBrowsers are the names yt-dlp accepts for --cookies-from-browser.
var SupportedBrowsers = []string{"chrome", "firefox", "safari", "edge", "brave", "chromium", "opera", "vivaldi"}

// DetectBrowsers returns the supported browsers that appear to be installed,
// judged by the presence of their profile directory.
func DetectBrowsers() []string {
	home, _ := os.UserHomeDir()
	var dirs map[string][]string
	switch runtime.GOOS {
	case "darwin":
		dirs = macProfileDirs(home)
		dirs["safari"] = []string{"/Applications/Safari.app"}
	case "windows":
		local, roaming := os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA")
		dirs = map[string][]string{
			"chrome":   {filepath.Join(local, "Google", "Chrome", "User Data")},
			"firefox":  {filepath.Join(roaming, "Mozilla", "Firefox")},
			"edge":     {filepath.Join(local, "Microsoft", "Edge", "User Data")},
			"brave":    {filepath.Join(local, "BraveSoftware", "Brave-Browser", "User Data")},
			"chromium": {filepath.Join(local, "Chromium", "User Data")},
			"opera":    {filepath.Join(roaming, "Opera Software", "Opera Stable")},
			"vivaldi":  {filepath.Join(local, "Vivaldi", "User Data")},
		}
	default:
		cfg := filepath.Join(home, ".config")
		dirs = map[string][]string{
			"chrome":   {filepath.Join(cfg, "google-chrome")},
			"firefox":  {filepath.Join(home, ".mozilla", "firefox"), filepath.Join(home, "snap", "firefox")},
			"edge":     {filepath.Join(cfg, "microsoft-edge")},
			"brave":    {filepath.Join(cfg, "BraveSoftware", "Brave-Browser")},
			"chromium": {filepath.Join(cfg, "chromium")},
			"opera":    {filepath.Join(cfg, "opera")},
			"vivaldi":  {filepath.Join(cfg, "vivaldi")},
		}
	}
	var found []string
	for _, b := range SupportedBrowsers {
		for _, d := range dirs[b] {
			if _, err := os.Stat(d); err == nil {
				found = append(found, b)
				break
			}
		}
	}
	return found
}

func macProfileDirs(home string) map[string][]string {
	as := filepath.Join(home, "Library", "Application Support")
	return map[string][]string{
		"chrome":   {filepath.Join(as, "Google", "Chrome")},
		"firefox":  {filepath.Join(as, "Firefox")},
		"safari":   {filepath.Join(home, "Library", "Containers", "com.apple.Safari", "Data", "Library", "Cookies")},
		"edge":     {filepath.Join(as, "Microsoft Edge")},
		"brave":    {filepath.Join(as, "BraveSoftware", "Brave-Browser")},
		"chromium": {filepath.Join(as, "Chromium")},
		"opera":    {filepath.Join(as, "com.operasoftware.Opera")},
		"vivaldi":  {filepath.Join(as, "Vivaldi")},
	}
}

// CookieAccess reports whether macOS privacy protection stops us reading the
// browser's profile. yt-dlp and gallery-dl see that as an empty folder and
// say they "could not find" the cookies, which sends people looking in the
// wrong place.
func CookieAccess(browser string) error {
	if browser == "" || runtime.GOOS != "darwin" {
		return nil
	}
	home, _ := os.UserHomeDir()
	for _, d := range macProfileDirs(home)[browser] {
		if _, err := os.ReadDir(d); errors.Is(err, fs.ErrPermission) {
			e := i18n.New("error.cookies_blocked", "browser", BrowserName(browser))
			if l := launcherName(); l != "" {
				e.Args["launcher"] = l
				return e.WithHint("hint.cookies_blocked")
			}
			return e.WithHint("hint.cookies_blocked_unknown")
		}
	}
	return nil
}

// BrowserName is how a browser is written, e.g. "Firefox" for "firefox".
func BrowserName(browser string) string {
	switch browser {
	case "":
		return ""
	case "edge":
		return "Microsoft Edge"
	}
	return strings.ToUpper(browser[:1]) + browser[1:]
}

// launcherName is the app macOS holds responsible for us: the one we were
// started from, or "" if it's one we don't know.
func launcherName() string {
	switch os.Getenv("__CFBundleIdentifier") {
	case "local.whydah":
		return "Whydah"
	case "com.microsoft.VSCode":
		return "Visual Studio Code"
	case "com.apple.Terminal":
		return "Terminal"
	case "com.googlecode.iterm2":
		return "iTerm"
	case "dev.warp.Warp-Stable":
		return "Warp"
	case "com.mitchellh.ghostty":
		return "Ghostty"
	}
	return ""
}
