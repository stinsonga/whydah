package tools

import (
	"archive/zip"
	"context"
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

type archive struct {
	label string
	url   string
	kind  string   // "zip" or "tar.xz"
	files []string // base names to extract; directory structure is flattened
}

const (
	ytDlpRelease  = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	ffBuilds      = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/"
	macFFmpeg     = "https://ffmpeg.martin-riedl.de/redirect/latest/macos/%s/snapshot/%s.zip"
	denoRelease   = "https://github.com/denoland/deno/releases/latest/download/"
	unsupportedOS = "no prebuilt binary for %s/%s"
)

func ytDlpURL() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64", "darwin/amd64":
		return ytDlpRelease + "yt-dlp_macos", nil
	case "windows/amd64", "windows/arm64": // arm64 Windows runs the x64 build under emulation
		return ytDlpRelease + "yt-dlp.exe", nil
	case "linux/amd64":
		return ytDlpRelease + "yt-dlp_linux", nil
	case "linux/arm64":
		return ytDlpRelease + "yt-dlp_linux_aarch64", nil
	}
	return "", fmt.Errorf(unsupportedOS, runtime.GOOS, runtime.GOARCH)
}

// gallery-dl's own releases no longer ship executables; its project builds
// them in gdl-org/builds, tagged by date. Not every platform has one.
const galleryRepo = "gdl-org/builds"

func galleryDLAsset() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "gallery-dl_macos", nil
	case "windows/amd64", "windows/arm64":
		return "gallery-dl_windows.exe", nil
	case "linux/amd64":
		return "gallery-dl_linux", nil
	}
	return "", fmt.Errorf(unsupportedOS, runtime.GOOS, runtime.GOARCH)
}

func galleryDLURL(tag, asset string) string {
	return "https://github.com/" + galleryRepo + "/releases/download/" + tag + "/" + asset
}

func ffmpegArchives() ([]archive, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64", "darwin/amd64":
		return []archive{
			{label: "ffmpeg", url: fmt.Sprintf(macFFmpeg, runtime.GOARCH, "ffmpeg"), kind: "zip", files: []string{"ffmpeg"}},
			{label: "ffprobe", url: fmt.Sprintf(macFFmpeg, runtime.GOARCH, "ffprobe"), kind: "zip", files: []string{"ffprobe"}},
		}, nil
	case "windows/amd64", "windows/arm64":
		return []archive{{label: "ffmpeg", url: ffBuilds + "ffmpeg-master-latest-win64-gpl.zip", kind: "zip", files: []string{"ffmpeg.exe", "ffprobe.exe"}}}, nil
	case "linux/amd64":
		return []archive{{label: "ffmpeg", url: ffBuilds + "ffmpeg-master-latest-linux64-gpl.tar.xz", kind: "tar.xz", files: []string{"ffmpeg", "ffprobe"}}}, nil
	case "linux/arm64":
		return []archive{{label: "ffmpeg", url: ffBuilds + "ffmpeg-master-latest-linuxarm64-gpl.tar.xz", kind: "tar.xz", files: []string{"ffmpeg", "ffprobe"}}}, nil
	}
	return nil, fmt.Errorf(unsupportedOS, runtime.GOOS, runtime.GOARCH)
}

func denoArchive() (archive, error) {
	triple := map[string]string{
		"darwin/arm64":  "aarch64-apple-darwin",
		"darwin/amd64":  "x86_64-apple-darwin",
		"windows/amd64": "x86_64-pc-windows-msvc",
		"windows/arm64": "x86_64-pc-windows-msvc",
		"linux/amd64":   "x86_64-unknown-linux-gnu",
		"linux/arm64":   "aarch64-unknown-linux-gnu",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	if triple == "" {
		return archive{}, fmt.Errorf(unsupportedOS, runtime.GOOS, runtime.GOARCH)
	}
	return archive{label: "deno", url: denoRelease + "deno-" + triple + ".zip", kind: "zip", files: []string{exe("deno")}}, nil
}

// fetchArchive downloads a into a temp file and extracts the wanted files into destDir.
func (m *Manager) fetchArchive(ctx context.Context, a archive, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(m.dir, "fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	file := filepath.Join(tmp, "archive")
	if err := m.downloadFile(ctx, a.url, file, a.label); err != nil {
		return err
	}
	m.stage("Unpacking %s", a.label)
	switch a.kind {
	case "zip":
		err = extractZip(file, destDir, a.files)
	case "tar.xz":
		err = extractTarXz(ctx, file, tmp, destDir, a.files)
	default:
		err = fmt.Errorf("unknown archive kind %q", a.kind)
	}
	return err
}

func extractZip(file, destDir string, want []string) error {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || !wanted(name, want) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeExecutable(filepath.Join(destDir, name), rc)
		rc.Close()
		if err != nil {
			return err
		}
		found++
	}
	if found < len(want) {
		return fmt.Errorf("archive is missing some of %v", want)
	}
	return nil
}

// extractTarXz shells out to tar: Go's stdlib has no xz decoder, and every
// Linux system that would use this path ships tar with xz support.
func extractTarXz(ctx context.Context, file, workDir, destDir string, want []string) error {
	out := filepath.Join(workDir, "x")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if b, err := exec.CommandContext(ctx, "tar", "-xJf", file, "-C", out).CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, b)
	}
	found := 0
	err := filepath.WalkDir(out, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !wanted(d.Name(), want) {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		found++
		return writeExecutable(filepath.Join(destDir, d.Name()), f)
	})
	if err == nil && found < len(want) {
		err = fmt.Errorf("archive is missing some of %v", want)
	}
	return err
}

func wanted(name string, want []string) bool {
	for _, w := range want {
		if strings.EqualFold(name, w) {
			return true
		}
	}
	return false
}

func writeExecutable(dest string, r io.Reader) error {
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func (m *Manager) downloadFile(ctx context.Context, url, dest, label string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	pw := &progressWriter{m: m, label: label, total: resp.ContentLength}
	return writeExecutable(dest, io.TeeReader(resp.Body, pw))
}

// progressWriter reports download progress to the tools status, throttled.
type progressWriter struct {
	m     *Manager
	label string
	total int64
	n     int64
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		if p.total > 0 {
			p.m.stage("Downloading %s: %d of %d MB", p.label, p.n>>20, p.total>>20)
		} else {
			p.m.stage("Downloading %s: %d MB", p.label, p.n>>20)
		}
	}
	return len(b), nil
}
