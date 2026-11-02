package engine

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlaylistEntries(t *testing.T) {
	raw := []ytInfo{
		{URL: "https://video.example.com/watch?v=a", Title: "A", Duration: 65, Thumbnails: []struct{ URL string }{{"small"}, {"big"}}},
		{URL: "abc123"}, // no usable URL: skipped
		{URL: "https://audio.example.com/harbour/city-ports"},               // title derived from the link
		{URL: "https://api.audio.example.com/tracks/295"},                   // numeric id: placeholder
		{URL: "id-only", WebpageURL: "https://example.com/v/2", Title: "C"}, // falls back to webpage_url
	}
	got := playlistEntries(raw)
	want := []Entry{
		{Index: 1, URL: "https://video.example.com/watch?v=a", Title: "A", Duration: 65, DurationString: "1:05", Thumbnail: "big"},
		{Index: 2, URL: "https://audio.example.com/harbour/city-ports", Title: "city ports", DurationString: "Unknown"},
		{Index: 3, URL: "https://api.audio.example.com/tracks/295", Title: "Item 3", DurationString: "Unknown"},
		{Index: 4, URL: "https://example.com/v/2", Title: "C", DurationString: "Unknown"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestWatchPlaylistURL(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=x&list=PL123":  "https://www.youtube.com/playlist?list=PL123",
		"https://m.youtube.com/watch?v=x&list=RDx&ab=1": "https://www.youtube.com/playlist?list=RDx",
		"https://youtu.be/x?list=PL9":                   "https://www.youtube.com/playlist?list=PL9",
		"https://www.youtube.com/watch?v=x":             "",
		"https://notyoutube.com/watch?v=x&list=PL123":   "",
		"https://video.example.com/123?list=PL123":      "",
	}
	for in, want := range cases {
		if got := watchPlaylistURL(in); got != want {
			t.Errorf("watchPlaylistURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOutputTemplate(t *testing.T) {
	single := &Job{}
	if got, want := outputTemplate("/dl/100%", "", single), filepath.Join("/dl/100%%", "%(title)s.%(ext)s"); got != want {
		t.Errorf("single: got %q, want %q", got, want)
	}
	item := &Job{Group: "g1", GroupTitle: `AC/DC: Live? 50% "best"`, Index: 7, pad: 3}
	want := filepath.Join("/dl", `AC_DC_ Live_ 50%% _best_`, "007 - %(title)s.%(ext)s")
	if got := outputTemplate("/dl", "{uploader}/{title}", item); got != want { // a playlist keeps its album folder
		t.Errorf("playlist: got %q, want %q", got, want)
	}
}

func TestNameTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"{title}":               "%(title)s",
		"{uploader}/{title}":    filepath.FromSlash("%(uploader,channel,uploader_id,webpage_url_domain)s/%(title)s"),
		"{date} {title} [{id}]": "%(upload_date>%Y-%m-%d,release_date>%Y-%m-%d|)s %(title)s [%(id)s]",
		"100% {title}":          "100%% %(title)s",
		`{site}\{title}`:        filepath.FromSlash("%(webpage_url_domain)s/%(title)s"),
	} {
		if got := ytNameTemplate(in); got != want {
			t.Errorf("ytNameTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaylistMetadataArgs(t *testing.T) {
	if args := playlistMetadataArgs(&Job{}); args != nil {
		t.Errorf("single job should have no playlist tags, got %v", args)
	}
	got := playlistMetadataArgs(&Job{Group: "g1", GroupTitle: "Hits: 100%", Index: 3})
	want := []string{"--parse-metadata", `%(__literal|)sHits\: 100%%:%(album)s`, "--parse-metadata", "%(__literal|)s3:%(track_number)s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeDirName(t *testing.T) {
	cases := map[string]string{
		"Normal Album": "Normal Album",
		"  ..dots..  ": "dots",
		"a/b\\c:d*e?":  "a_b_c_d_e_",
		"":             "Playlist",
		"...":          "Playlist",
		"tab\there":    "tab_here",
	}
	for in, want := range cases {
		if got := sanitizeDirName(in); got != want {
			t.Errorf("sanitizeDirName(%q) = %q, want %q", in, got, want)
		}
	}
}
