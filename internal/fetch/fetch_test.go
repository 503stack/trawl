package fetch

import (
	"context"
	"net"
	"strconv"
	"testing"
)

func TestLooksLikeErrorPage(t *testing.T) {
	for in, want := range map[string]bool{
		"<!DOCTYPE html><html>":          true,
		"\n  <html><body>gone":           true,
		`{"error":"expired"}`:            true,
		"<?xml version=\"1.0\"?><Error>": true,
		"{\x00\x01\x8f binary":           false,
		"<\x00\x00 binary":               false,
		"\x47\x40\x00\x10":               false,
		"":                               false,
	} {
		if got := LooksLikeErrorPage([]byte(in)); got != want {
			t.Errorf("LooksLikeErrorPage(%q) = %v", in, got)
		}
	}
}

// TestDialerFamily checks that "6" prefers IPv6 and falls back to IPv4, and
// that "4" never uses IPv6. "localhost" must resolve to both ::1 and 127.0.0.1.
func TestDialerFamily(t *testing.T) {
	l6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	defer l6.Close()
	port := strconv.Itoa(l6.Addr().(*net.TCPAddr).Port)
	l4, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Skip("port not free on IPv4")
	}
	defer l4.Close()
	if addrs, _ := net.LookupHost("localhost"); len(addrs) < 2 {
		t.Skip("localhost does not resolve to both families")
	}
	for _, l := range []net.Listener{l4, l6} {
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	family := func(ip, addr string) string {
		t.Helper()
		c, err := dialer(ip)(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("%s %s: %v", ip, addr, err)
		}
		defer c.Close()
		if c.RemoteAddr().(*net.TCPAddr).IP.To4() != nil {
			return "4"
		}
		return "6"
	}
	if got := family("6", "localhost:"+port); got != "6" {
		t.Errorf(`"6" used IPv%s`, got)
	}
	if got := family("4", "localhost:"+port); got != "4" {
		t.Errorf(`"4" used IPv%s`, got)
	}
	l6.Close()
	if got := family("6", "localhost:"+port); got != "4" {
		t.Errorf(`"6" without IPv6 listener used IPv%s`, got)
	}
}
