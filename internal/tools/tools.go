// Package tools makes sure yt-dlp, gallery-dl, ffmpeg/ffprobe and deno are
// available, downloading platform builds into the cache dir on first run.
//
// yt-dlp and gallery-dl are managed copies so they can be updated from the
// app when a new release fixes a site (see update.go). ffmpeg and deno are
// taken from PATH when present, since any recent build works.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"whydah/internal/events"
)

type Paths struct {
	YtDlp     string `json:"yt_dlp"`
	FFmpegDir string `json:"ffmpeg_dir"` // dir containing ffmpeg and ffprobe; "" if unavailable
	Deno      string `json:"deno"`       // "" if unavailable
	GalleryDL string `json:"gallery_dl"` // "" if unavailable
}

type Status struct {
	Ready        bool         `json:"ready"`
	Stage        string       `json:"stage"`
	Warnings     []string     `json:"warnings"`
	Error        string       `json:"error"` // fatal: nothing can be downloaded
	YtDlpVersion string       `json:"yt_dlp_version"`
	GalleryDLVer string       `json:"gallery_dl_version"` // e.g. "1.32.19:2026.11.01" (release:build)
	HeadlessVer  string       `json:"headless_version"`   // downloaded chrome-headless-shell; "" if none
	Browser      string       `json:"browser"`            // what the crow's nest renders with
	Paths        Paths        `json:"paths"`
	Update       UpdateStatus `json:"update"`
}

type Manager struct {
	dir     string
	hub     *events.Hub
	channel func() string // yt-dlp release channel to check/update against
	mu      sync.Mutex
	st      Status
	ready   chan struct{}

	// yt-dlp and gallery-dl runs hold the read lock; installing an update takes
	// the write lock, so it waits for running downloads and new ones wait for it.
	toolLock  sync.RWMutex
	lastCheck time.Time  // last automatic (failure-triggered) check
	browserMu sync.Mutex // one chrome-headless-shell download at a time
	refitting []func()   // called before a refit installs anything
}

// BeforeRefit registers f to run once a refit has waited for running tools
// to finish and before it installs anything, e.g. to quit a browser kept
// running between uses.
func (m *Manager) BeforeRefit(f func()) { m.refitting = append(m.refitting, f) }

// New creates a Manager; channel returns the configured update channel.
func New(cacheDir string, hub *events.Hub, channel func() string) *Manager {
	return &Manager{dir: filepath.Join(cacheDir, "bin"), hub: hub, channel: channel, ready: make(chan struct{})}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.st
	st.Warnings = append([]string(nil), m.st.Warnings...)
	return st
}

// Wait blocks until setup finished, returning the tool paths or the fatal error.
func (m *Manager) Wait(ctx context.Context) (Paths, error) {
	select {
	case <-m.ready:
	case <-ctx.Done():
		return Paths{}, ctx.Err()
	}
	st := m.Status()
	if st.Error != "" {
		return st.Paths, fmt.Errorf("%s", st.Error)
	}
	return st.Paths, nil
}

func (m *Manager) Start(ctx context.Context) {
	go func() {
		defer close(m.ready)
		m.setup(ctx)
	}()
}

func (m *Manager) update(f func(*Status)) {
	m.mu.Lock()
	f(&m.st)
	st := m.st
	m.mu.Unlock()
	m.hub.Publish(events.ToolsStatus, st)
}

func (m *Manager) stage(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	slog.Info("tools", "stage", msg)
	m.update(func(s *Status) { s.Stage = msg })
}

func (m *Manager) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	slog.Warn("tools", "warning", msg)
	m.update(func(s *Status) { s.Warnings = append(s.Warnings, msg) })
}

