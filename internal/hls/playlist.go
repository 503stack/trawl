// Package hls parses HLS playlists and downloads their segments in parallel.
package hls

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Variant is one quality level in a master playlist.
type Variant struct {
	URI       string
	Bandwidth int64
	Width     int
	Height    int
	Audio     string // audio rendition group
}

// Rendition is an #EXT-X-MEDIA entry (alternative audio, subtitles...).
type Rendition struct {
	Type, Group, Name, Language, URI string
	Default                          bool
}

// Key is an AES-128 segment key.
type Key struct {
	URI string
	IV  []byte // nil: derive from the media sequence number
}

// Part is a resource, or a byte range of one (Len > 0).
type Part struct {
	URI      string
	Off, Len int64
}

// Range returns the HTTP Range header value for the part, or "".
func (p Part) Range() string {
	if p.Len <= 0 {
		return ""
	}
	return fmt.Sprintf("bytes=%d-%d", p.Off, p.Off+p.Len-1)
}

// Segment is one media segment.
type Segment struct {
	Part
	Duration float64
	Seq      int64
	Key      *Key
	Init     *Part // #EXT-X-MAP initialization section (fMP4)
}

// Master is a multivariant playlist.
type Master struct {
	Variants   []Variant
	Renditions []Rendition
}

// Media is a media playlist.
type Media struct {
	Segments []Segment
	Duration float64
	Live     bool // no #EXT-X-ENDLIST
}

// ErrNotPlaylist means the data does not start with #EXTM3U.
var ErrNotPlaylist = errors.New("not an HLS playlist")

// Parse parses a playlist. Exactly one of the returned playlists is non-nil.
func Parse(data []byte, base *url.URL) (*Master, *Media, error) {
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, nil, ErrNotPlaylist
	}
	resolve := func(u string) (string, error) {
		ref, err := url.Parse(strings.TrimSpace(u))
		if err != nil {
			return "", err
		}
		return base.ResolveReference(ref).String(), nil
	}

	var (
		master   Master
		media    Media
		isMaster bool
		ended    bool
		seq      int64
		dur      float64
		key      *Key
		init     *Part
		variant  *Variant
		err      error
		rng      *Part                // pending #EXT-X-BYTERANGE
		nextOff  = map[string]int64{} // where an offset-less byte range continues
	)
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		tag, val, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXT-X-STREAM-INF":
			isMaster = true
			a := attrs(val)
			v := Variant{Audio: a["AUDIO"]}
			v.Bandwidth, _ = strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			if r := a["RESOLUTION"]; r != "" {
				fmt.Sscanf(strings.ToLower(r), "%dx%d", &v.Width, &v.Height)
			}
			variant = &v
		case tag == "#EXT-X-MEDIA":
			isMaster = true
			a := attrs(val)
			r := Rendition{Type: a["TYPE"], Group: a["GROUP-ID"], Name: a["NAME"], Language: a["LANGUAGE"], Default: a["DEFAULT"] == "YES"}
			if a["URI"] != "" {
				if r.URI, err = resolve(a["URI"]); err != nil {
					return nil, nil, err
				}
			}
			master.Renditions = append(master.Renditions, r)
		case tag == "#EXT-X-MEDIA-SEQUENCE":
			seq, _ = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		case tag == "#EXTINF":
			d, _, _ := strings.Cut(val, ",")
			dur, _ = strconv.ParseFloat(strings.TrimSpace(d), 64)
		case tag == "#EXT-X-KEY":
			a := attrs(val)
			switch a["METHOD"] {
			case "", "NONE":
				key = nil
			case "AES-128":
				k := &Key{}
				if k.URI, err = resolve(a["URI"]); err != nil {
					return nil, nil, err
				}
				if iv := a["IV"]; iv != "" {
					b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X"))
					if err != nil || len(b) != 16 {
						return nil, nil, fmt.Errorf("bad IV %q", iv)
					}
					k.IV = b
				}
				key = k
			default:
				return nil, nil, fmt.Errorf("encryption %s is not supported (DRM)", a["METHOD"])
			}
		case tag == "#EXT-X-MAP":
			a := attrs(val)
			p := &Part{}
			if p.URI, err = resolve(a["URI"]); err != nil {
				return nil, nil, err
			}
			if br := a["BYTERANGE"]; br != "" {
				if p.Len, p.Off, err = byteRange(br); err != nil {
					return nil, nil, err
				}
				p.Off = max(p.Off, 0)
			}
			init = p
		case tag == "#EXT-X-BYTERANGE":
			p := &Part{}
			if p.Len, p.Off, err = byteRange(val); err != nil {
				return nil, nil, err
			}
			rng = p
		case tag == "#EXT-X-ENDLIST":
			ended = true
		case strings.HasPrefix(line, "#"):
		default:
			u, err := resolve(line)
			if err != nil {
				return nil, nil, err
			}
			if variant != nil {
				variant.URI = u
				master.Variants = append(master.Variants, *variant)
				variant = nil
				continue
			}
			seg := Segment{Part: Part{URI: u}, Duration: dur, Seq: seq, Key: key, Init: init}
			if rng != nil {
				if rng.Off < 0 {
					rng.Off = nextOff[u]
				}
				seg.Off, seg.Len = rng.Off, rng.Len
				nextOff[u] = rng.Off + rng.Len
				rng = nil
			}
			media.Segments = append(media.Segments, seg)
			media.Duration += dur
			seq++
			dur = 0
		}
	}
	if isMaster {
		if len(master.Variants) == 0 {
			return nil, nil, errors.New("master playlist has no variants")
		}
		return &master, nil, nil
	}
	if len(media.Segments) == 0 {
		return nil, nil, errors.New("playlist has no segments")
	}
	media.Live = !ended
	return nil, &media, nil
}

// Best returns the variant with the highest bandwidth (then resolution).
func (m *Master) Best() Variant {
	vs := append([]Variant(nil), m.Variants...)
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Bandwidth != vs[j].Bandwidth {
			return vs[i].Bandwidth > vs[j].Bandwidth
		}
		return vs[i].Height > vs[j].Height
	})
	return vs[0]
}

// Audio returns the audio rendition to use for group, preferring DEFAULT=YES.
// It returns nil when the group has no separate audio playlist.
func (m *Master) Audio(group string) *Rendition {
	var first *Rendition
	for i := range m.Renditions {
		r := &m.Renditions[i]
		if r.Type != "AUDIO" || r.Group != group || r.URI == "" {
			continue
		}
		if r.Default {
			return r
		}
		if first == nil {
			first = r
		}
	}
	return first
}

// byteRange parses "<length>[@<offset>]"; a missing offset is returned as -1.
func byteRange(s string) (n, off int64, err error) {
	ls, offs, hasOff := strings.Cut(strings.TrimSpace(s), "@")
	if n, err = strconv.ParseInt(ls, 10, 64); err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("bad byte range %q", s)
	}
	off = -1
	if hasOff {
		if off, err = strconv.ParseInt(offs, 10, 64); err != nil || off < 0 {
			return 0, 0, fmt.Errorf("bad byte range %q", s)
		}
	}
	return n, off, nil
}

// attrs parses an attribute list: KEY=value,KEY2="quoted, value".
func attrs(s string) map[string]string {
	m := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return m
		}
		k := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				v, s = s[1:], ""
			} else {
				v, s = s[1:1+end], s[2+end:]
			}
		} else if end := strings.IndexByte(s, ','); end >= 0 {
			v, s = s[:end], s[end:]
		} else {
			v, s = s, ""
		}
		m[k] = strings.TrimSpace(v)
	}
}
