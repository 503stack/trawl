// Package naming picks, cleans and places output filenames without ever
// overwriting an existing file.
package naming

import (
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Namer turns titles and URL fragments into clean file names.
type Namer struct {
	Sep      string         // replaces spaces and illegal characters
	ASCII    bool           // fold accents (é→e, ø→o) and drop other non-ASCII
	MaxBytes int            // limit for the base name
	Strip    *regexp.Regexp // removed from titles before cleaning
}

var fold = map[rune]string{
	'ß': "ss", 'æ': "ae", 'Æ': "AE", 'ø': "o", 'Ø': "O", 'œ': "oe", 'Œ': "OE",
	'ł': "l", 'Ł': "L", 'đ': "d", 'Đ': "D", 'ð': "d", 'Ð': "D", 'þ': "th", 'Þ': "Th",
}

var dashRe = regexp.MustCompile(`\s*[-–—]+\s*`)

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// Clean normalizes s into a safe base name (no extension handling).
func (n *Namer) Clean(s string) string {
	s = decode(s)
	if n.Strip != nil {
		s = n.Strip.ReplaceAllString(s, "")
	}
	s = norm.NFC.String(s)
	s = dashRe.ReplaceAllString(s, "-")
	if n.ASCII {
		s = toASCII(s)
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case strings.ContainsRune("'’‘`\"“”´", r):
			// "Don't" → "Dont", not "Don_t"
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.()[]+,", r)):
			b.WriteRune(r)
		case r >= 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)):
			b.WriteRune(r)
		default:
			b.WriteString(n.Sep)
		}
	}
	s = b.String()
	for _, pair := range [][2]string{{n.Sep + n.Sep, n.Sep}, {n.Sep + "-", "-"}, {"-" + n.Sep, "-"}, {"--", "-"}} {
		for strings.Contains(s, pair[0]) {
			s = strings.ReplaceAll(s, pair[0], pair[1])
		}
	}
	trim := n.Sep + ".-,_ "
	s = strings.Trim(s, trim)
	for n.MaxBytes > 0 && len(s) > n.MaxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	s = strings.Trim(s, trim)
	if reserved[strings.ToUpper(s)] {
		s += n.Sep
	}
	return s
}

// decode undoes URL percent-encoding ("My%20Clip") and HTML entities
// ("Tom &amp; Jerry"), including double encoding like "%2520".
func decode(s string) string {
	for range 3 {
		d := html.UnescapeString(s)
		if strings.Contains(d, "%") {
			if u, err := url.PathUnescape(d); err == nil {
				d = u
			}
		}
		if d == s {
			break
		}
		s = d
	}
	return s
}

func toASCII(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r):
		case fold[r] != "":
			b.WriteString(fold[r])
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
}

var junkNames = map[string]bool{
	"index": true, "playlist": true, "master": true, "video": true, "videos": true, "stream": true,
	"chunklist": true, "media": true, "file": true, "download": true, "output": true, "manifest": true,
	"prog_index": true, "main": true, "default": true, "hls": true, "embed": true, "watch": true,
	"player": true, "get": true, "view": true, "play": true, "content": true, "mono": true, "out": true,
}

var junkRe = regexp.MustCompile(`^(\d{3,4}p|[\d.]+|[0-9a-f-]{12,}|v\d+)$`)

// Junk reports whether a cleaned name carries no meaning, e.g. "360p",
// "index", a hash or a random token.
func Junk(s string) bool {
	l := strings.ToLower(s)
	if junkNames[l] || junkRe.MatchString(l) {
		return true
	}
	// Random tokens like "BqTPEyJ6f1mAAJ69h6I1RQ": long, alphanumeric, mixed.
	if len(s) >= 16 {
		var letter, digit bool
		for _, r := range s {
			switch {
			case r >= '0' && r <= '9':
				digit = true
			case r < 0x80 && unicode.IsLetter(r):
				letter = true
			default:
				return false
			}
		}
		return letter && digit
	}
	return false
}

// Pick returns the first candidate that is non-empty and not junk after cleaning.
func (n *Namer) Pick(candidates ...string) string {
	for _, c := range candidates {
		if s := n.Clean(c); s != "" && !Junk(s) {
			return s
		}
	}
	return ""
}

var extRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)

// SplitExt splits "My File.vid" into "My File" and ".vid". Things that do not
// look like an extension stay part of the base name.
func SplitExt(name string) (string, string) {
	ext := path.Ext(name)
	if !extRe.MatchString(ext) {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), strings.ToLower(ext)
}

// ApplyRules maps an extension through the user's rules (".vid" → ".mp4").
func ApplyRules(ext string, rules map[string]string) string {
	if to, ok := rules[strings.ToLower(ext)]; ok {
		return to
	}
	return ext
}

// Place moves src into dir as base+ext. It never overwrites: if the name is
// taken it tries base_1+ext, base_2+ext, ... Hard links make the claim atomic;
// across filesystems it falls back to an exclusive create and copy.
func Place(src, dir, base, ext, sep string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 10000; i++ {
		name := base + ext
		if i > 0 {
			name = fmt.Sprintf("%s%s%d%s", base, sep, i, ext)
		}
		dst := filepath.Join(dir, name)
		err := os.Link(src, dst)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			err = copyExclusive(src, dst)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
		}
		if err != nil {
			return "", err
		}
		os.Remove(src)
		return dst, nil
	}
	return "", fmt.Errorf("no free name for %s%s in %s", base, ext, dir)
}

func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
