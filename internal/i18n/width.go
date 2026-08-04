package i18n

import "strings"

// Display-width padding.
//
// Go's %-20s pads by bytes. A Chinese label is 3 bytes per character but
// occupies 2 terminal columns, so byte padding leaves Chinese output ragged
// while English lines up — which is exactly what happened the first time the
// status block was translated. Column counts are what a reader sees, so pad by
// those.

// RuneWidth returns the number of terminal columns a rune occupies: 2 for the
// East Asian wide and fullwidth ranges, 0 for combining marks, 1 otherwise.
//
// This is the common subset of the Unicode East Asian Width property, not the
// whole table — enough for the CJK text and ASCII this project renders, and it
// avoids a dependency for one function.
func RuneWidth(r rune) int {
	switch {
	case r < 0x20:
		return 0 // control
	case r < 0x1100:
		return 1 // ASCII and Latin-1 fast path
	case r >= 0x0300 && r <= 0x036F:
		return 0 // combining diacriticals
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi, punctuation
		r >= 0x3041 && r <= 0x33FF, // Hiragana, Katakana, CJK compat
		r >= 0x3400 && r <= 0x4DBF, // CJK ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compat ideographs
		r >= 0xFE30 && r <= 0xFE6F, // CJK compat forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD: // CJK ext B and beyond
		return 2
	}
	return 1
}

// Width returns the number of terminal columns s occupies.
func Width(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

// PadRight pads s with spaces to the given column width. Strings already at or
// over the width are returned unchanged — truncating a label to save alignment
// would cost more than the ragged edge.
func PadRight(s string, width int) string {
	if pad := width - Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
