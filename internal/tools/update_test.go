package tools

import (
	"reflect"
	"testing"
)

func TestSetPending(t *testing.T) {
	s := &Status{YtDlpVersion: "2026.10.21", GalleryDLVer: "1.32.19:2026.10.30"}
	s.Update.Latest, s.Update.GalleryLatest = "2026.10.26", "2026.11.01"
	s.Update.setPending(s)
	want := []PendingUpdate{
		{Tool: "yt-dlp", Current: "2026.10.21", Latest: "2026.10.26"},
		{Tool: "gallery-dl", Current: "2026.10.30", Latest: "2026.11.01"},
	}
	if !s.Update.Available || !reflect.DeepEqual(s.Update.Pending, want) {
		t.Errorf("pending = %+v", s.Update.Pending)
	}

	s.YtDlpVersion, s.GalleryDLVer = "2026.10.26", "1.32.19:2026.11.01"
	s.Update.setPending(s)
	if s.Update.Available || len(s.Update.Pending) != 0 {
		t.Errorf("up to date, but pending = %+v", s.Update.Pending)
	}

	// A system gallery-dl has no build tag, so it's never offered.
	s.GalleryDLVer, s.Update.GalleryLatest = "1.30.0", "2026.11.01"
	s.Update.setPending(s)
	if s.Update.Available {
		t.Errorf("system gallery-dl offered an update: %+v", s.Update.Pending)
	}
}
