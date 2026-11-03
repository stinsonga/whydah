package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"whydah/internal/i18n"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2026.11.03", "2026.11.02", true},
		{"2026.11.02.2", "2026.11.02", true},
		{"2026.11.02.10", "2026.11.02.9", true},
		{"2026.12.01", "2026.11.30", true},
		{"2026.11.02", "2026.11.02", false},
		{"2026.11.01", "2026.11.02", false},
		{"2026.11.02", "dev", false},
		{"dev", "2026.11.02", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// release is a fake GitHub release of tag with files, signed with key.
type release struct {
	tag   string
	files map[string][]byte
	key   ed25519.PrivateKey
	sums  []byte // what SHA256SUMS lists; computed from files if nil
}

func (rel release) serve(t *testing.T) *httptest.Server {
	sums := rel.sums
	if sums == nil {
		var b strings.Builder
		for name, data := range rel.files {
			h := sha256.Sum256(data)
			fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), name)
		}
		sums = []byte(b.String())
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(rel.key, sums))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		download := "/owner/whydah/releases/download/" + rel.tag + "/"
		switch {
		case r.URL.Path == "/owner/whydah/releases/latest":
			http.Redirect(w, r, "/owner/whydah/releases/tag/"+rel.tag, http.StatusFound)
		case r.URL.Path == download+"SHA256SUMS":
			w.Write(sums)
		case r.URL.Path == download+"SHA256SUMS.sig":
			fmt.Fprintln(w, sig)
		case strings.HasPrefix(r.URL.Path, download) && rel.files[strings.TrimPrefix(r.URL.Path, download)] != nil:
			w.Write(rel.files[strings.TrimPrefix(r.URL.Path, download)])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// trustKey makes a signing key and builds it in for the test.
func trustKey(t *testing.T) ed25519.PrivateKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := publicKey
	publicKey = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { publicKey = old })
	return priv
}

func updater(srv *httptest.Server, exe string) *Updater {
	return &Updater{base: srv.URL, repo: "owner/whydah", exe: exe, st: Status{Current: "2026.11.02", Supported: true}}
}

func binaryName() string {
	_, name := installTarget("/somewhere/whydah")
	return name
}

// installed writes an "installed copy" to replace, and returns its path.
func installed(t *testing.T) string {
	exe := filepath.Join(t.TempDir(), "whydah")
	if err := os.WriteFile(exe, []byte("old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestInstallBinary(t *testing.T) {
	key := trustKey(t)
	srv := release{tag: "2026.12.01", key: key, files: map[string][]byte{binaryName(): []byte("new build"), "other-file": []byte("x")}}.serve(t)
	exe := installed(t)
	u := updater(srv, exe)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if st := u.Status(); !st.Available || st.Latest != "2026.12.01" || !strings.HasSuffix(st.ReleaseURL, "/owner/whydah/releases/tag/2026.12.01") {
		t.Fatalf("after checking: %+v", st)
	}
	if err := u.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new build" {
		t.Errorf("installed copy is %q, want the new build", got)
	}
	if st := u.Status(); st.State != "restarting" || st.Current != "2026.12.01" || st.Available {
		t.Errorf("after installing: %+v", st)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".whydah-update-*")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

// An update has to be signed with the key built in, and be what it was
// signed as; otherwise nothing changes.
func TestInstallRefusesTampering(t *testing.T) {
	trustKey(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	good := []byte("new build")
	h := sha256.Sum256(good)
	for name, rel := range map[string]release{
		"error.selfupdate_signature": {key: other, files: map[string][]byte{binaryName(): good}},
		"error.selfupdate_checksum": {files: map[string][]byte{binaryName(): []byte("something else")},
			sums: []byte(hex.EncodeToString(h[:]) + "  " + binaryName() + "\n")},
		"error.selfupdate_missing": {files: map[string][]byte{"whydah-plan9-mips": good}},
	} {
		t.Run(name, func(t *testing.T) {
			rel.tag = "2026.12.01"
			if rel.key == nil {
				rel.key = trustKey(t)
			}
			exe := installed(t)
			u := updater(rel.serve(t), exe)
			ctx := context.Background()
			if err := u.Check(ctx); err != nil {
				t.Fatal(err)
			}
			err := u.Install(ctx)
			if e, ok := i18n.As(err); !ok || e.Key != name {
				t.Fatalf("Install() = %v, want %s", err, name)
			}
			if got, _ := os.ReadFile(exe); string(got) != "old build" {
				t.Errorf("installed copy is %q, want it untouched", got)
			}
			if st := u.Status(); st.Error == nil || st.State != "" || st.Current != "2026.11.02" {
				t.Errorf("after failing: %+v", st)
			}
		})
	}
}

// On macOS the whole Whydah.app is replaced, and only by a validly signed one.
func TestInstallBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("app bundles are macOS's")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("no codesign")
	}
	// A signed app: a copy of this test as its program, signed ad hoc.
	src := filepath.Join(t.TempDir(), "Whydah.app")
	program := filepath.Join(src, "Contents", "MacOS", "whydah")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, data, 0o755); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(filepath.Join("..", "..", "packaging", "macos", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Contents", "Info.plist"), plist, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("codesign", "--force", "--sign", "-", src).CombinedOutput(); err != nil {
		t.Skipf("can't sign a test app: %v %s", err, out)
	}
	if data, err = os.ReadFile(program); err != nil { // signing changed it
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(src), path)
		hdr, _ := zip.FileInfoHeader(info)
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil {
			_, err = w.Write(b)
		}
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		t.Fatal(err)
	}

	// The installed app, with a file only the old one has.
	apps := t.TempDir()
	exe := filepath.Join(apps, "Whydah.app", "Contents", "MacOS", "whydah")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(exe, []byte("old build"), 0o755)
	os.WriteFile(filepath.Join(apps, "Whydah.app", "Contents", "old-only"), nil, 0o644)

	key := trustKey(t)
	u := updater(release{tag: "2026.12.01", key: key, files: map[string][]byte{bundleAsset: zipped.Bytes()}}.serve(t), exe)
	ctx := context.Background()
	if err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := u.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, data) {
		t.Error("the app's program wasn't replaced")
	}
	if _, err := os.Stat(filepath.Join(apps, "Whydah.app", "Contents", "old-only")); err == nil {
		t.Error("the old app's files are still there")
	}
	if out, err := exec.Command("codesign", "--verify", "--strict", filepath.Join(apps, "Whydah.app")).CombinedOutput(); err != nil {
		t.Errorf("the installed app's signature: %v %s", err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(apps, ".whydah-update-*")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestUnzipStaysInside(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../escaped")
	w.Write([]byte("x"))
	zw.Close()
	archive := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(archive, buf.Bytes(), 0o644)
	if err := unzip(archive, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Error("unzipped a file outside the folder")
	}
}
