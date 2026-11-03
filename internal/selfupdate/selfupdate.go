// Package selfupdate keeps Whydah itself up to date from its GitHub
// releases. A release has a build per platform (scripts/release.sh), a
// SHA256SUMS list of their checksums, and SHA256SUMS.sig, an ed25519
// signature of the list made with a key that never leaves the machine
// releases are made on. Whydah has the public half built in (key.go), so it
// only installs an update its owner signed, whoever else can publish to the
// repository.
package selfupdate

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"whydah/internal/events"
	"whydah/internal/i18n"
)

// repo is the GitHub repository releases come from, "owner/name". The
// Makefile sets it from the git remote; a build without it doesn't update.
// releasesBase is where GitHub is, for a build tried against a stand-in.
var (
	repo         = ""
	releasesBase = "https://github.com"
)

const (
	checkInterval = 24 * time.Hour
	firstCheck    = time.Minute // after starting, so a launch isn't slowed
	maxDownload   = 512 << 20   // more than any build could need
	bundleAsset   = "Whydah-macos.zip"
)

// Status is what the UI shows about Whydah's own updates.
type Status struct {
	Supported  bool        `json:"supported"` // a release build that knows where releases are
	Current    string      `json:"current"`
	Latest     string      `json:"latest"`
	Available  bool        `json:"available"`
	ReleaseURL string      `json:"release_url"` // the latest release's page, for what's new
	CheckedAt  time.Time   `json:"checked_at"`
	CheckError string      `json:"check_error"`
	State      string      `json:"state"` // "", "checking", "installing" or "restarting"
	Error      *i18n.Error `json:"error"` // the last install's failure
}

type Updater struct {
	hub  *events.Hub
	base string // https://github.com, or a test server
	repo string
	exe  string // what's running, if not os.Executable() (tests)

	mu sync.Mutex
	st Status
}

// New is an Updater for version (main.version) of Whydah.
func New(version string, hub *events.Hub) *Updater {
	_, err := parseVersion(version)
	return &Updater{hub: hub, base: releasesBase, repo: repo,
		st: Status{Current: version, Supported: repo != "" && publicKey != "" && err == nil}}
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st
}

func (u *Updater) update(f func(*Status)) {
	u.mu.Lock()
	f(&u.st)
	st := u.st
	u.mu.Unlock()
	if u.hub != nil {
		u.hub.Publish(events.SelfUpdate, st)
	}
}

// Watch checks for a new release shortly after starting, then daily.
func (u *Updater) Watch(ctx context.Context) {
	if !u.Status().Supported {
		return
	}
	wait := firstCheck
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		_ = u.Check(ctx)
		wait = checkInterval
	}
}

// Check looks up the latest release without installing it.
func (u *Updater) Check(ctx context.Context) error {
	st := u.Status()
	if !st.Supported || st.State != "" {
		return nil
	}
	u.update(func(s *Status) { s.State = "checking" })
	latest, err := u.latest(ctx)
	u.update(func(s *Status) {
		s.State, s.CheckedAt, s.CheckError = "", time.Now(), ""
		if err != nil {
			s.CheckError = err.Error()
			return
		}
		s.Latest, s.Available = latest, newer(latest, s.Current)
		s.ReleaseURL = u.base + "/" + u.repo + "/releases/tag/" + latest
	})
	if err != nil {
		slog.Warn("Whydah update check failed", "err", err)
	} else if u.Status().Available {
		slog.Info("Whydah update available", "current", u.Status().Current, "latest", latest)
	}
	return err
}

// latest reads the latest release's tag from GitHub's /releases/latest
// redirect, which (unlike the REST API) isn't rate-limited.
func (u *Updater) latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+"/"+u.repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if strings.HasSuffix(loc, "/releases") { // GitHub's answer when there's no release
		return "", i18n.New("error.selfupdate_none")
	}
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("unexpected response from GitHub: %s", resp.Status)
	}
	tag := loc[i+len("/tag/"):]
	if _, err := parseVersion(tag); err != nil {
		return "", fmt.Errorf("latest release %q: %w", tag, err)
	}
	return tag, nil
}

// Install downloads the latest release, checks it was signed with
// Whydah's key, and puts it in place of the running copy, which keeps
// running until it's restarted. Its error is explained (an *i18n.Error).
func (u *Updater) Install(ctx context.Context) error {
	st := u.Status()
	if !st.Supported || !st.Available || st.State == "installing" || st.State == "restarting" {
		return nil
	}
	u.update(func(s *Status) { s.State, s.Error = "installing", nil })
	err := u.install(ctx, st.Latest)
	u.update(func(s *Status) {
		s.State = ""
		if err == nil {
			s.State, s.Current, s.Available = "restarting", st.Latest, false
			return
		}
		s.Error, _ = i18n.As(err)
	})
	if err != nil {
		slog.Error("Whydah update failed", "err", err)
		return err
	}
	slog.Info("Whydah updated", "from", st.Current, "to", st.Latest)
	return nil
}

