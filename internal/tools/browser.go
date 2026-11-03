package tools

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The crow's nest (rendering a page to find images its scripts add) needs a
// Chromium-family browser it can drive headless. An installed Chrome, Edge,
// Brave... is used as is; otherwise Google's chrome-headless-shell is
// downloaded on first use (about 100 MB) and kept current by refit.

const cftVersions = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// installedBrowser returns the first Chromium-family browser found, or "".
func installedBrowser() string {
	var cands []string
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Chromium.app/Contents/MacOS/Chromium",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Vivaldi.app/Contents/MacOS/Vivaldi",
		} {
			home, _ := os.UserHomeDir()
			cands = append(cands, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
		}
	case "windows":
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if base == "" {
				continue
			}
			for _, exe := range []string{
				`Google\Chrome\Application\chrome.exe`,
				`Microsoft\Edge\Application\msedge.exe`,
				`Chromium\Application\chrome.exe`,
				`BraveSoftware\Brave-Browser\Application\brave.exe`,
			} {
				cands = append(cands, filepath.Join(base, exe))
			}
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				cands = append(cands, p)
			}
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// browserName is e.g. "Google Chrome", or "headless Chrome" when we'd use our own.
func browserName(path string) string {
	if path == "" {
		return "headless Chrome"
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if pretty, ok := map[string]string{
		"chrome": "Google Chrome", "google-chrome": "Google Chrome", "google-chrome-stable": "Google Chrome",
		"msedge": "Microsoft Edge", "microsoft-edge": "Microsoft Edge",
		"brave": "Brave", "brave-browser": "Brave", "chromium-browser": "Chromium",
	}[name]; ok {
		return pretty
	}
	return name // macOS binaries already have their app's name
}

func headlessPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "mac-arm64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "windows/amd64", "windows/arm64":
		return "win64", nil
	case "linux/amd64":
		return "linux64", nil
	}
	return "", fmt.Errorf("no headless Chrome build for %s/%s; install Chrome or Chromium", runtime.GOOS, runtime.GOARCH)
}

func (m *Manager) headlessDir() string { return filepath.Join(m.dir, "chrome-headless-shell") }

// managedHeadless is the downloaded chrome-headless-shell, or "" if there's none.
func (m *Manager) managedHeadless() string {
	plat, err := headlessPlatform()
	if err != nil {
		return ""
	}
	p := filepath.Join(m.headlessDir(), "chrome-headless-shell-"+plat, exe("chrome-headless-shell"))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (m *Manager) headlessVersion() string {
	b, _ := os.ReadFile(filepath.Join(m.headlessDir(), "VERSION"))
	return strings.TrimSpace(string(b))
}

// BrowserProfilePrefix starts the name of the throwaway profile folder the
// crow's nest runs its browser with, which tells a leftover one apart from
// the user's own browser.
const BrowserProfilePrefix = "whydah-browser-"

// SignInProfileName is the folder, in the app folder, of the browser profile
// people sign in to sites with (see engine/signin.go). It's kept.
const SignInProfileName = "whydah-sign-in-profile"

// VisibleBrowser is the installed browser the sign-in window opens in, or ""
// if there's none: chrome-headless-shell can't show a window.
func (m *Manager) VisibleBrowser() string { return installedBrowser() }

// VisibleBrowserName is how VisibleBrowser is written, or "" if there's none.
func (m *Manager) VisibleBrowserName() string {
	if p := installedBrowser(); p != "" {
		return browserName(p)
	}
	return ""
}

// BrowserReady reports whether a browser can be used without a download.
func (m *Manager) BrowserReady() bool {
	return installedBrowser() != "" || m.managedHeadless() != ""
}

// Browser returns a browser to render pages with, downloading
// chrome-headless-shell first if no Chromium-family browser is installed.
func (m *Manager) Browser(ctx context.Context) (string, error) {
	if p := installedBrowser(); p != "" {
		return p, nil
	}
	m.browserMu.Lock()
	defer m.browserMu.Unlock()
	if p := m.managedHeadless(); p != "" {
		return p, nil
	}
	if err := m.installHeadless(ctx, ""); err != nil {
		m.update(func(s *Status) { s.Stage = nil })
		return "", fmt.Errorf("getting headless Chrome: %w", err)
	}
	m.update(func(s *Status) {
		s.Stage = nil
		s.HeadlessVer = m.headlessVersion()
		s.Update.Supported = true
	})
	return m.managedHeadless(), nil
}

// latestHeadless reads the current stable chrome-headless-shell version and
// its download link for this platform.
func latestHeadless(ctx context.Context) (version, link string, err error) {
	plat, err := headlessPlatform()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cftVersions, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("Chrome for Testing: %s", resp.Status)
	}
	var v struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", "", fmt.Errorf("Chrome for Testing: %w", err)
	}
	stable := v.Channels["Stable"]
	for _, d := range stable.Downloads["chrome-headless-shell"] {
		if d.Platform == plat {
			return stable.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("no chrome-headless-shell for %s", plat)
}

// installHeadless downloads the latest chrome-headless-shell, unless current
// is already it, unpacking beside the old copy and swapping it in.
func (m *Manager) installHeadless(ctx context.Context, current string) error {
	version, link, err := latestHeadless(ctx)
	if err != nil || version == current {
		return err
	}
	tmp, err := os.MkdirTemp(m.dir, "fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, "archive.zip")
	if err := m.downloadFile(ctx, link, file, "headless Chrome"); err != nil {
		return err
	}
	m.stage("setup.unpacking", "tool", "headless Chrome")
	out := filepath.Join(tmp, "x")
	if err := extractZipTree(file, out); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	dir := m.headlessDir()
	old := dir + ".old"
	os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(out, dir); err != nil {
		_ = os.Rename(old, dir)
		return err
	}
	return os.RemoveAll(old)
}

// extractZipTree unpacks a whole zip, keeping its folders, file modes and symlinks.
func extractZipTree(file, dest string) error {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer zr.Close()
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for _, f := range zr.File {
		p := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(p, root) {
			return fmt.Errorf("bad path in archive: %s", f.Name)
		}
		mode := f.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := os.Symlink(string(target), p); err != nil {
				return err
			}
			continue
		}
		w, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()|0o600)
		if err == nil {
			_, err = io.Copy(w, rc)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
