package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"whydah/internal/events"
)

const (
	checkInterval   = 6 * time.Hour
	autoCheckMinGap = 10 * time.Minute // between failure-triggered checks
)

var releaseRepos = map[string]string{
	"stable":  "yt-dlp/yt-dlp",
	"nightly": "yt-dlp/yt-dlp-nightly-builds",
}

// PendingUpdate is a newer release of one managed tool.
type PendingUpdate struct {
	Tool    string `json:"tool"`
	Current string `json:"current"`
	Latest  string `json:"latest"`
}

// UpdateStatus describes updates to the managed tools (yt-dlp, gallery-dl,
// and chrome-headless-shell once the crow's nest has downloaded it).
// Checking is automatic; installing ("refit") only happens when the user asks.
type UpdateStatus struct {
	Supported      bool            `json:"supported"` // false when neither tool is a copy we manage
	Channel        string          `json:"channel"`   // yt-dlp channel; gallery-dl only has dated builds
	Latest         string          `json:"latest"`    // newest yt-dlp on Channel
	GalleryLatest  string          `json:"gallery_latest"`
	HeadlessLatest string          `json:"headless_latest"`
	Available      bool            `json:"available"`
	Pending        []PendingUpdate `json:"pending"`
	CheckedAt      time.Time       `json:"checked_at"`
	CheckError     string          `json:"check_error"`
	State          string          `json:"state"`     // "", "checking", "waiting" (for running downloads), "installing"
	Error          string          `json:"error"`     // last install failure
	Installed      string          `json:"installed"` // versions after the last successful refit, for a confirmation
}

// Use marks a yt-dlp or gallery-dl process as running, so an update waits
// for it. Call the returned func once the process has exited.
func (m *Manager) Use() func() {
	m.toolLock.RLock()
	return m.toolLock.RUnlock
}

// WatchForUpdates checks once setup is done and then every checkInterval.
func (m *Manager) WatchForUpdates(ctx context.Context) {
	if _, err := m.Wait(ctx); err != nil {
		return
	}
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		_ = m.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckSoon runs a background check after a failed download, since a new
// release often fixes a site that stopped working. Rate-limited.
func (m *Manager) CheckSoon() {
	m.mu.Lock()
	if time.Since(m.lastCheck) < autoCheckMinGap {
		m.mu.Unlock()
		return
	}
	m.lastCheck = time.Now()
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.Check(ctx)
	}()
}

// Check looks up the latest releases without installing them.
func (m *Manager) Check(ctx context.Context) error {
	st := m.Status()
	if !st.Ready || !st.Update.Supported || st.Update.State == "waiting" || st.Update.State == "installing" {
		return nil
	}
	channel := m.channel()
	m.update(func(s *Status) { s.Update.State = "checking" })
	var ytLatest, gdlLatest, hlLatest string
	var ytErr, gdlErr, hlErr error
	if m.managed(st.Paths.YtDlp) {
		ytLatest, ytErr = latestRelease(ctx, releaseRepos[channel])
	}
	if m.managed(st.Paths.GalleryDL) {
		gdlLatest, gdlErr = latestRelease(ctx, galleryRepo)
	}
	if m.managedHeadless() != "" {
		hlLatest, _, hlErr = latestHeadless(ctx)
	}
	err := errors.Join(ytErr, gdlErr, hlErr)
	m.update(func(s *Status) {
		if s.Update.State == "checking" { // an install may have started meanwhile
			s.Update.State = ""
		}
		s.Update.Channel, s.Update.CheckedAt, s.Update.CheckError = channel, time.Now(), ""
		if err != nil {
			s.Update.CheckError = err.Error()
		}
		if ytErr == nil {
			s.Update.Latest = ytLatest
		}
		if gdlErr == nil {
			s.Update.GalleryLatest = gdlLatest
		}
		if hlErr == nil {
			s.Update.HeadlessLatest = hlLatest
		}
		s.Update.setPending(s)
	})
	if err != nil {
		slog.Warn("update check failed", "err", err)
	} else {
		slog.Info("update check", "channel", channel, "yt-dlp", ytLatest, "gallery-dl", gdlLatest, "headless", hlLatest)
	}
	return err
}

// setPending lists the tools whose latest release differs from the installed one.
// Not "newer than": switching yt-dlp channel should offer the other channel's latest.
func (u *UpdateStatus) setPending(s *Status) {
	u.Pending = nil
	if u.Latest != "" && u.Latest != s.YtDlpVersion {
		u.Pending = append(u.Pending, PendingUpdate{Tool: "yt-dlp", Current: s.YtDlpVersion, Latest: u.Latest})
	}
	if build := galleryBuild(s.GalleryDLVer); u.GalleryLatest != "" && build != "" && u.GalleryLatest != build {
		u.Pending = append(u.Pending, PendingUpdate{Tool: "gallery-dl", Current: build, Latest: u.GalleryLatest})
	}
	if u.HeadlessLatest != "" && s.HeadlessVer != "" && u.HeadlessLatest != s.HeadlessVer {
		u.Pending = append(u.Pending, PendingUpdate{Tool: "headless Chrome", Current: s.HeadlessVer, Latest: u.HeadlessLatest})
	}
	u.Available = len(u.Pending) > 0
}

// galleryBuild returns the build tag from gallery-dl's "1.32.19:2026.11.01"
// version, which is what gdl-org/builds releases are tagged with.
func galleryBuild(version string) string {
	if _, build, ok := strings.Cut(version, ":"); ok {
		return build
	}
	return ""
}

