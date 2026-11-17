package page

import (
	"net/url"
	"slices"
	"testing"
)

func TestTitle(t *testing.T) {
	cases := map[string]string{
		`<meta property="og:title" content="Big &amp; Small"><title>x</title>`: "Big & Small",
		`<head><title> Plain </title></head>`:                                  "Plain",
		`<p>none</p>`:                                                          "",
	}
	for in, want := range cases {
		if got := Title([]byte(in)); got != want {
			t.Errorf("Title(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFind(t *testing.T) {
	base, _ := url.Parse("https://site.example/watch/1")
	html := `<html><head>
<meta property="og:video:secure_url" content="https://cdn.example/og.mp4">
<script type="application/ld+json">{"@type":"VideoObject","contentUrl":"https:\/\/cdn.example\/ld.mp4"}</script>
</head><body>
<video src="/v/tag.webm"></video>
<video><source src="rel/source.mp4?a=1&amp;b=2" type="video/mp4"></video>
<video src="blob:https://site.example/123"></video>
<script>player({hls:"https://cdn.example/hls/master.m3u8?t=x", dash:"/x/manifest.mpd",
  kaltura:"//cdn.example/p/video.mp4/index.m3u8", poster:"/img/poster.jpg", again:"https://cdn.example/og.mp4"})</script>
</body></html>`
	got := Find([]byte(html), base)
	want := []string{
		"https://cdn.example/og.mp4",
		"https://site.example/v/tag.webm",
		"https://site.example/watch/rel/source.mp4?a=1&b=2",
		"https://cdn.example/ld.mp4",
		"https://cdn.example/hls/master.m3u8?t=x",
		"https://cdn.example/p/video.mp4/index.m3u8",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Find:\n got %q\nwant %q", got, want)
	}
}

func TestIframes(t *testing.T) {
	base, _ := url.Parse("https://site.example/watch/1")
	got := Iframes([]byte(`<iframe src="//player.example/embed/9"></iframe><iframe src="about:blank"></iframe>`), base)
	if !slices.Equal(got, []string{"https://player.example/embed/9"}) {
		t.Errorf("Iframes = %q", got)
	}
}

func TestBest(t *testing.T) {
	got := Best([]string{
		"https://c.example/ads/spot.m3u8",
		"https://c.example/trailer_1080p.mp4",
		"https://c.example/movie_480p.mp4",
		"https://c.example/movie_720p.mp4",
		"https://c.example/hls/720p.m3u8",
		"https://c.example/hls/master.m3u8",
	})
	want := []string{
		"https://c.example/hls/master.m3u8",
		"https://c.example/hls/720p.m3u8",
		"https://c.example/movie_720p.mp4",
		"https://c.example/movie_480p.mp4",
		"https://c.example/ads/spot.m3u8",
		"https://c.example/trailer_1080p.mp4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Best:\n got %q\nwant %q", got, want)
	}
}

func TestCleanAnyExt(t *testing.T) {
	raw := []string{"https://c.example/cdn/x.vid", "https://c.example/stream?id=1", "blob:https://c.example/1", "/rel.vid"}
	if got := Clean(raw, nil, false); !slices.Equal(got, []string{"https://c.example/cdn/x.vid"}) {
		t.Errorf("Clean = %q", got)
	}
	if got := Clean(raw, nil, true); !slices.Equal(got, raw[:2]) {
		t.Errorf("Clean any = %q", got)
	}
	cdn := []string{"https://cdn.example/remote_control.php?file=abc.mp4&acctoken=x", "https://cdn.example/get.php?id=1"}
	if got := Clean(cdn, nil, false); !slices.Equal(got, cdn[:1]) {
		t.Errorf("Clean query ext = %q", got)
	}
	for in, want := range map[string]string{cdn[0]: "abc.mp4", cdn[1]: "get.php", "https://c.example/v/a.mp4?file=b.mp4": "a.mp4"} {
		u, _ := url.Parse(in)
		if got := Name(u); got != want {
			t.Errorf("Name(%s) = %q, want %q", in, got, want)
		}
	}
	if !IsExtra("https://c.example/p/vidthumb.mp4") || IsExtra("https://c.example/cdn/x.vid") {
		t.Error("IsExtra")
	}
}
