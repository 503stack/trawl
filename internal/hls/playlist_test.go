package hls

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"net/url"
	"testing"
)

var base, _ = url.Parse("https://cdn.example/path/v/720p.m3u8?token=abc")

func TestParseMaster(t *testing.T) {
	master, media, err := Parse([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English, main",DEFAULT=YES,URI="audio/en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="aud"
360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="aud"
https://other.example/1080p.m3u8
`), base)
	if err != nil || media != nil {
		t.Fatal(err, media)
	}
	b := master.Best()
	if b.Height != 1080 || b.URI != "https://other.example/1080p.m3u8" {
		t.Errorf("best = %+v", b)
	}
	a := master.Audio("aud")
	if a == nil || a.Name != "English, main" || a.URI != "https://cdn.example/path/v/audio/en.m3u8" {
		t.Errorf("audio = %+v", a)
	}
}

func TestParseMedia(t *testing.T) {
	_, m, err := Parse([]byte(`#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
s0.m4s
#EXT-X-KEY:METHOD=AES-128,URI="/k/key.bin",IV=0x000102030405060708090a0b0c0d0e0f
#EXTINF:3.5,
s1.m4s?x=1
#EXT-X-ENDLIST
`), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 2 || m.Duration != 7.5 || m.Live {
		t.Fatalf("media = %+v", m)
	}
	s0, s1 := m.Segments[0], m.Segments[1]
	if s0.Seq != 7 || s0.Key != nil || s0.Init == nil || s0.Init.URI != "https://cdn.example/path/v/init.mp4" {
		t.Errorf("s0 = %+v", s0)
	}
	if s1.Seq != 8 || s1.Key == nil || s1.Key.URI != "https://cdn.example/k/key.bin" || s1.Key.IV[15] != 0x0f || s1.URI != "https://cdn.example/path/v/s1.m4s?x=1" {
		t.Errorf("s1 = %+v", s1)
	}
}

func TestParseByteRange(t *testing.T) {
	_, m, err := Parse([]byte(`#EXTM3U
#EXT-X-MAP:URI="main.mp4",BYTERANGE="720@0"
#EXTINF:6,
#EXT-X-BYTERANGE:1000@720
main.mp4
#EXTINF:6,
#EXT-X-BYTERANGE:2000
main.mp4
#EXT-X-ENDLIST
`), base)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Segments
	if s[0].Init.Range() != "bytes=0-719" || s[0].Range() != "bytes=720-1719" || s[1].Range() != "bytes=1720-3719" {
		t.Errorf("ranges: %q %q %q", s[0].Init.Range(), s[0].Range(), s[1].Range())
	}
}

func TestParseRejects(t *testing.T) {
	if _, _, err := Parse([]byte("<html>"), base); err != ErrNotPlaylist {
		t.Errorf("html: %v", err)
	}
	if _, _, err := Parse([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:1,\na.ts\n"), base); err == nil {
		t.Error("SAMPLE-AES accepted")
	}
}

func TestDecrypt(t *testing.T) {
	key, iv := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)
	plain := []byte("segment data that is not block aligned")
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)
	got, err := Decrypt(enc, key, iv)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
}
