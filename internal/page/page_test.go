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
