// Package config reads trawl settings from TRAWL_* environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Folder is a named download destination.
type Folder struct {
	Name string `json:"name"`
	Path string `json:"-"`
}

// Config holds all runtime settings.
type Config struct {
	Listen    string
	DataDir   string
	TempDir   string
	Folders   []Folder
	Password  string
	NotifyURL string

	MaxJobs        int
	MaxConnections int
	JobConnections int
	ChunkSize      int64
	Retries        int
	UserAgent      string
	HTTP2          bool

	NameSeparator string
	NameASCII     bool
	NameMaxBytes  int
	NameStrip     *regexp.Regexp
	ExtRules      map[string]string

	VerifyLevel       string  // transfer | probe | decode
	DurationTolerance float64 // fraction, e.g. 0.01
	MinSize           int64
	RequireAudio      bool
	Dedupe            string // skip | keep | off
}

// DefaultUserAgent is sent when a job does not set its own User-Agent.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

// Defaults returns the configuration used when no environment is set.
func Defaults() *Config {
	return &Config{
		Listen:            ":8080",
		DataDir:           "/data",
		Folders:           []Folder{{Name: "downloads", Path: "/downloads"}},
		MaxJobs:           2,
		MaxConnections:    32,
		JobConnections:    16,
		ChunkSize:         8 << 20,
		Retries:           5,
		UserAgent:         DefaultUserAgent,
		NameSeparator:     "_",
		NameASCII:         true,
		NameMaxBytes:      200,
		ExtRules:          map[string]string{},
		VerifyLevel:       "probe",
		DurationTolerance: 0.01,
		MinSize:           1 << 20,
		RequireAudio:      true,
		Dedupe:            "skip",
	}
}

// Load builds a Config from defaults overridden by the environment.
func Load() (*Config, error) {
	c := Defaults()
	p := &parser{}

	c.Listen = p.str("TRAWL_LISTEN", c.Listen)
	c.DataDir = p.str("TRAWL_DATA", c.DataDir)
	c.TempDir = p.str("TRAWL_TEMP", filepath.Join(c.DataDir, "tmp"))
	if v := p.str("TRAWL_FOLDERS", ""); v != "" {
		c.Folders = p.folders(v)
	}
	c.Password = p.str("TRAWL_PASSWORD", "")
	c.NotifyURL = p.str("TRAWL_NOTIFY_URL", "")

	c.MaxJobs = p.int("TRAWL_MAX_JOBS", c.MaxJobs)
	c.MaxConnections = p.int("TRAWL_MAX_CONNECTIONS", c.MaxConnections)
	c.JobConnections = p.int("TRAWL_JOB_CONNECTIONS", c.JobConnections)
	c.ChunkSize = p.size("TRAWL_CHUNK_SIZE", c.ChunkSize)
	c.Retries = p.int("TRAWL_RETRIES", c.Retries)
	c.UserAgent = p.str("TRAWL_USER_AGENT", c.UserAgent)
	c.HTTP2 = p.bool("TRAWL_HTTP2", c.HTTP2)

	c.NameSeparator = p.str("TRAWL_NAME_SEPARATOR", c.NameSeparator)
	c.NameASCII = p.bool("TRAWL_NAME_ASCII", c.NameASCII)
	c.NameMaxBytes = p.int("TRAWL_NAME_MAX_BYTES", c.NameMaxBytes)
	if v := p.str("TRAWL_NAME_STRIP", ""); v != "" {
		re, err := regexp.Compile(v)
		p.check("TRAWL_NAME_STRIP", err)
		c.NameStrip = re
	}
	if v := p.str("TRAWL_EXT_RULES", ""); v != "" {
		c.ExtRules = p.extRules(v)
	}

	c.VerifyLevel = p.str("TRAWL_VERIFY", c.VerifyLevel)
	c.DurationTolerance = p.percent("TRAWL_VERIFY_DURATION_TOLERANCE", c.DurationTolerance)
	c.MinSize = p.size("TRAWL_VERIFY_MIN_SIZE", c.MinSize)
	c.RequireAudio = p.bool("TRAWL_VERIFY_REQUIRE_AUDIO", c.RequireAudio)
	c.Dedupe = p.str("TRAWL_DEDUPE", c.Dedupe)

	if !ValidVerify(c.VerifyLevel) {
		p.check("TRAWL_VERIFY", errors.New("must be transfer, probe or decode"))
	}
	switch c.Dedupe {
	case "skip", "keep", "off":
	default:
		p.check("TRAWL_DEDUPE", errors.New("must be skip, keep or off"))
	}
	if c.NameSeparator != "_" && c.NameSeparator != "-" && c.NameSeparator != "." {
		p.check("TRAWL_NAME_SEPARATOR", errors.New("must be _, - or ."))
	}
	if c.MaxJobs < 1 || c.MaxConnections < 1 || c.JobConnections < 1 {
		p.check("TRAWL_MAX_*", errors.New("job and connection limits must be at least 1"))
	}
	if c.ChunkSize < 64<<10 {
		p.check("TRAWL_CHUNK_SIZE", errors.New("must be at least 64KiB"))
	}
	if len(c.Folders) == 0 {
		p.check("TRAWL_FOLDERS", errors.New("no folders configured"))
	}
	return c, errors.Join(p.errs...)
}