// Install updates every managed tool to its latest release ("refit"). It
// waits for running yt-dlp/gallery-dl/browser processes to exit and holds new ones until done.
func (m *Manager) Install(ctx context.Context) error {
	m.mu.Lock()
	st := m.st
	switch {
	case !st.Ready:
		m.mu.Unlock()
		return errors.New("still setting up")
	case !st.Update.Supported:
		m.mu.Unlock()
		return errors.New("these tools are system installs; update them the way they were installed")
	case st.Update.State == "waiting" || st.Update.State == "installing":
		m.mu.Unlock()
		return errors.New("an update is already in progress")
	}
	m.st.Update.State, m.st.Update.Error, m.st.Update.Installed = "waiting", "", ""
	snap := m.st
	m.mu.Unlock()
	m.hub.Publish(events.ToolsStatus, snap)

	m.toolLock.Lock()
	defer m.toolLock.Unlock()
	for _, f := range m.refitting {
		f()
	}
	m.update(func(s *Status) { s.Update.State = "installing" })

	uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	channel := m.channel()
	var errs []error
	ytVersion, gdlVersion := st.YtDlpVersion, st.GalleryDLVer
	if yt := st.Paths.YtDlp; m.managed(yt) {
		if out, err := Command(uctx, yt, "--update-to", channel+"@latest").CombinedOutput(); err != nil {
			slog.Warn("yt-dlp update failed", "err", err, "output", string(out))
			errs = append(errs, fmt.Errorf("yt-dlp: %s", lastLine(string(out), err.Error())))
		}
		ytVersion = m.version(ctx, yt)
	}
	if gdl := st.Paths.GalleryDL; m.managed(gdl) {
		if err := m.installGalleryDL(uctx, gdl, galleryBuild(gdlVersion)); err != nil {
			slog.Warn("gallery-dl update failed", "err", err)
			errs = append(errs, fmt.Errorf("gallery-dl: %w", err))
		}
		gdlVersion = m.version(ctx, gdl)
	}
	hlVersion := st.HeadlessVer
	if m.managedHeadless() != "" {
		m.browserMu.Lock()
		if err := m.installHeadless(uctx, hlVersion); err != nil {
			slog.Warn("headless Chrome update failed", "err", err)
			errs = append(errs, fmt.Errorf("headless Chrome: %w", err))
		}
		m.browserMu.Unlock()
		hlVersion = m.headlessVersion()
	}
	err := errors.Join(errs...)
	m.update(func(s *Status) {
		s.Stage = "" // downloadFile reports progress there
		s.Update.State, s.Update.Channel = "", channel
		s.YtDlpVersion, s.GalleryDLVer, s.HeadlessVer = ytVersion, gdlVersion, hlVersion
		if m.managed(st.Paths.YtDlp) {
			s.Update.Latest = ytVersion
		}
		if build := galleryBuild(gdlVersion); build != "" && m.managed(st.Paths.GalleryDL) {
			s.Update.GalleryLatest = build
		}
		if hlVersion != "" {
			s.Update.HeadlessLatest = hlVersion
		}
		s.Update.setPending(s)
		s.Update.CheckedAt, s.Update.CheckError = time.Now(), ""
		if err != nil {
			s.Update.Error = err.Error()
			return
		}
		s.Update.Installed = installedSummary(s)
	})
	if err != nil {
		return err
	}
	slog.Info("tools updated", "yt-dlp", ytVersion, "gallery-dl", gdlVersion, "channel", channel)
	return nil
}

// installGalleryDL replaces the managed gallery-dl with the latest build, if newer.
func (m *Manager) installGalleryDL(ctx context.Context, dest, current string) error {
	asset, err := galleryDLAsset()
	if err != nil {
		return err
	}
	tag, err := latestRelease(ctx, galleryRepo)
	if err != nil || tag == current {
		return err
	}
	// Download beside the old copy and swap, so a failed download leaves it working.
	tmp := filepath.Join(filepath.Dir(dest), "gallery-dl.new"+filepath.Ext(dest))
	defer os.Remove(tmp)
	if err := m.downloadFile(ctx, galleryDLURL(tag, asset), tmp, "gallery-dl"); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// installedSummary is e.g. "yt-dlp 2026.10.26, gallery-dl 1.32.19".
func installedSummary(s *Status) string {
	var parts []string
	if s.YtDlpVersion != "" {
		parts = append(parts, "yt-dlp "+s.YtDlpVersion)
	}
	if s.GalleryDLVer != "" {
		release, _, _ := strings.Cut(s.GalleryDLVer, ":")
		parts = append(parts, "gallery-dl "+release)
	}
	if s.HeadlessVer != "" {
		parts = append(parts, "headless Chrome "+s.HeadlessVer)
	}
	return strings.Join(parts, ", ")
}

// latestRelease reads the latest tag from GitHub's /releases/latest redirect,
// which (unlike the REST API) isn't rate-limited for anonymous clients.
func latestRelease(ctx context.Context, repo string) (string, error) {
	if repo == "" {
		return "", errors.New("unknown update channel")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("unexpected response from GitHub: %s", resp.Status)
	}
	return loc[i+len("/tag/"):], nil
}

func lastLine(out, fallback string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return strings.TrimPrefix(l, "ERROR: ")
		}
	}
	return fallback
}
