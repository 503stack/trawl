package naming

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestClean(t *testing.T) {
	n := &Namer{Sep: "_", ASCII: true, MaxBytes: 40}
	cases := map[string]string{
		"My Video: Part 2 / Final?":    "My_Video_Part_2_Final",
		"  Don't Stop  —  Live 2026  ": "Dont_Stop-Live_2026",
		"Smörgåsbord på Ø":             "Smorgasbord_pa_O",
		"Straße <test>|*":              "Strasse_test",
		"...hidden.":                   "hidden",
		"CON":                          "CON_",
		"日本語 title":                    "title",
		"a very long title that goes on and on forever": "a_very_long_title_that_goes_on_and_on_fo",
	}
	for in, want := range cases {
		if got := n.Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	u := &Namer{Sep: "-", MaxBytes: 200}
	if got := u.Clean("日本語 タイトル"); got != "日本語-タイトル" {
		t.Errorf("unicode mode: got %q", got)
	}
	s := &Namer{Sep: "_", ASCII: true, Strip: regexp.MustCompile(`\s*[-|]\s*SiteName$`)}
	if got := s.Clean("Great Clip | SiteName"); got != "Great_Clip" {
		t.Errorf("strip: got %q", got)
	}
}

func TestJunk(t *testing.T) {
	for _, s := range []string{"360p", "index", "master", "1.0", "BqTPEyJ6f1mAAJ69h6I1RQ", "f6b2c46964b5bb4e", "1794692248"} {
		if !Junk(s) {
			t.Errorf("Junk(%q) = false", s)
		}
	}
	for _, s := range []string{"My_Video", "holiday2026", "Interview_With_Someone_2026"} {
		if Junk(s) {
			t.Errorf("Junk(%q) = true", s)
		}
	}
	n := &Namer{Sep: "_", ASCII: true}
	if got := n.Pick("", "360p", "Real Title"); got != "Real_Title" {
		t.Errorf("Pick = %q", got)
	}
}

func TestSplitExt(t *testing.T) {
	for in, want := range map[string][2]string{
		"clip.VID": {"clip", ".vid"}, "Vol. 2": {"Vol. 2", ""}, "noext": {"noext", ""}, "a.tar.gz": {"a.tar", ".gz"},
	} {
		b, e := SplitExt(in)
		if b != want[0] || e != want[1] {
			t.Errorf("SplitExt(%q) = %q %q", in, b, e)
		}
	}
}

func TestPlaceNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	var got []string
	for i := range 3 {
		src := filepath.Join(t.TempDir(), "src")
		os.WriteFile(src, []byte{byte(i)}, 0o644)
		p, err := Place(src, dir, "clip", ".mp4", "_")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.Base(p))
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Errorf("source not removed")
		}
	}
	want := []string{"clip.mp4", "clip_1.mp4", "clip_2.mp4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
		b, _ := os.ReadFile(filepath.Join(dir, want[i]))
		if len(b) != 1 || b[0] != byte(i) {
			t.Errorf("%s has wrong content", want[i])
		}
	}
}
