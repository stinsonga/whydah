//go:build !windows

package tools

import "testing"

func TestOrphanMatching(t *testing.T) {
	helpers := []string{"/Users/x/Library/Caches/whydah/bin/", "/opt/homebrew/bin/gallery-dl"}
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{"/Users/x/Library/Caches/whydah/bin/yt-dlp --no-colors -- https://example.com", true},
		{"/opt/homebrew/bin/gallery-dl --resolve-json https://example.com", true},
		{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --headless --user-data-dir=/var/folders/ab/T/whydah-browser-123 --remote-debugging-port=0", true},
		{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", false}, // the user's own browser
		{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --user-data-dir=/Users/x/chrome-dev", false},
		{"/usr/local/bin/yt-dlp https://example.com", false},
	} {
		if got := ours(c.cmd, helpers); got != c.want {
			t.Errorf("ours(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
	m := profileDir.FindStringSubmatch("/x/Chrome --headless --user-data-dir=/var/folders/ab/T/whydah-browser-123 --remote-debugging-port=0")
	if m == nil || m[1] != "/var/folders/ab/T/whydah-browser-123" {
		t.Errorf("profile dir = %v", m)
	}
}
