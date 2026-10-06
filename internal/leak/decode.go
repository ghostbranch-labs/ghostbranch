package leak

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Encoding is how a blob was read for scanning.
type Encoding string

const (
	Plain  Encoding = "text"
	UTF16  Encoding = "utf-16"
	UTF32  Encoding = "utf-32"
	Binary Encoding = "binary"
)

// sniffLen is how much of a blob is inspected to classify it, like git's
// own binary detection.
const sniffLen = 8000

// minRun is the shortest printable run scanned inside a blob that is not
// text.
const minRun = 8

func head(b []byte) []byte {
	if len(b) > sniffLen {
		return b[:sniffLen]
	}
	return b
}

// Decode classifies a blob and returns its text. A blob is Plain when its
// first sniffLen bytes hold no NUL; a UTF-8 BOM is dropped from the text.
// A UTF-16 BOM counts only when a NUL follows it inside that same window,
// so Latin-1 text starting with the same two bytes stays Plain. UTF-32 with
// a BOM and BOM-less UTF-16 (the 40%/5% NUL test) are decoded. Everything
// else is Binary and returns "".
func Decode(b []byte) (string, Encoding) {
	h := head(b)
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b, h = b[3:], h[3:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}):
		return decodeUTF32(b[4:], false), UTF32
	case bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		return decodeUTF32(b[4:], true), UTF32
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}) && bytes.IndexByte(h[2:], 0) >= 0:
		return decodeUTF16(b[2:], false), UTF16
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}) && bytes.IndexByte(h[2:], 0) >= 0:
		return decodeUTF16(b[2:], true), UTF16
	}
	if bytes.IndexByte(h, 0) < 0 {
		return string(b), Plain
	}
	if bigEndian, ok := looksUTF16(h); ok {
		return decodeUTF16(b, bigEndian), UTF16
	}
	return "", Binary
}

// decodeUTF32 transcodes UTF-32 to UTF-8. A trailing partial unit is
// dropped; invalid code points become U+FFFD.
func decodeUTF32(b []byte, bigEndian bool) string {
	var sb strings.Builder
	for i := 0; i+4 <= len(b); i += 4 {
		var r uint32
		if bigEndian {
			r = binary.BigEndian.Uint32(b[i:])
		} else {
			r = binary.LittleEndian.Uint32(b[i:])
		}
		if r > unicode.MaxRune || (r >= 0xD800 && r <= 0xDFFF) {
			sb.WriteRune(utf8.RuneError)
			continue
		}
		sb.WriteRune(rune(r))
	}
	return sb.String()
}

// Runs returns the printable ASCII runs of at least minRun characters in b,
// one per line: bytes read one at a time, and characters sitting in 2- and
// 4-byte units whose other bytes are NUL, in every alignment and byte
// position. That finds ASCII secrets in binaries and in UTF-16 or UTF-32
// text the decoder's heuristics missed.
func Runs(b []byte) string {
	var out strings.Builder
	appendRuns(&out, b, 1, 0, 0)
	for _, w := range []int{2, 4} {
		for shift := 0; shift < w; shift++ {
			appendRuns(&out, b, w, shift, 0)
			appendRuns(&out, b, w, shift, w-1)
		}
	}
	return out.String()
}

func appendRuns(out *strings.Builder, b []byte, w, shift, at int) {
	var cur []byte
	flush := func() {
		if len(cur) >= minRun {
			out.Write(cur)
			out.WriteByte('\n')
		}
		cur = cur[:0]
	}
	for i := shift; i+w <= len(b); i += w {
		c := b[i+at]
		ok := c == '\t' || (c >= 0x20 && c <= 0x7e)
		for j := 0; ok && j < w; j++ {
			if j != at && b[i+j] != 0 {
				ok = false
			}
		}
		if ok {
			cur = append(cur, c)
		} else {
			flush()
		}
	}
	flush()
}

// looksUTF16 detects BOM-less UTF-16 by its alternating NULs. Text that is
// mostly ASCII or Latin-1 has a NUL in the high byte of nearly every code
// unit: the odd-indexed bytes for little-endian, the even-indexed ones for
// big-endian. The sample is UTF-16 when over 40% of the bytes on one side
// are NUL and at most 5% on the other side are. The 40% floor leaves room for
// non-Latin characters mixed in; the 5% ceiling rejects binaries, whose NULs
// fall on both sides (and UTF-32, whose NULs fill both sides).
func looksUTF16(sample []byte) (bigEndian, ok bool) {
	units := len(sample) / 2
	if units == 0 {
		return false, false
	}
	var evenNUL, oddNUL int
	for i := 0; i+1 < len(sample); i += 2 {
		if sample[i] == 0 {
			evenNUL++
		}
		if sample[i+1] == 0 {
			oddNUL++
		}
	}
	mostly := func(n int) bool { return n*10 > units*4 } // > 40%
	hardly := func(n int) bool { return n*20 <= units }  // <= 5%
	switch {
	case mostly(oddNUL) && hardly(evenNUL):
		return false, true
	case mostly(evenNUL) && hardly(oddNUL):
		return true, true
	}
	return false, false
}

// decodeUTF16 transcodes UTF-16 to UTF-8. A trailing odd byte is dropped and
// unpaired surrogates become U+FFFD; neither stops the rest being scanned.
func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		lo, hi := b[2*i], b[2*i+1]
		if bigEndian {
			lo, hi = hi, lo
		}
		u[i] = uint16(lo) | uint16(hi)<<8
	}
	return string(utf16.Decode(u))
}
