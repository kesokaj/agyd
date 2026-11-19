package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// sanitizeTerminal removes ANSI/VT100 escape sequences (CSI, OSC, DCS/SOS/PM/APC,
// and short ESC sequences), control characters, and Unicode format/bidi runes so
// untrusted text from the server or agent cannot manipulate the user's terminal
// or inject tabwriter columns. Line breaks and tabs become single spaces.
func sanitizeTerminal(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipESC(s, i+1)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case 0x9b: // 8-bit CSI
			i = skipCSI(s, i+size)
			continue
		case 0x9d: // 8-bit OSC
			i = skipStringSeq(s, i+size, true)
			continue
		case 0x90, 0x98, 0x9e, 0x9f: // 8-bit DCS, SOS, PM, APC
			i = skipStringSeq(s, i+size, false)
			continue
		case '\r':
			b.WriteByte(' ')
			if i+1 < len(s) && s[i+1] == '\n' {
				i += 2
			} else {
				i += size
			}
			continue
		case '\n', '\t':
			b.WriteByte(' ')
			i += size
			continue
		}
		if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// skipESC advances past a 7-bit escape sequence starting immediately after ESC.
func skipESC(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		return skipCSI(s, i+1)
	case ']':
		return skipStringSeq(s, i+1, true)
	case 'P', 'X', '^', '_':
		return skipStringSeq(s, i+1, false)
	}
	// nF / Fp / Fe / Fs sequences: optional 0x20-0x2F intermediates + 0x30-0x7E final.
	j := i
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		j++
	}
	if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
		return j + 1
	}
	return i
}

// skipCSI advances past ECMA-48 CSI parameter/intermediate bytes and final byte.
func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		return i + 1
	}
	return i
}

// skipStringSeq advances past an OSC/DCS/SOS/PM/APC payload until ST (ESC \,
// U+009C), BEL (when allowBEL is true), or end of string.
func skipStringSeq(s string, i int, allowBEL bool) int {
	for i < len(s) {
		if allowBEL && s[i] == 0x07 {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0x9c {
			return i + size
		}
		i += size
	}
	return i
}
