package engine

import (
	"context"
	"fmt"
	"testing"

	"whydah/internal/config"
	"whydah/internal/i18n"
)

func TestExplain(t *testing.T) {
	off, on := config.Settings{}, config.Settings{BrowserCookies: "firefox"}
	for _, c := range []struct {
		raw, key, hint string
		s              config.Settings
	}{
		{"ERROR: [youtube] abc: Video unavailable. The uploader has not made this video available in your country", "error.geo_blocked", "", off},
		{"ERROR: [youtube] abc: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication.", "error.bot_check", "hint.cookies_off", off},
		{"ERROR: [youtube] abc: Private video. Sign in if you've been granted access to this video", "error.private", "hint.cookies_on", on},
		{"ERROR: [youtube] abc: Sign in to confirm your age. This video may be inappropriate for some users.", "error.age_restricted", "hint.cookies_off", off},
		{"ERROR: [youtube] abc: Join this channel to get access to members-only content like this video, and other exclusive perks.", "error.members_only", "hint.cookies_on", on},
		{"ERROR: [instagram] xyz: Requested content is not available, rate-limit reached or login required. Use --cookies", "error.sign_in", "hint.cookies_off", off},
		{"ERROR: [vimeo] 123: HTTP Error 429: Too Many Requests", "error.rate_limited", "", off},
		{"ERROR: [youtube] abc: This live event will begin in 3 hours.", "error.not_started", "", off},
		{"ERROR: [youtube] abc: Video unavailable. This video has been removed by the uploader", "error.unavailable", "", off},
		{"ERROR: [generic] Unable to download webpage: HTTP Error 404: Not Found", "error.not_found", "", off},
		{"ERROR: unable to download video data: HTTP Error 403: Forbidden", "error.forbidden", "hint.forbidden", off},
		{"ERROR: unable to download video data: HTTP Error 403: Forbidden", "error.forbidden", "hint.forbidden_cookies", on},
		{"ERROR: [generic] Unable to download webpage: <urlopen error [Errno 8] nodename nor servname provided, or not known>", "error.network", "", off},
		{"ERROR: [twitter] 123: Read timed out.", "error.timeout", "", off},
		{"ERROR: unable to write data: [Errno 28] No space left on device", "error.disk_full", "", off},
		{"ERROR: Postprocessing: ffprobe and ffmpeg not found. Please install or provide the path using --ffmpeg-location", "error.needs_ffmpeg", "", off},
		{"ERROR: [youtube] abc: Requested format is not available. Use --list-formats for a list of available formats", "error.format_unavailable", "", off},
		{"ERROR: [Foo] 1: This video is DRM protected", "error.drm", "", off},
		{"ERROR: [youtube] aaaaaaaaaaa: This video is unavailable", "error.unavailable", "", off},
		{`Get "http://whydah-test.invalid/": dial tcp: lookup whydah-test.invalid: no such host`, "error.network", "", off},
		{"ERROR: something nobody has seen before", "error.other", "", off},
	} {
		e := explain(c.raw, "example.com", c.s)
		if e.Key != c.key || e.Hint != c.hint {
			t.Errorf("%s\n  got %s + %q, want %s + %q", c.raw, e.Key, e.Hint, c.key, c.hint)
		}
		if e.Detail != c.raw {
			t.Errorf("%s: detail = %q", c.key, e.Detail)
		}
	}
	if e := explain("ERROR: Private video", "example.com", on); e.Args["browser"] != "Firefox" || e.In("en") != "This video is private. Make sure you're signed in to example.com in Firefox, then try again." {
		t.Errorf("with cookies on: %q", e.In("en"))
	}
}

func TestExplainErr(t *testing.T) {
	s := config.Settings{}
	page := "https://www.example.com/watch/1"
	for _, c := range []struct {
		err error
		key string
	}{
		{errNoPhotos, "error.nothing_found"},
		{fmt.Errorf("look: %w", errNoPhotos), "error.nothing_found"},
		{statusError{403, "403 Forbidden"}, "error.forbidden"},
		{statusError{404, "404 Not Found"}, "error.not_found"},
		{statusError{500, "500 Internal Server Error"}, "error.page_status"},
		{context.DeadlineExceeded, "error.timeout"},
		{i18n.New("error.drm"), "error.drm"},
		{fmt.Errorf("ERROR: [vimeo] 1: HTTP Error 429: Too Many Requests"), "error.rate_limited"},
	} {
		e, ok := i18n.As(explainErr(c.err, page, s))
		if !ok || e.Key != c.key {
			t.Errorf("%v: got %+v, want %s", c.err, e, c.key)
		}
		if ok && e.Args["site"] != "" && e.Args["site"] != "example.com" {
			t.Errorf("%v: site = %q", c.err, e.Args["site"])
		}
	}
	if err := explainErr(context.Canceled, page, s); err != context.Canceled {
		t.Errorf("a cancellation became %v", err)
	}
}