// ValidVerify reports whether s is a known verification level.
func ValidVerify(s string) bool {
	return s == "transfer" || s == "probe" || s == "decode"
}

// Folder returns the folder with the given name, or the first folder if name is empty.
func (c *Config) Folder(name string) (Folder, bool) {
	if name == "" {
		return c.Folders[0], true
	}
	for _, f := range c.Folders {
		if f.Name == name {
			return f, true
		}
	}
	return Folder{}, false
}

type parser struct{ errs []error }

func (p *parser) check(key string, err error) {
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %w", key, err))
	}
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) int(key string, def int) int {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	p.check(key, err)
	return n
}

func (p *parser) bool(key string, def bool) bool {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	p.check(key, err)
	return b
}

func (p *parser) size(key string, def int64) int64 {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := ParseSize(v)
	p.check(key, err)
	return n
}

func (p *parser) percent(key string, def float64) float64 {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	p.check(key, err)
	return f / 100
}

// folders parses "name:/path;name2:/path2". A bare path uses its base name.
func (p *parser) folders(v string) []Folder {
	var out []Folder
	for _, item := range strings.Split(v, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, path, ok := strings.Cut(item, ":")
		if !ok {
			name, path = filepath.Base(item), item
		}
		if !filepath.IsAbs(path) {
			p.check("TRAWL_FOLDERS", fmt.Errorf("%q is not an absolute path", path))
			continue
		}
		out = append(out, Folder{Name: strings.TrimSpace(name), Path: filepath.Clean(path)})
	}
	return out
}

// extRules parses ".vid=.mp4,.ts=.mp4".
func (p *parser) extRules(v string) map[string]string {
	rules := map[string]string{}
	for _, item := range strings.Split(v, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || from == "" || to == "" {
			p.check("TRAWL_EXT_RULES", fmt.Errorf("bad rule %q, want .from=.to", item))
			continue
		}
		rules[NormExt(from)] = NormExt(to)
	}
	return rules
}

// NormExt lowercases an extension and makes sure it starts with a dot.
func NormExt(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	if e != "" && !strings.HasPrefix(e, ".") {
		e = "." + e
	}
	return e
}

var sizeRe = regexp.MustCompile(`(?i)^\s*(\d+(?:\.\d+)?)\s*([kmgt]?)i?b?\s*$`)

// ParseSize parses sizes like "8MB", "512k" or "1GiB" (binary units).
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	shift := map[string]uint{"": 0, "k": 10, "m": 20, "g": 30, "t": 40}[strings.ToLower(m[2])]
	return int64(f * float64(int64(1)<<shift)), nil
}
