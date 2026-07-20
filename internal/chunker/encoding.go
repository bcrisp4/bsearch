package chunker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Normalize detects the input's encoding by BOM sniffing (UTF-8, UTF-16LE,
// UTF-16BE), strips the BOM, and returns UTF-8 text. Inputs without a BOM
// must already be valid UTF-8. Decoding is strict — odd-length UTF-16,
// unpaired surrogates, or invalid UTF-8 return an error rather than
// substituting replacement runes, so undecodable files fail loudly instead
// of being indexed garbled (DESIGN.md: Chunking, encoding).
//
// Chunk byte offsets index into the returned string, not the raw input.
func Normalize(raw []byte) (string, error) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		raw = raw[3:]
	// UTF-32 BOMs before UTF-16: the UTF-32LE BOM (FF FE 00 00) starts
	// with the UTF-16LE BOM and would otherwise be silently decoded to
	// NUL-interleaved garbage.
	case len(raw) >= 4 && raw[0] == 0xFF && raw[1] == 0xFE && raw[2] == 0x00 && raw[3] == 0x00:
		return "", errors.New("UTF-32LE not supported")
	case len(raw) >= 4 && raw[0] == 0x00 && raw[1] == 0x00 && raw[2] == 0xFE && raw[3] == 0xFF:
		return "", errors.New("UTF-32BE not supported")
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return decodeUTF16(raw[2:], binary.LittleEndian)
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return decodeUTF16(raw[2:], binary.BigEndian)
	}
	if !utf8.Valid(raw) {
		return "", errors.New("invalid UTF-8 and no recognized BOM")
	}
	return string(raw), nil
}

// decodeUTF16 decodes BOM-stripped UTF-16 bytes, rejecting odd lengths and
// unpaired surrogates.
func decodeUTF16(b []byte, order binary.ByteOrder) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("UTF-16 input has odd byte length %d", len(b)+2)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	// utf16.Decode maps unpaired surrogates to U+FFFD; detect them first so
	// broken input errors instead of decoding garbled.
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u < 0xDC00: // high surrogate needs a low partner
			if i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] >= 0xE000 {
				return "", fmt.Errorf("UTF-16 unpaired high surrogate at unit %d", i)
			}
			i++
		case u >= 0xDC00 && u < 0xE000:
			return "", fmt.Errorf("UTF-16 unpaired low surrogate at unit %d", i)
		}
	}
	return string(utf16.Decode(units)), nil
}
