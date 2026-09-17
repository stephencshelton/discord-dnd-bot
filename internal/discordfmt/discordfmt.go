// Package discordfmt holds the Discord message-surface limits and the helpers
// for fitting generated text into them.
//
// It exists because the gateway and the worker both render AI output into
// Discord, and both got it subtly wrong in the same way: chopping text at a
// fixed rune count mid-word (or mid-Markdown-line), which reads exactly like the
// model failed. Centralizing the limits and the fitting rules keeps every
// surface consistent and the intent documented in one place.
package discordfmt

import "strings"

// Discord's hard content limits.
const (
	// MessageLimit is the maximum characters in a message body.
	MessageLimit = 2000
	// EmbedDescriptionLimit is the maximum characters in an embed description.
	EmbedDescriptionLimit = 4096
	// EmbedFieldValueLimit is the maximum characters in an embed FIELD value.
	// Much smaller than EmbedDescriptionLimit — mixing the two up is an easy way
	// to get a 50035 rejection, since a field value that would be a fine
	// description is four times over the field cap.
	EmbedFieldValueLimit = 1024
	// EmbedFieldNameLimit is the maximum characters in an embed field name.
	EmbedFieldNameLimit = 256
	// EmbedTotalLimit is the maximum COMBINED characters across an embed's title,
	// description, footer, author and all field names/values. Individually-valid
	// fields can still breach this in aggregate, so a multi-result embed must be
	// budgeted as a whole, not just per field.
	EmbedTotalLimit = 6000
	// MaxEmbedFields is the maximum number of fields in one embed.
	MaxEmbedFields = 25

	// ChunkLimit is the message size used when splitting long content across
	// several messages. It sits below MessageLimit to leave room for the newline
	// joins and any prefix the caller adds.
	ChunkLimit = 1900
)

// ellipsis marks text that was cut short.
const ellipsis = "…"

// Truncate fits s into max characters, cutting at a WORD boundary and marking
// the cut with an ellipsis. Cutting mid-word is what made a trimmed reply look
// like a broken generation ("…the Spore Queen's plan to weaponize souls (and why
// Irovalin's soul \"wasn't"), so the boundary search matters more than squeezing
// in the last few characters. It falls back to a hard cut only when there's no
// whitespace to break on (e.g. one enormous token).
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	// Reserve one rune for the ellipsis.
	cut := string(r[:max-1])
	// Prefer the last sentence end, then the last whitespace, but don't rewind so
	// far that we throw away most of the allowance.
	minKeep := (max - 1) / 2
	if i := lastSentenceEnd(cut); i > minKeep {
		return strings.TrimRight(cut[:i], " \t\n") + ellipsis
	}
	if i := strings.LastIndexAny(cut, " \t\n"); i > minKeep {
		return strings.TrimRight(cut[:i], " \t\n") + ellipsis
	}
	return cut + ellipsis
}

// lastSentenceEnd returns the index just past the last sentence-ending
// punctuation in s, or -1.
func lastSentenceEnd(s string) int {
	best := -1
	for _, sep := range []string{". ", "! ", "? ", ".\n", "!\n", "?\n"} {
		if i := strings.LastIndex(s, sep); i+1 > best {
			best = i + 1
		}
	}
	// A trailing terminator with nothing after it also ends a sentence.
	if best < 0 && len(s) > 0 && strings.ContainsRune(".!?", rune(s[len(s)-1])) {
		return len(s)
	}
	return best
}

// ChunkMarkdown splits Markdown into message-sized pieces WITHOUT cutting a line
// in half, so each piece still renders as valid Markdown — a heading or bullet
// split mid-line loses its formatting and reads as garbage. Lines are packed
// greedily; a single line longer than size is hard-split as a last resort.
//
// Posting long content as several messages (rather than one truncated message)
// also means Discord soft-wraps it to the reader's width.
func ChunkMarkdown(s string, size int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if size <= 0 {
		return []string{s}
	}

	var out []string
	var cur strings.Builder
	curRunes := 0
	flush := func() {
		if chunk := strings.TrimRight(cur.String(), "\n"); strings.TrimSpace(chunk) != "" {
			out = append(out, chunk)
		}
		cur.Reset()
		curRunes = 0
	}
	for _, line := range strings.Split(s, "\n") {
		n := len([]rune(line))
		// A line longer than a whole chunk has to be broken mid-line. Break it on
		// WORD boundaries: a long prose paragraph (a session-notes section, or a
		// description that approved proposals kept appending to) is one such line,
		// and slicing it at an exact rune count splits a word across two messages
		// — which reads as corrupted text rather than continued text.
		if n > size {
			flush()
			out = append(out, SplitWords(line, size)...)
			continue
		}
		// +1 for the newline that will join this line to the current chunk.
		if curRunes > 0 && curRunes+1+n > size {
			flush()
		}
		if curRunes > 0 {
			cur.WriteByte('\n')
			curRunes++
		}
		cur.WriteString(line)
		curRunes += n
	}
	flush()
	return out
}

// SplitWords breaks s into pieces of at most size runes, splitting at whitespace
// so words stay intact. A single token longer than size (a URL, a run of
// punctuation) is hard-split as a last resort, since there is no better option.
//
// Unlike Truncate this DISCARDS NOTHING — it is for content that must be shown in
// full across several messages.
// Every index below is a RUNE index into r. Mixing the two kinds of index is
// what broke this before: the break position came from strings.LastIndexAny (a
// BYTE offset) and was then used to slice a []rune. On any multibyte input — an
// em-dash or a curly quote, which AI prose is full of — the byte offset runs
// ahead of the rune count, so the slice read past the end of the window into the
// rune slice's spare capacity. Go permits that (s[:n] is legal up to cap), so it
// did not panic reliably; it emitted NUL runes, dropped input, and returned
// pieces LONGER than size — which Discord then rejected with 50035, the exact
// failure this package exists to prevent.
func SplitWords(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	r := []rune(s)
	if len(r) <= size {
		return []string{s}
	}

	var out []string
	for len(r) > size {
		// Break at the last whitespace in the window. Refuse to rewind past the
		// halfway mark, or a line of mostly-unbreakable text would produce lots of
		// tiny pieces.
		cut := -1
		for i := size - 1; i >= 0; i-- {
			if r[i] == ' ' || r[i] == '\t' {
				cut = i
				break
			}
		}
		if cut < size/2 {
			cut = -1
		}
		if cut < 0 {
			// No usable break: hard-split this piece.
			out = append(out, string(r[:size]))
			r = r[size:]
			continue
		}
		out = append(out, strings.TrimRight(string(r[:cut]), " \t"))
		// Skip the whitespace we broke on.
		skip := cut
		for skip < len(r) && (r[skip] == ' ' || r[skip] == '\t') {
			skip++
		}
		r = r[skip:]
	}
	if rest := string(r); strings.TrimSpace(rest) != "" {
		out = append(out, rest)
	}
	return out
}

// HardSplit slices s into pieces of at most size runes, ignoring word and line
// boundaries. Only for content with no break opportunity.
func HardSplit(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	var out []string
	r := []rune(s)
	for len(r) > size {
		out = append(out, string(r[:size]))
		r = r[size:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}
