package chunker

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16Bytes encodes s as UTF-16 with the given byte order, prefixing the BOM.
func utf16Bytes(s string, bigEndian bool) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, 2+2*len(units))
	units = append([]uint16{0xFEFF}, units...)
	for _, u := range units {
		if bigEndian {
			buf = append(buf, byte(u>>8), byte(u))
		} else {
			buf = append(buf, byte(u), byte(u>>8))
		}
	}
	return buf
}

func TestNormalizePlainUTF8(t *testing.T) {
	got, err := Normalize([]byte("# Hello\n"))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "# Hello\n" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeStripsUTF8BOM(t *testing.T) {
	got, err := Normalize([]byte("\xEF\xBB\xBF# Hello\n"))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "# Hello\n" {
		t.Fatalf("BOM not stripped: %q", got)
	}
}

func TestNormalizeUTF16LE(t *testing.T) {
	got, err := Normalize(utf16Bytes("# Héllo — ok\n", false))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "# Héllo — ok\n" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeUTF16BE(t *testing.T) {
	got, err := Normalize(utf16Bytes("# Héllo — ok\n", true))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "# Héllo — ok\n" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeUTF16SurrogatePair(t *testing.T) {
	got, err := Normalize(utf16Bytes("emoji: 😀\n", false))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "emoji: 😀\n" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeInvalidUTF8Errors(t *testing.T) {
	_, err := Normalize([]byte("ok \xFF\xFE\xFD garbage"))
	if err == nil {
		t.Fatal("want error for invalid UTF-8")
	}
}

func TestNormalizeUTF16OddLengthErrors(t *testing.T) {
	b := utf16Bytes("abc", false)
	b = append(b, 0x41) // trailing odd byte
	if _, err := Normalize(b); err == nil {
		t.Fatal("want error for odd-length UTF-16")
	}
}

func TestNormalizeUTF16UnpairedSurrogateErrors(t *testing.T) {
	// BOM + lone high surrogate 0xD800, little-endian.
	b := []byte{0xFF, 0xFE, 0x00, 0xD8}
	if _, err := Normalize(b); err == nil {
		t.Fatal("want error for unpaired surrogate")
	}
}

func TestNormalizeEmpty(t *testing.T) {
	got, err := Normalize(nil)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeErrorMentionsReason(t *testing.T) {
	_, err := Normalize([]byte("\xC3")) // truncated multibyte sequence
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error should name the encoding problem: %v", err)
	}
}
