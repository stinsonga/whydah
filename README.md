# Whydah

Salvage video, audio and photos from the web: YouTube, Vimeo, X, TikTok,
SoundCloud and the other sites [yt-dlp](https://github.com/yt-dlp/yt-dlp)
supports, photo galleries from Instagram, Reddit, Imgur, Flickr and the
other sites [gallery-dl](https://github.com/mikf/gallery-dl) supports, and the
images on any other web page. It's a single executable with no runtime to
install: run it and a UI opens in your browser.

Named after the *Whydah*, Samuel Bellamy's flagship, so a few things have
nautical names: **spyglass** looks at a link, the **crow's nest** looks
harder (in a hidden browser), **salvage** downloads, **refit** updates the
tools, and **the hold** is your download folder.

## Using it

```sh
whydah                               # open the UI (watches your clipboard for links)
whydah salvage URL [URL...]          # download everything at each link, then exit
whydah URL [URL...]                  # same as salvage
whydah spyglass URL [URL...]         # show what each link has, without downloading
whydah refit                         # update yt-dlp and gallery-dl to their latest releases

whydah salvage --audio URL           # audio only (format from settings, default mp3)
whydah salvage --output ~/Movies URL # download folder for this run
whydah salvage --browser firefox URL # use that browser's cookies (private / members-only)
whydah spyglass --crows-nest URL     # find photos by loading the page in a hidden browser
whydah spyglass --linked URL         # check the videos a page's thumbnails link to
whydah salvage --linked URL          # ...and download them
```

Launching it again while it's running just reopens the existing UI.

### From your browser

Settings → *Browser button* has a **⚓ Salvage with Whydah** link: drag it
to your bookmarks bar, and clicking it on any page opens that page in
Whydah (in its own tab, reused each time). It carries the UI's key after
the `#` in Whydah's address, a part browsers never send to any site, so
other websites can't use it. It points at the port Whydah is using, so if
that ever changes (another app took the usual one), drag a fresh copy.

### File names

Settings → *File names* is a template for single videos, audio and
streams, e.g. `{uploader}/{date} {title}`: `{title}`, `{uploader}` (the
site's name when there's none, as for streams), `{date}` (upload date,
2026-10-25), `{site}` and `{id}`, and `/` makes folders inside the download
folder. It needs `{title}` or `{id}` so files don't overwrite each other.
Playlists keep their album folders with numbered tracks, and galleries their
own folders. The default is `{title}`.

### History

**History** lists everything salvaged, newest first, after the hold is
cleared and across restarts. Search matches every word you type against
titles, sites, links, playlist names and file names. **Show** finds the
file (when it's still there), **Salvage again** looks at the link afresh (a
stream's page, not the stream itself, whose link has likely expired), and
✕ removes an entry (never the file). It's kept in `history.jsonl`, up to
the latest 10,000; terminal salvages aren't recorded.

### What a link turns into

Each link is tried in order, and the first that finds something wins:

1. **yt-dlp**: a video, or a playlist / album / channel.
2. **gallery-dl**: a photo gallery or post on a site it knows.
3. **Page scan**: the images on the page itself: `<img>` tags (largest
   `srcset` size), `<picture>` sources, `<noscript>` fallbacks, links
   straight to image files (usually the full size of a thumbnail), and image
   links in the data a page's scripts start from (such as Next.js's
   `__NEXT_DATA__`). Icons and images under 160 px are dropped, and sizes of
   one picture are merged into the largest. A direct link to an image is
   saved as is.
4. **Crow's nest**: if the page scan finds fewer than three photos or signs
   of a video or audio player, or the site turns plain requests away
   (401/403/429/503, as bot walls do), the page is loaded in a hidden browser. It runs the page's scripts, scrolls to
   the bottom so lazy-loaded and endless-feed images appear, and collects
   what's shown at the size it was shown, plus images the browser downloaded
   that the page no longer shows (old carousel slides). It also catches
   the streams the page's player plays (see below). It takes 5–30
   seconds. The browser stays running for two minutes after a look, so the
   next one starts sooner; each look gets a fresh private window, so no
   cookies or storage carry from one page to the next. The **Crow's nest** button on any photo result (or
   `--crows-nest`) goes straight there.

If yt-dlp recognises the site but can't fetch the video (it needs a sign-in,
for example), you see that error rather than a page scan, since the scan
would only find the video's preview image. **Photos on this page…** on a
video or error card scans the page anyway.

