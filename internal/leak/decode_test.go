package leak_test

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
)

func le16(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func be16(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u>>8), byte(u))
	}
	return b
}

func u32(s string, big bool) []byte {
	var b []byte
	for _, r := range s {
		if big {
			b = binary.BigEndian.AppendUint32(b, uint32(r))
		} else {
			b = binary.LittleEndian.AppendUint32(b, uint32(r))
		}
	}
	return b
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestDecode(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		enc  leak.Encoding
	}{
		{"plain", []byte("a=1\n"), "a=1\n", leak.Plain},
		{"utf-8 bom", cat([]byte{0xEF, 0xBB, 0xBF}, []byte("x\n")), "x\n", leak.Plain},
		{"latin-1", []byte("caf\xe9\n"), "caf\xe9\n", leak.Plain},
		{"utf-16le bom", cat([]byte{0xFF, 0xFE}, le16("key\n")), "key\n", leak.UTF16},
		{"utf-16be bom", cat([]byte{0xFE, 0xFF}, be16("key\n")), "key\n", leak.UTF16},
		{"utf-16le, no bom", le16("plain ascii text\n"), "plain ascii text\n", leak.UTF16},
		{"utf-32le bom", cat([]byte{0xFF, 0xFE, 0, 0}, u32("key\n", false)), "key\n", leak.UTF32},
		{"utf-32be bom", cat([]byte{0, 0, 0xFE, 0xFF}, u32("key\n", true)), "key\n", leak.UTF32},
		{"binary", []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 1, 2, 0, 3}, "", leak.Binary},
	}
	for _, c := range cases {
		got, enc := leak.Decode(c.in)
		if got != c.want || enc != c.enc {
			t.Errorf("%s: Decode = %q, %s; want %q, %s", c.name, got, enc, c.want, c.enc)
		}
	}
}

// FF FE followed by plain text whose first NUL sits at byte 8000 or 8001:
// the BOM check must use the binary check's window, so this is text and
// is scanned raw rather than decoded as UTF-16 garbage.
func TestBOMWindowMatchesTheBinaryCheck(t *testing.T) {
	for _, nul := range []int{8000, 8001} {
		b := cat([]byte{0xFF, 0xFE}, []byte("TOKEN=secret-value\n"))
		for len(b) < nul {
			b = append(b, 'a')
		}
		b = append(b, 0, 'z')
		text, enc := leak.Decode(b)
		if enc != leak.Plain || !strings.Contains(text, "TOKEN=secret-value") {
			t.Errorf("first NUL at %d: Decode = %s, contains secret: %v", nul, enc, strings.Contains(text, "TOKEN=secret-value"))
		}
	}
}

func TestRunsFindASCIIAtEveryStride(t *testing.T) {
	const secret = "ghp_runs_secret_123"
	bin := []byte{0x89, 'P', 'N', 'G', 0, 0, 1, 2}
	cases := map[string][]byte{
		"stride 1":              cat(bin, []byte(secret), []byte{0, 0}),
		"utf-16le":              cat(bin, le16(secret)),
		"utf-16be":              cat(bin, be16(secret)),
		"utf-16le, odd offset":  cat(bin, []byte{1}, le16(secret)),
		"utf-32le":              cat(bin, u32(secret, false)),
		"utf-32be, offset by 3": cat(bin, []byte{1, 2, 3}, u32(secret, true)),
	}
	for name, b := range cases {
		if !strings.Contains(leak.Runs(b), secret) {
			t.Errorf("%s: Runs missed the secret: %q", name, leak.Runs(b))
		}
	}
	if r := leak.Runs([]byte("short\x00tiny\x00")); r != "" {
		t.Errorf("runs under 8 characters must be dropped, got %q", r)
	}
}
