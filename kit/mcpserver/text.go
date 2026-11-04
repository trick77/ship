package mcpserver

import (
	"strings"
	"unicode/utf8"
)

const truncatedNote = "\n[output truncated]"

// OneLine collapses s's whitespace to single spaces and cuts it to at most n runes, the last one an ellipsis when
// it was cut: a description fit for one line of tool text. n below 1 gives "".
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// CapText trims s to at most max bytes on a line boundary when it can, a rune boundary otherwise, and says so: the
// cut the handler makes at Server.MaxTextBytes, for a tool that cuts part of its text itself. A max with no room
// for the note gives "".
func CapText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncatedNote)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl > cut/2 {
		cut = nl
	}
	return s[:cut] + truncatedNote
}
