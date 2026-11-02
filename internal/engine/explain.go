package engine

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"whydah/internal/config"
	"whydah/internal/i18n"
)

// Failures are explained for the user where they leave the engine (a look
// that found nothing, a download that failed): what went wrong in plain
// words, in their language, with what to do about it. What yt-dlp or
// gallery-dl actually said stays with it as the detail.

// explanations sort what yt-dlp and gallery-dl say when they fail, most
// specific first. Their messages are English and fairly stable; one that
// matches nothing here is shown as it is.
var explanations = []struct {
	re   *regexp.Regexp
	key  string
	hint string // "cookies": how cookies could help, given the settings
}{
	{regexp.MustCompile(`(?i)\bdrm\b`), "error.drm", ""},
	{regexp.MustCompile(`(?i)no space left on device`), "error.disk_full", ""},
	{regexp.MustCompile(`(?i)(ffmpeg|ffprobe)\b.*\bnot (found|installed)`), "error.needs_ffmpeg", ""},
	{regexp.MustCompile(`(?i)requested format (is )?not available`), "error.format_unavailable", ""},
	{regexp.MustCompile(`(?i)not (made this video )?available (in|from) your (country|location)|geo.?restrict|blocked (it )?in your country`), "error.geo_blocked", ""},
	{regexp.MustCompile(`(?i)confirm you.?re not a bot`), "error.bot_check", "cookies"},
	{regexp.MustCompile(`(?i)members.only|join this channel|available to this channel.s members`), "error.members_only", "cookies"},
	{regexp.MustCompile(`(?i)private video|video is private|account is private|this content is private`), "error.private", "cookies"},
	{regexp.MustCompile(`(?i)age.restricted|confirm your age|inappropriate for some users`), "error.age_restricted", "cookies"},
	{regexp.MustCompile(`(?i)login required|log ?in to|sign ?in to|requires? (authentication|login)|authentication required|use --cookies|account cookies|only available (for|to) registered users|\b401\b|unauthorized`), "error.sign_in", "cookies"},
	{regexp.MustCompile(`(?i)\b429\b|too many requests|rate.?limit`), "error.rate_limited", ""},
	{regexp.MustCompile(`(?i)live event will begin|premieres in|scheduled to start|this live (stream|event) has not started|is upcoming`), "error.not_started", ""},
	{regexp.MustCompile(`(?i)video (is )?unavailable|this (video|content) is (not|no longer) available|has been removed|no longer available|been terminated|copyright (claim|grounds)|video (has been|was) deleted|content isn.t available`), "error.unavailable", ""},
	{regexp.MustCompile(`(?i)\b404\b|not found`), "error.not_found", ""},
	{regexp.MustCompile(`(?i)\b403\b|forbidden`), "error.forbidden", "forbidden"},
	{regexp.MustCompile(`(?i)timed? ?out`), "error.timeout", ""},
	{regexp.MustCompile(`(?i)no such host|dial tcp|getaddrinfo|name or service not known|nodename nor servname|failed to resolve|temporary failure in name resolution|connection (refused|reset)|network is unreachable|no route to host|certificate verify failed|ssl: `), "error.network", ""},
}

// explainErr explains err, which came from a look at or a download of
// pageURL. A cancellation passes through, since that isn't a failure.
func explainErr(err error, pageURL string, s config.Settings) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if e, ok := i18n.As(err); ok {
		return e
	}
	site := siteName(pageURL)
	var status statusError
	switch {
	case errors.Is(err, errNoPhotos):
		return i18n.New("error.nothing_found")
	case errors.Is(err, context.DeadlineExceeded):
		return i18n.New("error.timeout", "site", site)
	case errors.As(err, &status):
		switch status.code {
		case 401:
			return withHint(i18n.New("error.sign_in", "site", site), "cookies", site, s).WithDetail(err.Error())
		case 403:
			return withHint(i18n.New("error.forbidden", "site", site), "forbidden", site, s).WithDetail(err.Error())
		case 404, 410:
			return i18n.New("error.not_found")
		case 429:
			return i18n.New("error.rate_limited", "site", site)
		}
		return i18n.New("error.page_status", "status", status.status)
	}
	return explain(err.Error(), site, s)
}

// explain sorts a tool's error message into one the user can act on.
func explain(raw, site string, s config.Settings) *i18n.Error {
	raw = strings.TrimSpace(raw)
	for _, x := range explanations {
		if x.re.MatchString(raw) {
			return withHint(i18n.New(x.key, "site", site), x.hint, site, s).WithDetail(raw)
		}
	}
	return i18n.New("error.other", "detail", raw).WithDetail(raw)
}

// withHint adds what the user could do: "cookies" when being signed in
// would help, "forbidden" when a refit or being signed in might.
func withHint(e *i18n.Error, hint, site string, s config.Settings) *i18n.Error {
	if hint == "" {
		return e
	}
	if s.BrowserCookies == "" {
		return e.WithHint(map[string]string{"cookies": "hint.cookies_off", "forbidden": "hint.forbidden"}[hint])
	}
	e.Args["browser"] = config.BrowserName(s.BrowserCookies)
	return e.WithHint(map[string]string{"cookies": "hint.cookies_on", "forbidden": "hint.forbidden_cookies"}[hint])
}

// siteName is how a link's site is written in a message: its host, without "www.".
func siteName(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Hostname() == "" {
		return "the site"
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}
