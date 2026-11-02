package engine

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"whydah/internal/tools"
)

// The hidden browser starts signed out. To see what you'd see, it borrows
// your browser's cookies for the site being scanned: yt-dlp already knows how
// to read them from every browser (Firefox included), and saves its cookie
// jar to a file on exit. Only that site's cookies are kept, in memory; the
// file is deleted straight away.

type cookie struct {
	Domain   string // without a leading dot
	Sub      bool   // also sent to subdomains
	Path     string
	Secure   bool
	HTTPOnly bool
	Expires  int64 // Unix seconds; 0 for a session cookie
	Name     string
	Value    string
}

type cookieJar []cookie

// siteCookies reads the browser's cookies for pageURL's site.
func siteCookies(ctx context.Context, ytDlp, browser, pageURL string) (cookieJar, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "whydah-cookies-") // private to this user
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "cookies.txt") // must not exist yet: yt-dlp rejects an empty file
	cmd := tools.Command(ctx, ytDlp, "--no-warnings", "--cookies-from-browser", browser, "--cookies", file)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run() // fails for want of a URL, after saving the jar
	f, err := os.Open(file)
	if err != nil {
		return nil, ytError(stderr.String(), firstErr(runErr, err))
	}
	defer f.Close()
	return parseCookies(f, site(u.Hostname())), nil
}

// parseCookies reads a Netscape cookies.txt, keeping cookies for site and its subdomains.
func parseCookies(r io.Reader, site string) cookieJar {
	var jar cookieJar
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		httpOnly := strings.HasPrefix(line, "#HttpOnly_")
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(f[0]), ".")
		if !domainMatch(domain, site) {
			continue
		}
		exp, _ := strconv.ParseInt(f[4], 10, 64)
		jar = append(jar, cookie{
			Domain: domain, Sub: f[1] == "TRUE", Path: firstNonEmpty(f[2], "/"), Secure: f[3] == "TRUE",
			HTTPOnly: httpOnly, Expires: exp, Name: f[5], Value: f[6],
		})
	}
	return jar
}

// fromCDP converts cookies read from a browser over CDP.
func fromCDP(list []cdpCookie) cookieJar {
	jar := make(cookieJar, 0, len(list))
	for _, c := range list {
		var exp int64
		if c.Expires > 0 {
			exp = int64(c.Expires)
		}
		jar = append(jar, cookie{
			Domain: strings.TrimPrefix(strings.ToLower(c.Domain), "."), Sub: strings.HasPrefix(c.Domain, "."),
			Path: firstNonEmpty(c.Path, "/"), Secure: c.Secure, HTTPOnly: c.HTTPOnly, Expires: exp, Name: c.Name, Value: c.Value,
		})
	}
	return jar
}

// forSite keeps the cookies for site and its subdomains.
func (jar cookieJar) forSite(site string) cookieJar {
	var out cookieJar
	for _, c := range jar {
		if domainMatch(c.Domain, site) {
			out = append(out, c)
		}
	}
	return out
}

// write saves jar as a Netscape cookies.txt, which yt-dlp and gallery-dl read.
func (jar cookieJar) write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	bw.WriteString("# Netscape HTTP Cookie File\n")
	flag := map[bool]string{true: "TRUE", false: "FALSE"}
	for _, c := range jar {
		if strings.ContainsAny(c.Name+c.Value+c.Domain+c.Path, "\t\r\n") {
			continue
		}
		domain := c.Domain
		if c.Sub {
			domain = "." + domain
		}
		if c.HTTPOnly {
			domain = "#HttpOnly_" + domain
		}
		bw.WriteString(strings.Join([]string{domain, flag[c.Sub], c.Path, flag[c.Secure], strconv.FormatInt(c.Expires, 10), c.Name, c.Value}, "\t") + "\n")
	}
	return bw.Flush()
}

// header is the Cookie header a browser would send to u, or "".
func (jar cookieJar) header(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	var parts []string
	for _, c := range jar {
		if (c.Secure && u.Scheme != "https") || !strings.HasPrefix(firstNonEmpty(u.Path, "/"), c.Path) {
			continue
		}
		if host == c.Domain || (c.Sub && domainMatch(host, c.Domain)) {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

func (jar cookieJar) apply(h http.Header, u *url.URL) {
	if v := jar.header(u); v != "" {
		h.Set("Cookie", v)
	}
}

// cdp converts the jar for Network.setCookies.
func (jar cookieJar) cdp() []map[string]any {
	var out []map[string]any
	for _, c := range jar {
		domain := c.Domain
		if c.Sub {
			domain = "." + domain
		}
		m := map[string]any{"name": c.Name, "value": c.Value, "domain": domain, "path": c.Path,
			"secure": c.Secure, "httpOnly": c.HTTPOnly}
		if c.Expires > 0 {
			m["expires"] = c.Expires
		}
		out = append(out, m)
	}
	return out
}

// domainMatch reports whether host is domain or one of its subdomains.
func domainMatch(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// site approximates a host's registrable domain ("www.example.co.uk" ->
// "example.co.uk"), so a page's cookies for its image subdomains come too.
func site(host string) string {
	labels := strings.Split(strings.ToLower(host), ".")
	n := 2
	if len(labels) >= 3 {
		switch labels[len(labels)-2] {
		case "co", "com", "net", "org", "gov", "ac", "edu", "ne", "or":
			if len(labels[len(labels)-1]) == 2 { // e.g. .co.uk, .com.au
				n = 3
			}
		}
	}
	if len(labels) <= n {
		return strings.Join(labels, ".")
	}
	return strings.Join(labels[len(labels)-n:], ".")
}
