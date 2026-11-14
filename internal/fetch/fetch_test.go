package fetch

import "testing"

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
