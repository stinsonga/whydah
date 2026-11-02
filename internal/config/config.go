// Package config loads and persists user settings as JSON in the OS config dir.
package config

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"whydah/internal/i18n"
)

const appName = "whydah"

// oldAppName is the project's previous name; its folders are moved on first run.
const oldAppName = "videoextract"

type Settings struct {
	OutputDir      string   `json:"output_dir"`
	AudioFormat    string   `json:"audio_format"`    // mp3, m4a, opus, flac, wav
	VideoQuality   string   `json:"video_quality"`   // best, 2160, 1440, 1080, 720, 480
	CompatibleMP4  bool     `json:"compatible_mp4"`  // prefer H.264/AAC in MP4 so QuickTime etc. can play it
	BrowserCookies string   `json:"browser_cookies"` // "", a yt-dlp browser name, or OwnBrowser
	WatchClipboard bool     `json:"watch_clipboard"`
	MaxConcurrent  int      `json:"max_concurrent"`
	SupportedHosts []string `json:"supported_hosts"`
	UpdateChannel  string   `json:"update_channel"` // yt-dlp release channel: stable or nightly
	Language       string   `json:"language"`       // "" to follow the browser (UI) or system (terminal), or a code from i18n.Languages
	NameTemplate   string   `json:"name_template"`  // file names for single videos and streams, e.g. "{uploader}/{title}"; see NameTokens
}

// NameTokens are what a file name template can use. "/" in a template
// makes folders inside the download folder.
var NameTokens = []string{"title", "uploader", "date", "site", "id"}

var nameToken = regexp.MustCompile(`\{([^{}]*)\}`)

// CheckNameTemplate reports what's wrong with a file name template, if
// anything: a token that doesn't exist, no {title} or {id} to tell files
// apart, or a path that leaves the download folder.
func CheckNameTemplate(t string) error {
	for _, m := range nameToken.FindAllStringSubmatch(t, -1) {
		if !contains(NameTokens, m[1]) {
			return i18n.New("error.name_token", "token", m[0], "tokens", "{"+strings.Join(NameTokens, "}, {")+"}")
		}
	}
	if !strings.Contains(t, "{title}") && !strings.Contains(t, "{id}") {
		return i18n.New("error.name_unique")
	}
	t = strings.ReplaceAll(t, "\\", "/")
	if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~") || filepath.IsAbs(t) || filepath.VolumeName(t) != "" {
		return i18n.New("error.name_path")
	}
	for _, seg := range strings.Split(t, "/") {
		if strings.TrimSpace(seg) == ".." {
			return i18n.New("error.name_path")
		}
	}
	return nil
}

var (
	AudioFormats   = []string{"mp3", "m4a", "opus", "flac", "wav"}
	VideoQualities = []string{"best", "2160", "1440", "1080", "720", "480"}
	UpdateChannels = []string{"stable", "nightly"}
)

func Defaults() Settings {
	out := "downloads"
	if home, err := os.UserHomeDir(); err == nil {
		out = filepath.Join(home, "Downloads", "Whydah")
	}
	return Settings{
		OutputDir:      out,
		AudioFormat:    "mp3",
		NameTemplate:   "{title}",
		VideoQuality:   "best",
		CompatibleMP4:  true,
		WatchClipboard: true,
		MaxConcurrent:  3,
		UpdateChannel:  "stable",
		SupportedHosts: []string{
			"youtube.com", "youtu.be", "vimeo.com", "twitter.com", "x.com",
			"tiktok.com", "instagram.com", "facebook.com", "soundcloud.com",
			"reddit.com", "imgur.com", "flickr.com", "tumblr.com", "bsky.app",
			"pinterest.com", "deviantart.com", "artstation.com",
		},
	}
}

// AppDir holds settings, the log file and the UI token.
func AppDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return appFolder(base)
}

// CacheDir holds downloaded helper binaries; safe to delete, they are re-fetched.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return appFolder(base)
}

// appFolder returns base/whydah, first moving base/videoextract there if
// only the old one exists, so settings and downloaded helpers carry over.
func appFolder(base string) (string, error) {
	dir := filepath.Join(base, appName)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(filepath.Join(base, oldAppName), dir) // fails harmlessly if there's nothing to move
	}
	return dir, os.MkdirAll(dir, 0o755)
}

type Store struct {
	mu   sync.RWMutex
	path string
	s    Settings
}

func Load() (*Store, error) {
	dir, err := AppDir()
	if err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, "settings.json"), s: Defaults()}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, st.save()
	case err != nil:
		return nil, err
	}
	// Unmarshal over defaults so fields added in later versions get sane values.
	if err := json.Unmarshal(data, &st.s); err != nil {
		return nil, err
	}
	st.s = normalize(st.s)
	return st, nil
}

func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s := st.s
	s.SupportedHosts = append([]string(nil), st.s.SupportedHosts...)
	return s
}

func (st *Store) Update(s Settings) (Settings, error) {
	s = normalize(s)
	st.mu.Lock()
	st.s = s
	st.mu.Unlock()
	return s, st.save()
}

// Override changes settings for this run only (e.g. CLI flags) without saving.
func (st *Store) Override(f func(*Settings)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f(&st.s)
	st.s = normalize(st.s)
}

func (st *Store) save() error {
	st.mu.RLock()
	data, err := json.MarshalIndent(st.s, "", "  ")
	st.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func normalize(s Settings) Settings {
	d := Defaults()
	s.OutputDir = strings.TrimSpace(s.OutputDir)
	if s.OutputDir == "" {
		s.OutputDir = d.OutputDir
	}
	if strings.HasPrefix(s.OutputDir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			s.OutputDir = filepath.Join(home, s.OutputDir[1:])
		}
	}
	if !contains(AudioFormats, s.AudioFormat) {
		s.AudioFormat = d.AudioFormat
	}
	if !contains(VideoQualities, s.VideoQuality) {
		s.VideoQuality = d.VideoQuality
	}
	if !contains(UpdateChannels, s.UpdateChannel) {
		s.UpdateChannel = d.UpdateChannel
	}
	if s.BrowserCookies != "" && s.BrowserCookies != OwnBrowser && !contains(SupportedBrowsers, s.BrowserCookies) {
		s.BrowserCookies = ""
	}
	if s.Language != "" && i18n.Match(s.Language) != s.Language {
		s.Language = ""
	}
	if s.NameTemplate = strings.TrimSpace(s.NameTemplate); s.NameTemplate == "" || CheckNameTemplate(s.NameTemplate) != nil {
		s.NameTemplate = d.NameTemplate
	}
	s.MaxConcurrent = min(max(s.MaxConcurrent, 1), 8)
	var hosts []string
	for _, h := range s.SupportedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		hosts = d.SupportedHosts
	}
	s.SupportedHosts = hosts
	return s
}

// IsSupportedURL reports whether text is an http(s) URL on one of the hosts
// (or a subdomain of one, so "m.example.com" matches but "notexample.com" doesn't).
func IsSupportedURL(text string, hosts []string) bool {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
