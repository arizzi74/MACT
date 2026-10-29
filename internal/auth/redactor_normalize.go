package auth

import (
	"strings"
	"unicode/utf8"
)

// Look through common invisible separators when matching, while preserving the
// original bytes everywhere outside a redacted span (including emoji joiners).
// This deliberately does not normalize arbitrary Unicode or decode encodings.
func redactInvisibleSeparators(text string, r *Redactor) string {
	if !strings.ContainsAny(text, "\u200b\u200c\u200d\u2060\ufeff") {
		return text
	}
	var normalized strings.Builder
	positions := make([]int, 0, len(text)+1)
	for offset, char := range text {
		switch char {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			continue
		}
		_, width := utf8.DecodeRuneInString(text[offset:])
		encoded := text[offset : offset+width]
		normalized.WriteString(encoded)
		for index := range len(encoded) {
			positions = append(positions, offset+index)
		}
	}
	positions = append(positions, len(text))
	// Collect matches against the same view; mark the original byte spans so
	// overlapping rules cannot expose part of a previously matched credential.
	var spans [][2]int
	for _, expression := range r.expressions {
		for _, match := range expression.FindAllStringIndex(normalized.String(), -1) {
			if match[0] != match[1] {
				spans = append(spans, [2]int{positions[match[0]], positions[match[1]]})
			}
		}
	}
	if len(spans) == 0 {
		return text
	}
	// Regexes are few; a byte mask avoids sorting and stays linear in text size
	// plus matched bytes, even for many overlapping user-defined expressions.
	hidden := make([]bool, len(text))
	for _, span := range spans {
		for index := span[0]; index < span[1]; index++ {
			hidden[index] = true
		}
	}
	var result strings.Builder
	for offset := 0; offset < len(text); {
		if !hidden[offset] {
			result.WriteByte(text[offset])
			offset++
			continue
		}
		result.WriteString(r.replacement)
		for offset < len(text) && hidden[offset] {
			offset++
		}
	}
	return result.String()
}
