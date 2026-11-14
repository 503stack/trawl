// Package media inspects, checks and remuxes downloaded files using
// ffprobe and ffmpeg.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/503stack/trawl/internal/fetch"
)

// Info is what ffprobe found.
type Info struct {
	Format   string  `json:"format"`
	Duration float64 `json:"duration"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	VCodec   string  `json:"vcodec,omitempty"`
	ACodec   string  `json:"acodec,omitempty"`
}

// Sniff returns a MIME type based on the first bytes of the file.
func Sniff(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 512)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	b = b[:n]
	switch {
	case n >= 8 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "moof" || string(b[4:8]) == "styp"):
		return "video/mp4", nil
	case n > 0 && b[0] == 0x47 && (n <= 188 || b[188] == 0x47):
		return "video/mp2t", nil
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "video/x-matroska", nil
	case bytes.HasPrefix(b, []byte("#EXTM3U")):
		return "application/vnd.apple.mpegurl", nil
	case fetch.LooksLikeErrorPage(b):
		return "text/html", nil
	}
	return http.DetectContentType(b), nil
}

// IsMedia reports whether a MIME type is audio or video.
func IsMedia(ctype string) bool {
	return strings.HasPrefix(ctype, "video/") || strings.HasPrefix(ctype, "audio/") || ctype == "application/ogg"
}

var mediaExts = map[string]bool{".mp4": true, ".m4v": true, ".m4a": true, ".mkv": true, ".webm": true, ".ts": true,
	".mov": true, ".avi": true, ".flv": true, ".mp3": true, ".aac": true, ".ogg": true, ".opus": true, ".flac": true, ".wav": true}

// IsMediaExt reports whether ext is a known audio/video extension.
func IsMediaExt(ext string) bool { return mediaExts[strings.ToLower(ext)] }

// Probe runs ffprobe on path.
func Probe(ctx context.Context, path string) (*Info, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %s", firstLines(stderr.String(), err))
	}
	var p struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Duration    string `json:"duration"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}
	info := &Info{Format: p.Format.FormatName}
	info.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	for _, s := range p.Streams {
		switch {
		case s.CodecType == "video" && s.Disposition.AttachedPic == 0 && info.VCodec == "":
			info.VCodec, info.Width, info.Height = s.CodecName, s.Width, s.Height
		case s.CodecType == "audio" && info.ACodec == "":
			info.ACodec = s.CodecName
		}
		if info.Duration == 0 {
			info.Duration, _ = strconv.ParseFloat(s.Duration, 64)
		}
	}
	return info, nil
}

// Expect describes what a downloaded media file should look like.
type Expect struct {
	Duration     float64 // seconds, 0 = unknown
	Height       int     // 0 = unknown
	MinSize      int64
	Tolerance    float64 // fraction of Duration
	RequireAudio bool
}

// Check compares probe results with expectations. Errors fail the job;
// warnings are only reported.
func Check(info *Info, size int64, e Expect) (warnings []string, err error) {
	if info.VCodec == "" && info.ACodec == "" {
		return nil, errors.New("no audio or video streams found")
	}
	if size < e.MinSize {
		return nil, fmt.Errorf("file is only %d bytes (minimum %d)", size, e.MinSize)
	}
	if e.RequireAudio && info.VCodec != "" && info.ACodec == "" {
		return nil, errors.New("video has no audio track")
	}
	if e.Duration > 0 {
		tol := math.Max(e.Duration*e.Tolerance, 2)
		if math.Abs(info.Duration-e.Duration) > tol {
			return nil, fmt.Errorf("duration is %s but the playlist says %s", Clock(info.Duration), Clock(e.Duration))
		}
	}
	if e.Height > 0 && info.Height > 0 && info.Height != e.Height {
		warnings = append(warnings, fmt.Sprintf("expected %dp, got %dx%d", e.Height, info.Width, info.Height))
	}
	return warnings, nil
}

// Decode decodes every frame and fails on any decoder error. Slow.
func Decode(ctx context.Context, path string) error {
	var stderr limitedBuffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "error", "-i", path, "-f", "null", "-")
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil || stderr.Len() > 0 {
		return fmt.Errorf("decode check: %s", firstLines(stderr.String(), err))
	}
	return nil
}

// Remux copies the streams of inputs into dst without re-encoding. With two
// inputs the video comes from the first and the audio from the second.
func Remux(ctx context.Context, inputs []string, dst string) error {
	args := []string{"-hide_banner", "-nostdin", "-v", "error", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	if len(inputs) > 1 {
		args = append(args, "-map", "0:v", "-map", "1:a")
	} else {
		args = append(args, "-map", "0:v?", "-map", "0:a?")
	}
	args = append(args, "-c", "copy")
	if strings.HasSuffix(dst, ".mp4") || strings.HasSuffix(dst, ".m4a") {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, dst)
	var stderr limitedBuffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("ffmpeg: %s", firstLines(stderr.String(), err))
	}
	return nil
}

// ExtFor returns the natural extension for a probed file, or "".
func ExtFor(info *Info) string {
	f := info.Format
	switch {
	case strings.Contains(f, "mp4") || strings.Contains(f, "mov"):
		if info.VCodec == "" {
			return ".m4a"
		}
		return ".mp4"
	case strings.Contains(f, "matroska") || strings.Contains(f, "webm"):
		if (info.VCodec == "vp8" || info.VCodec == "vp9" || info.VCodec == "av1") &&
			(info.ACodec == "" || info.ACodec == "opus" || info.ACodec == "vorbis") {
			return ".webm"
		}
		return ".mkv"
	}
	for _, name := range []string{"mpegts", "mp3", "aac", "flv", "avi", "ogg", "wav", "flac"} {
		if strings.Contains(f, name) {
			if name == "mpegts" {
				return ".ts"
			}
			return "." + name
		}
	}
	return ""
}

// NeedsRemux reports whether turning a file of this format into ext needs a
// container change rather than a rename.
func NeedsRemux(info *Info, ext string) bool {
	switch ext {
	case ".mp4", ".m4a", ".m4v", ".mov":
		return !strings.Contains(info.Format, "mp4")
	case ".mkv", ".webm":
		return !strings.Contains(info.Format, "matroska")
	}
	return false
}

var typeExts = map[string]string{
	"application/zip": ".zip", "application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png",
	"image/gif": ".gif", "image/webp": ".webp", "application/x-gzip": ".gz", "application/x-rar-compressed": ".rar",
	"application/x-7z-compressed": ".7z", "text/plain": ".txt", "video/mp4": ".mp4", "video/mp2t": ".ts",
	"video/x-matroska": ".mkv", "video/webm": ".webm", "audio/mpeg": ".mp3", "video/avi": ".avi",
}

// ExtForType maps a sniffed MIME type to an extension, or "".
func ExtForType(ctype string) string {
	t, _, _ := strings.Cut(ctype, ";")
	return typeExts[strings.TrimSpace(t)]
}

// SHA256 hashes a file.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Clock formats seconds as h:mm:ss.
func Clock(sec float64) string {
	s := int(math.Round(sec))
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func firstLines(s string, err error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		if err != nil {
			return err.Error()
		}
		return "unknown error"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 3 {
		lines = append(lines[:3], fmt.Sprintf("(+%d more lines)", len(lines)-3))
	}
	return strings.Join(lines, " | ")
}

// limitedBuffer keeps the first 8 KiB written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 8<<10 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