func (u *Updater) install(ctx context.Context, tag string) error {
	exe, err := u.exe, error(nil)
	if exe == "" {
		exe, err = os.Executable()
	}
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	target, asset := installTarget(exe)
	dir := filepath.Dir(target)
	work, err := os.MkdirTemp(dir, ".whydah-update-") // beside the target, so it moves into place without copying
	if err != nil {
		return i18n.New("error.selfupdate_write", "path", dir).WithDetail(err.Error())
	}
	defer os.RemoveAll(work)

	download := u.base + "/" + u.repo + "/releases/download/" + tag + "/"
	sums, err := fetch(ctx, download+"SHA256SUMS", 1<<20)
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	sig, err := fetch(ctx, download+"SHA256SUMS.sig", 1<<10)
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	if err := verify(sums, sig); err != nil {
		return i18n.New("error.selfupdate_signature").WithDetail(err.Error())
	}
	want, ok := checksum(sums, asset)
	if !ok {
		return i18n.New("error.selfupdate_missing").WithDetail(asset)
	}
	file := filepath.Join(work, asset)
	if err := save(ctx, download+asset, file, want); err != nil {
		return err
	}
	if asset == bundleAsset {
		return replaceBundle(file, target, work)
	}
	return replaceBinary(file, target)
}

// installTarget is what an update replaces: the app bundle the running
// binary is in, or the binary itself. asset is the release file it's
// replaced with.
func installTarget(exe string) (target, asset string) {
	if runtime.GOOS == "darwin" {
		if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
			return exe[:i+len(".app")], bundleAsset
		}
	}
	name := "whydah-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return exe, name
}

// verify checks sig (base64) is publicKey's signature of sums.
func verify(sums, sig []byte) error {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("no valid public key built in")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if !ed25519.Verify(key, sums, raw) {
		return errors.New("the signature doesn't match")
	}
	return nil
}

// checksum finds name's SHA-256 in a shasum-style list ("hex  name").
func checksum(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func get(ctx context.Context, link string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", link, resp.Status)
	}
	return resp, nil
}

func fetch(ctx context.Context, link string, limit int64) ([]byte, error) {
	resp, err := get(ctx, link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// save downloads link to file, checking it has the SHA-256 want.
func save(ctx context.Context, link, file, want string) error {
	resp, err := get(ctx, link)
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return i18n.New("error.selfupdate_checksum").WithDetail("expected " + want + ", got " + got)
	}
	return nil
}

// replaceBinary moves file over target. Windows won't let a running
// program's file be replaced, but it can be renamed, so the old one is
// moved aside first (Tidy deletes it on the next start).
func replaceBinary(file, target string) error {
	if err := os.Chmod(file, 0o755); err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	if runtime.GOOS == "windows" {
		old := target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return i18n.New("error.selfupdate_write", "path", filepath.Dir(target)).WithDetail(err.Error())
		}
		if err := os.Rename(file, target); err != nil {
			_ = os.Rename(old, target)
			return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
		}
		return nil
	}
	if err := os.Rename(file, target); err != nil {
		return i18n.New("error.selfupdate_write", "path", filepath.Dir(target)).WithDetail(err.Error())
	}
	return nil
}

// replaceBundle unpacks the zipped app into work and swaps it with the app
// at target. The running binary keeps working: its file stays open.
func replaceBundle(file, target, work string) error {
	unpacked := filepath.Join(work, "new")
	if err := unzip(file, unpacked); err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	app := filepath.Join(unpacked, "Whydah.app")
	if _, err := os.Stat(filepath.Join(app, "Contents", "MacOS", "whydah")); err != nil {
		return i18n.New("error.selfupdate_failed").WithDetail("the download has no Whydah.app")
	}
	if out, err := exec.Command("codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		return i18n.New("error.selfupdate_signature").WithDetail(strings.TrimSpace(string(out)))
	}
	old := filepath.Join(work, "old.app")
	if err := os.Rename(target, old); err != nil {
		return i18n.New("error.selfupdate_write", "path", filepath.Dir(target)).WithDetail(err.Error())
	}
	if err := os.Rename(app, target); err != nil {
		_ = os.Rename(old, target)
		return i18n.New("error.selfupdate_failed").WithDetail(err.Error())
	}
	return nil
}

// unzip extracts archive into dir, refusing paths that would leave it.
func unzip(archive, dir string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		path := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("%s: outside the archive", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = os.MkdirAll(path, 0o755)
		case mode&os.ModeSymlink != 0:
			err = errors.New(f.Name + ": the app has no symbolic links")
		default:
			err = unzipFile(f, path, mode.Perm()|0o600)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func unzipFile(f *zip.File, path string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(rc, maxDownload))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Tidy deletes what an update on Windows left behind (see replaceBinary).
func Tidy() {
	if exe, err := os.Executable(); err == nil && runtime.GOOS == "windows" {
		_ = os.Remove(exe + ".old")
	}
}

// parseVersion reads a release version, the date it was made on and, for
// a second release that day, a count: "2026.11.02" or "2026.11.02.2".
func parseVersion(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return nil, fmt.Errorf("%q isn't a release version", v)
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%q isn't a release version", v)
		}
		out[i] = n
	}
	return out, nil
}

// newer reports whether version a comes after b.
func newer(a, b string) bool {
	va, err := parseVersion(a)
	if err != nil {
		return false
	}
	vb, err := parseVersion(b)
	if err != nil {
		return false
	}
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return len(va) > len(vb)
}
