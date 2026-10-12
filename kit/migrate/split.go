package migrate

import "strings"

// splitStatements splits body on top-level semicolons, never on one inside a single-quoted literal, a dollar-quoted
// block (tagged or not), a line comment or a block comment. Empty and comment-only statements are dropped, so a
// trailing semicolon or a closing comment does not become a blank Exec.
func splitStatements(body string) []string {
	var stmts []string
	var cur strings.Builder

	inSingleQuote := false
	inLineComment := false
	inBlockComment := false
	dollarTag := "" // non-empty while inside a $tag$ ... $tag$ block

	n := len(body)
	for i := 0; i < n; i++ {
		c := body[i]

		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			cur.WriteByte(c)
			if c == '*' && i+1 < n && body[i+1] == '/' {
				cur.WriteByte('/')
				i++
				inBlockComment = false
			}
			continue
		}

		if dollarTag != "" {
			cur.WriteByte(c)
			if c == '$' && strings.HasPrefix(body[i:], dollarTag) {
				cur.WriteString(dollarTag[1:])
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		if inSingleQuote {
			cur.WriteByte(c)
			if c == '\'' {
				// A doubled quote is an escaped quote, not the closing delimiter.
				if i+1 < n && body[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
				} else {
					inSingleQuote = false
				}
			}
			continue
		}

		switch {
		case c == '\'':
			inSingleQuote = true
			cur.WriteByte(c)
		case c == '-' && i+1 < n && body[i+1] == '-':
			inLineComment = true
			cur.WriteByte(c)
		case c == '/' && i+1 < n && body[i+1] == '*':
			inBlockComment = true
			cur.WriteByte(c)
		case c == '$':
			if tag, ok := dollarQuoteTag(body[i:]); ok {
				dollarTag = tag
				cur.WriteString(tag)
				i += len(tag) - 1
			} else {
				cur.WriteByte(c)
			}
		case c == ';':
			stmts = appendStatement(stmts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return appendStatement(stmts, cur.String())
}

// dollarQuoteTag reports whether s begins with a dollar-quote opener ($$ or $tag$) and returns it.
func dollarQuoteTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '$' {
			return s[:i+1], true
		}
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return "", false
}

func appendStatement(stmts []string, stmt string) []string {
	trimmed := strings.TrimSpace(stmt)
	if trimmed == "" || isCommentOnly(trimmed) {
		return stmts
	}
	return append(stmts, trimmed)
}

// isCommentOnly reports whether s holds only whitespace and comments.
func isCommentOnly(s string) bool {
	i, n := 0, len(s)
	for i < n {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r':
			i++
		case s[i] == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				i++
			}
		case s[i] == '/' && i+1 < n && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end == -1 {
				return true
			}
			i = i + 2 + end + 2
		default:
			return false
		}
	}
	return true
}