With **Settings → cookies from browser** on, the crow's nest is signed in as
you: yt-dlp reads that browser's cookies (Firefox included), and the ones for
the site being looked at are loaded into the hidden browser and sent with
the photo downloads. Nothing else is kept; the exported file is deleted
straight away.

### Streams from a site's own player

Sites with a player of their own, which yt-dlp has no extractor for, are
salvaged by catching what the player loads. While the crow's nest has the
page open, it notes the HLS (`.m3u8`) and DASH (`.mpd`) manifests and the
video and audio files the page plays, including inside embedded players
from other sites. Many players load nothing until they're started, so it
plays every `<video>` and `<audio>` with the sound off and presses a large
play button (never one inside a link, which would open another page).

Each stream is checked with yt-dlp, which drops ones it can't get (DRM,
expired links) and folds an HLS stream's quality variants into the one
master. Ads (from known ad servers), segments and clips under 5 seconds are
left out. A page with a stream and few photos shows it as the result, with
duration, resolution and **Salvage video** / **Salvage audio**; one with a
gallery as well gets a **▶ N streams…** button. The download sends the
Referer the player sent, since stream servers often refuse requests
without it, and is named after the page.

### Videos behind thumbnails

Listing pages (a site's videos page, a talks index, search results) show
thumbnails that each lead to a page with the video. While scanning a page,
Whydah notes the thumbnail links that look like that, without fetching
anything: a video-like address (a path segment like `/watch`, `/video/`,
`/talks/` or `/embed/`, or one joined to an ID like `/video-48213`), a video
on a video site (not a channel or profile), a duration badge on the
thumbnail ("12:34"), or a play button (a class or label with the word
"play" or "video", not just the letters, so `display` doesn't count); plus
the rest of a listing, when three or more links of the same shape qualify.
Logins, tags, categories and the page itself are skipped, and in the page's
header, nav and footer only an address or a duration counts, so logos and
social icons stay out.

They show up as **N linked videos…** on the result (or as their own card on
a page with no photos). Clicking it runs yt-dlp on each linked page, a few
at a time, with progress; the ones with a video come back with their real
titles and durations, to pick from like a playlist and salvage as video or
audio into a folder named after the page. Script-built listings (TED's,
for one) only have their thumbnails after the **Crow's nest**.

### Playlists and albums

Playlist, album, set and channel links show the item list so you can pick
which to download, with each item's thumbnail when the site has one (click
it to enlarge; the arrow keys step through); from the terminal every item is downloaded. Items are
saved to a folder named after the playlist as `01 - Title.ext`, and audio is
tagged with the playlist as album, the position as track number, and cover
art. A YouTube link opened from a playlist (`watch?v=…&list=…`) downloads
just that video, with a **Whole playlist…** button for the rest.

### Photos

Galleries show a grid to pick from (all are selected to start); the ⤢ on a
tile opens it full size, and the arrow keys step through the rest. They're
saved to a folder named after the gallery, as one entry in the hold, with
gallery-dl's file names or, for a page scan, `01 - name.ext`. A single photo
goes straight into the download folder. Retrying a partly failed gallery
only fetches what's missing. Listing stops at 1000 photos; **Salvage all**
still gets every one.

### When something goes wrong

Failures are explained in plain words with what to do about it: a video
that's private, blocked in your country, age-restricted, members-only or
removed; a site that wants you signed in, refused the download or is
rate-limiting you; no connection, a full disk, a stream that hasn't started.
Where being signed in would help, it says how (turn on cookies, or sign in
again in that browser), and **Refit** is offered only for failures a newer
yt-dlp might fix. What the tool actually said is under **Details**.

### Languages

Settings → *Language* follows your browser (in the UI) or system (in the
terminal) by default. Only English exists so far; the messages live in
`internal/i18n/locales`, one JSON file per language. To add one, copy
`en.json` to e.g. `de.json`, translate the values (keep the `{placeholders}`;
give each `.one`/`.other` key the plural forms the language needs, such as
`.few`), set `language.name` to the language's own name, and rebuild. Keys
left out fall back to English, and `go test ./internal/i18n` checks the
file against the code. So far the failure messages and a few labels use
the catalog; the rest of the UI is still English text in `index.html`,
moving over a screen at a time with `t("key")` and `data-t="key"`.

### Across restarts

The hold is kept in `queue.json` next to the settings. Downloads that were
queued or running when Whydah quit (or crashed, or the Mac restarted) start
again on the next launch: yt-dlp carries on from its `.part` file and
galleries skip the photos already saved. Finished ones stay listed, with
**Retry** on the failed ones, until you clear them. Terminal salvages
aren't kept.

## Helpers

On first run it downloads its helpers (about 175 MB, shown in the UI):

| Tool | Why | Source |
|------|-----|--------|
| yt-dlp | video and audio | always a managed copy, so the app can update it |
| gallery-dl | photo galleries | managed copy from [gdl-org/builds](https://github.com/gdl-org/builds); a system copy on Intel Macs and ARM Linux, which have no build |
| ffmpeg + ffprobe | merges video+audio, converts audio | system copy if on PATH, otherwise downloaded |
| deno | solves YouTube's JS challenges | system copy if on PATH, otherwise downloaded |
| a Chromium browser | the crow's nest | an installed Chrome, Edge, Chromium, Brave or Vivaldi, with its own throwaway profile; otherwise Google's [chrome-headless-shell](https://developer.chrome.com/blog/chrome-headless-shell), downloaded the first time it's needed (about 100 MB, 210 MB unpacked) |

If a helper can't be obtained the app still works in a reduced mode and shows
a warning. Without gallery-dl, photos come from the page scan only. The
crow's nest runs by itself only when a browser is already there; a
chrome-headless-shell download waits until you press **Crow's nest**.

### Keeping the tools current (refit)

Sites change often, and a new yt-dlp or gallery-dl release is usually what
fixes a download that stopped working. A downloaded chrome-headless-shell is
kept current too; an installed browser updates itself. The app checks for new releases at
startup, every 6 hours, and right after a download fails. It never installs
one on its own: the UI shows a **Refit now** banner (and a hint on failed
downloads), and the refit waits for running downloads to finish first. After
a refit it offers to retry downloads that failed. From the terminal:
`whydah refit`.

Settings → *yt-dlp updates* picks yt-dlp's release channel: stable (default)
or nightly, which gets site fixes sooner but is occasionally buggy. gallery-dl
always uses the latest build.

### Where things live

| | macOS | Windows | Linux |
|-|-------|---------|-------|
| Settings, `queue.json`, `history.jsonl`, `app.log` | `~/Library/Application Support/whydah` | `%AppData%\whydah` | `~/.config/whydah` |
| Helper binaries | `~/Library/Caches/whydah` | `%LocalAppData%\whydah` | `~/.cache/whydah` |
| Downloads (default) | `~/Downloads/Whydah` | same | same |

Deleting the cache folder is safe; helpers are re-downloaded on next launch.
Folders from the project's old name (`videoextract`) are moved over on first
run.

## Building

Requires Go 1.26+. No cgo, so every platform cross-compiles from any machine.

```sh
make build   # ./bin/whydah for this machine
make app     # bin/Whydah.app, a universal macOS app (see below)
make dist    # dist/whydah-{darwin,windows,linux}-{arch}
make vet
go test ./...
```

### The macOS app

`make app` builds `bin/Whydah.app`; drag it to Applications if you like and
open it from Finder, the Dock or Spotlight. It has no Dock icon or window of
its own: opening it starts Whydah in the background and opens the UI, and
opening it again while it runs reopens the UI. Quit from the UI.

Opened this way, Whydah gets its own macOS privacy permissions instead of
borrowing the terminal's, so Full Disk Access (needed for browser cookies) can
go to Whydah alone. macOS ties that permission to the app's signature; to keep
it across rebuilds, create a local signing certificate once:

```sh
./scripts/make-signing-cert.sh   # adds "Whydah Local Signing" to your login keychain
make app
```

Without the certificate the app is signed ad-hoc, and the permission has to be
switched off and on again after each build. The certificate is only trusted
on your Mac; copies for other people need an Apple Developer ID.

### The icon

`packaging/icon/main.go` draws the icon (an anchor with a download arrow on
its shank, brass on navy) and writes it in every format: `make icon`. Its
output is checked in, so builds don't need it: the macOS `.icns` (used by
`make app`), the UI's favicon, a 512 px PNG for Linux launchers, and the
Windows `.ico`. `make winres` then embeds the `.ico` in Windows builds
(`rsrc_windows_*.syso`, picked up by the Go linker), using
[go-winres](https://github.com/tc-hib/go-winres).

## Layout

```
main.go                 subcommands and flags, wiring, single-instance check, logging
detach_*.go             starting the server in the background when opened as Whydah.app
packaging/macos         Info.plist and Whydah.icns for Whydah.app
packaging/icon          draws the icon in every format (make icon)
scripts                 make-signing-cert.sh
internal/config         settings.json load/save, URL host matching, browser detection
internal/events         pub/sub hub between engine, clipboard watcher and frontends
internal/tools          finds/downloads/updates yt-dlp, gallery-dl, ffmpeg, deno, headless Chrome
internal/engine         inspection and downloads: yt-dlp, gallery-dl, page scan, crow's nest,
                        linked videos (links.go), stream sniffing (streams.go)
                        (render.go, the shared browser in browser.go, a small DevTools
                        client in cdp.go); jobs, progress, cancel, explained failures (explain.go),
                        history (history.go),
                        the saved queue (queue.go)
internal/clipboard      clipboard polling (Win32 API on Windows, pbpaste/wl-paste/xclip elsewhere)
internal/web            localhost HTTP + SSE API and the embedded single-file UI
internal/cli            terminal mode: salvage, spyglass, refit
internal/i18n           messages in each language (locales/*.json), explained failures
```

The UI server listens on 127.0.0.1 only. Every API call needs a per-install
token (in `ui-token` next to the settings), and the Host header is checked, so
other websites can't drive it from your browser.

## Known limitations

- Release binaries aren't code-signed yet. macOS Gatekeeper will block a
  downloaded copy until you right-click → Open (or run
  `xattr -d com.apple.quarantine whydah`). Windows SmartScreen shows a
  similar warning.
- Linked videos are only as good as yt-dlp on the linked page: a site's own
  player that yt-dlp doesn't know comes back as "not a video" (streams are
  only caught on the page you look at, not on every linked page).
- DRM-protected streams (most paid streaming services) can't be salvaged.
  Players that need several clicks, or a sign-in the crow's nest doesn't
  have, won't play, so nothing is caught. Stream links that work only once
  or expire within seconds fail the check.
- The crow's nest only reads photos from the page itself, not from its
  iframes (it does follow iframes for streams), and stops scrolling an
  endless feed after about 12 seconds. Pages often include some clutter
  (avatars, banners) that the picker lets you untick.
- On macOS, using browser cookies needs Full Disk Access (System Settings →
  Privacy & Security). Give it to Whydah.app, or, when running from a
  terminal, to that terminal app, then reopen it. Without it macOS hides the
  browser's profile, and Whydah says so.
