// Package page reads a web page's title and finds the video files and HLS
// playlists it links to. It only looks at the HTML: streams that the player
// builds in JavaScript are found by the bookmarklet instead, which sends the
// URLs the browser actually loaded.
package page

import (
	"html"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	metaTitleRe = regexp.MustCompile(`(?is)<meta\s[^>]*(?:property|name)\s*=\s*["'](?:og:title|twitter:title)["'][^>]*>`)
	metaVideoRe = regexp.MustCompile(`(?is)<meta\s[^>]*(?:property|name)\s*=\s*["'](?:og:video(?::url|:secure_url)?|twitter:player:stream)["'][^>]*>`)
	contentRe   = regexp.MustCompile(`(?is)\scontent\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	titleRe     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	srcTagRe    = regexp.MustCompile(`(?is)<(?:video|source)\b[^>]*?\ssrc\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	iframeRe    = regexp.MustCompile(`(?is)<iframe\b[^>]*?\ssrc\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	contentURL  = regexp.MustCompile(`"contentUrl"\s*:\s*"([^"]+)"`)
	absURLRe    = regexp.MustCompile(`(?:https?:)?//[^\s"'<>()\\]+`)
	relURLRe    = regexp.MustCompile(`["'](/[^\s"'<>()\\]+)["']`)
	heightRe    = regexp.MustCompile(`(?:^|\D)(2160|1440|1080|720|540|480|360|240)(?:p|\D|$)`)
	extraRe     = regexp.MustCompile(`(?i)preview|trailer|teaser|thumb|sprite|promo|advert|(?:^|[/_.-])ads?(?:[/_.-]|$)`)
)

// unescaper undoes the escaping found in JSON and inline scripts.
var unescaper = strings.NewReplacer(`\/`, `/`, `/`, `/`, `/`, `/`, `&`, `&`, `=`, `=`, `=`, `=`)

// Title returns og:title, twitter:title or <title>, or "".
func Title(data []byte) string {
	if tag := metaTitleRe.Find(data); tag != nil {
		if t := strings.TrimSpace(attr(contentRe, tag)); t != "" {
			return t
		}
	}
	if t := titleRe.FindSubmatch(data); t != nil {
		return strings.TrimSpace(html.UnescapeString(string(t[1])))
	}
	return ""
}

// Find returns the video and playlist URLs in a page, resolved against base,
// in the order they were found: tagged sources first, then URLs in scripts.
func Find(data []byte, base *url.URL) []string {
	var raw []string
	for _, tag := range metaVideoRe.FindAll(data, -1) {
		raw = append(raw, attr(contentRe, tag))
	}
	for _, m := range srcTagRe.FindAllSubmatch(data, -1) {
		raw = append(raw, html.UnescapeString(string(m[1])+string(m[2])))
	}
	text := html.UnescapeString(unescaper.Replace(string(data)))
	for _, m := range contentURL.FindAllStringSubmatch(text, -1) {
		raw = append(raw, m[1])
	}
	raw = append(raw, absURLRe.FindAllString(text, -1)...)
	for _, m := range relURLRe.FindAllStringSubmatch(text, -1) {
		if !strings.HasPrefix(m[1], "//") {
			raw = append(raw, m[1])
		}
	}
	return Clean(raw, base, false)
}

// Iframes returns the http(s) iframe sources in a page, resolved against base.
func Iframes(data []byte, base *url.URL) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range iframeRe.FindAllSubmatch(data, -1) {
		if u := resolve(html.UnescapeString(string(m[1])+string(m[2])), base); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// Clean resolves URLs against base and keeps unique http(s) URLs whose path
// ends in a video or playlist extension. With anyExt, the extension is not
// checked: for URLs the browser played in a <video> element, such as a site's
// own ".vid" or an extensionless stream URL.
func Clean(raw []string, base *url.URL, anyExt bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range raw {
		u := resolve(strings.TrimSpace(r), base)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if anyExt || kind(u) > 0 {
			out = append(out, u)
		}
	}
	return out
}

// Best orders candidates best first. HLS playlists beat files (trawl then
// picks the best variant itself); files are ranked by the resolution in their
// URL. Previews, trailers, thumbnails and ads go last. Ties keep their order,
// so put the most trusted candidates first.
func Best(cands []string) []string {
	out := append([]string(nil), cands...)
	sort.SliceStable(out, func(a, b int) bool { return score(out[a]) > score(out[b]) })
	return out
}

func score(u string) int {
	p, _ := url.Parse(u)
	s := 0
	switch kind(u) {
	case 2:
		s = 1000
		if strings.Contains(strings.ToLower(path.Base(p.Path)), "master") {
			s += 300 // more than any resolution hint
		}
	default: // a file; unknown extensions only come from the browser
		s = 500
	}
	if m := heightRe.FindStringSubmatch(p.Path); m != nil {
		h, _ := strconv.Atoi(m[1])
		s += h / 10
	}
	if IsExtra(u) {
		s -= 800
	}
	return s
}

// IsExtra reports whether a URL looks like a preview, trailer, thumbnail or
// ad rather than the video itself. Such URLs are never picked automatically.
func IsExtra(u string) bool {
	p, err := url.Parse(u)
	return err == nil && extraRe.MatchString(p.Path)
}

// kind is 2 for an HLS playlist, 1 for a video file, 0 otherwise. Besides the
// path, it looks at query values, for CDNs that serve files from a script
// (remote_control.php?file=….mp4).
func kind(u string) int {
	p, err := url.Parse(u)
	if err != nil {
		return 0
	}
	if k := extKind(p.Path); k > 0 {
		return k
	}
	for _, vs := range p.Query() {
		for _, v := range vs {
			if k := extKind(v); k > 0 {
				return k
			}
		}
	}
	return 0
}

// Name returns the file name in a video URL: the last path element, or the
// query value that names the file (remote_control.php?file=x.mp4 → x.mp4).
func Name(u *url.URL) string {
	if extKind(u.Path) == 0 {
		for _, vs := range u.Query() {
			for _, v := range vs {
				if extKind(v) > 0 {
					return path.Base(v)
				}
			}
		}
	}
	return path.Base(u.Path)
}

func extKind(s string) int {
	switch strings.ToLower(path.Ext(s)) {
	case ".m3u8":
		return 2
	case ".mp4", ".m4v", ".webm", ".mov", ".mkv", ".vid":
		return 1
	}
	return 0
}

func resolve(s string, base *url.URL) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

func attr(re *regexp.Regexp, tag []byte) string {
	if c := re.FindSubmatch(tag); c != nil {
		return html.UnescapeString(string(c[1]) + string(c[2]))
	}
	return ""
}