func (m *Manager) setup(ctx context.Context) {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		m.update(func(s *Status) { s.Error = err.Error() })
		return
	}
	var p Paths

	yt, err := m.ensureYtDlp(ctx)
	if err != nil {
		m.update(func(s *Status) { s.Error = "could not get yt-dlp: " + err.Error(); s.Stage = "" })
		return
	}
	p.YtDlp = yt

	if dir, err := m.ensureFFmpeg(ctx); err != nil {
		m.warn("ffmpeg unavailable (%v): audio conversion and high-quality video merging are disabled", err)
	} else {
		p.FFmpegDir = dir
	}

	if deno, err := m.ensureDeno(ctx); err != nil {
		m.warn("deno unavailable (%v): some YouTube formats may be missing", err)
	} else {
		p.Deno = deno
	}

	var gdlVersion string
	if gdl, err := m.ensureGalleryDL(ctx); err != nil {
		m.warn("gallery-dl unavailable (%v): photos are found by scanning the page only, which misses galleries on sites like Instagram or Reddit", err)
	} else {
		p.GalleryDL, gdlVersion = gdl, m.version(ctx, gdl)
	}

	// Before anything can start a download: a resumed job mustn't race a
	// leftover yt-dlp for its .part file.
	reapOrphans(ctx, []string{m.dir + string(filepath.Separator), p.YtDlp, p.GalleryDL})

	version := m.version(ctx, yt)
	m.update(func(s *Status) {
		s.Paths, s.YtDlpVersion, s.GalleryDLVer, s.Ready, s.Stage = p, version, gdlVersion, true, ""
		s.HeadlessVer = m.headlessVersion()
		s.Browser = browserName(installedBrowser())
		s.Update.Supported = m.managed(yt) || m.managed(p.GalleryDL) || m.managedHeadless() != ""
	})
}

// managed reports whether path is a copy we downloaded (and so may update).
// A system install is updated by whatever installed it.
func (m *Manager) managed(path string) bool {
	return path != "" && strings.HasPrefix(path, m.dir)
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (m *Manager) ensureYtDlp(ctx context.Context) (string, error) {
	dest := filepath.Join(m.dir, exe("yt-dlp"))
	if exists(dest) {
		return dest, nil
	}
	url, err := ytDlpURL()
	if err == nil {
		m.stage("Downloading yt-dlp")
		if err = m.downloadFile(ctx, url, dest, "yt-dlp"); err == nil {
			return dest, os.Chmod(dest, 0o755)
		}
	}
	// Last resort: a system install (may be outdated, but better than nothing).
	if p, lerr := exec.LookPath("yt-dlp"); lerr == nil {
		m.warn("using system yt-dlp at %s (%v)", p, err)
		return p, nil
	}
	return "", err
}

func (m *Manager) ensureFFmpeg(ctx context.Context) (string, error) {
	if ff, err := exec.LookPath(exe("ffmpeg")); err == nil {
		dir := filepath.Dir(ff)
		if exists(filepath.Join(dir, exe("ffprobe"))) {
			return dir, nil
		}
	}
	dir := filepath.Join(m.dir, "ffmpeg")
	if exists(filepath.Join(dir, exe("ffmpeg"))) && exists(filepath.Join(dir, exe("ffprobe"))) {
		return dir, nil
	}
	archives, err := ffmpegArchives()
	if err != nil {
		return "", err
	}
	for _, a := range archives {
		m.stage("Downloading %s (first run only)", a.label)
		if err := m.fetchArchive(ctx, a, dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (m *Manager) ensureDeno(ctx context.Context) (string, error) {
	if p, err := exec.LookPath(exe("deno")); err == nil {
		return p, nil
	}
	dir := filepath.Join(m.dir, "deno")
	dest := filepath.Join(dir, exe("deno"))
	if exists(dest) {
		return dest, nil
	}
	a, err := denoArchive()
	if err != nil {
		return "", err
	}
	m.stage("Downloading %s (first run only)", a.label)
	if err := m.fetchArchive(ctx, a, dir); err != nil {
		return "", err
	}
	return dest, nil
}

func (m *Manager) ensureGalleryDL(ctx context.Context) (string, error) {
	dest := filepath.Join(m.dir, exe("gallery-dl"))
	if exists(dest) {
		return dest, nil
	}
	asset, err := galleryDLAsset()
	if err == nil {
		var tag string
		if tag, err = latestRelease(ctx, galleryRepo); err == nil {
			m.stage("Downloading gallery-dl (first run only)")
			err = m.downloadFile(ctx, galleryDLURL(tag, asset), dest, "gallery-dl")
		}
		if err == nil {
			return dest, nil
		}
	}
	// No build for this platform (e.g. Intel Macs), but maybe one from pip or Homebrew.
	if p, lerr := exec.LookPath(exe("gallery-dl")); lerr == nil {
		return p, nil
	}
	return "", err
}

// version returns a tool's --version output (yt-dlp and gallery-dl).
func (m *Manager) version(ctx context.Context, bin string) string {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := Command(vctx, bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
