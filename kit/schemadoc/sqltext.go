package schemadoc

import "strings"

// SQLite keeps each CREATE statement as written; what its pragmas leave out (an index expression's text, a partial
// index's predicate, a generated column's expression, CHECK constraints) is read from that text. The scanner below
// knows just enough SQL for it: parentheses, commas, string literals, quoted identifiers ("x", `x`, [x]) and
// comments, whose contents never count. Comments are dropped first: text put on one line must not end in one.

// skipQuoted is the index just past the literal, quoted identifier or comment starting at s[i], i when none does.
// An unterminated one runs to the end of s.
func skipQuoted(s string, i int) int {
	switch c := s[i]; {
	case c == '\'' || c == '"' || c == '`':
		for j := i + 1; j < len(s); j++ {
			if s[j] != c {
				continue
			}
			if j+1 < len(s) && s[j+1] == c { // a doubled quote is one quote character
				j++
				continue
			}
			return j + 1
		}
		return len(s)
	case c == '[':
		if k := strings.IndexByte(s[i:], ']'); k >= 0 {
			return i + k + 1
		}
		return len(s)
	case strings.HasPrefix(s[i:], "--"):
		if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
			return i + k + 1
		}
		return len(s)
	case strings.HasPrefix(s[i:], "/*"):
		if k := strings.Index(s[i+2:], "*/"); k >= 0 {
			return i + 2 + k + 2
		}
		return len(s)
	}
	return i
}

// stripComments is s with each comment replaced by a space.
func stripComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := skipQuoted(s, i)
		switch {
		case j == i:
			b.WriteByte(s[i])
			j = i + 1
		case s[i] == '-' || s[i] == '/':
			b.WriteByte(' ')
		default:
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// openParen is the index of the first '(' in s outside literals and comments, -1 when there is none.
func openParen(s string) int {
	for i := 0; i < len(s); {
		if j := skipQuoted(s, i); j != i {
			i = j
			continue
		}
		if s[i] == '(' {
			return i
		}
		i++
	}
	return -1
}

// parenList is the list in the parentheses opening at s[open]: its top-level elements, each on one line, and the
// index of the closing parenthesis, -1 when it never closes.
func parenList(s string, open int) ([]string, int) {
	var items []string
	depth, start := 0, open+1
	for i := open; i < len(s); {
		if j := skipQuoted(s, i); j != i {
			i = j
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return append(items, oneLine(s[start:i])), i
			}
		case ',':
			if depth == 1 {
				items = append(items, oneLine(s[start:i]))
				start = i + 1
			}
		}
		i++
	}
	return nil, -1
}

// firstList is the top-level elements of the first parenthesised list in a statement with its comments dropped, and
// what follows the list; ok false when there is none.
func firstList(create string) (items []string, rest string, ok bool) {
	s := stripComments(create)
	open := openParen(s)
	if open < 0 {
		return nil, "", false
	}
	items, end := parenList(s, open)
	if end < 0 {
		return nil, "", false
	}
	return items, s[end+1:], true
}

// indexKey is the n-th (0-based) key of a CREATE INDEX statement as written, without its collation and order;
// "(expression)" when the statement does not parse that far.
func indexKey(create string, n int) string {
	if keys, _, ok := firstList(create); ok && n < len(keys) {
		if k := keyExpr(keys[n]); k != "" {
			return k
		}
	}
	return "(expression)"
}

// keyExpr is an index key (on one line) without its trailing COLLATE <name> and ASC/DESC: the index_xinfo pragma
// reports both, and they are appended from there for every key alike. Only a top-level tail counts: lower(a COLLATE
// nocase) and a || ' DESC' are the expression.
func keyExpr(k string) string {
	for i := 0; i < len(k); {
		if j := skipQuoted(k, i); j != i {
			i = j
			continue
		}
		switch {
		case k[i] == '(':
			_, end := parenList(k, i)
			if end < 0 {
				return k
			}
			i = end + 1
			continue
		case (i == 0 || !identByte(k[i-1])) && keyTail(k[i:]):
			return strings.TrimSpace(k[:i])
		}
		i++
	}
	return k
}

// keyTail reports whether s is nothing but a key's [COLLATE <name>] [ASC|DESC], one of them at least.
func keyTail(s string) bool {
	rest := s
	if keywordAt(rest, 0, "COLLATE") {
		rest = strings.TrimLeft(rest[len("COLLATE"):], " ")
		n := 0
		if rest != "" {
			if n = skipQuoted(rest, 0); n == 0 {
				if n = strings.IndexByte(rest, ' '); n < 0 {
					n = len(rest)
				}
			}
		}
		if n == 0 {
			return false
		}
		rest = strings.TrimLeft(rest[n:], " ")
	}
	for _, kw := range []string{"ASC", "DESC"} {
		if keywordAt(rest, 0, kw) {
			rest = rest[len(kw):]
		}
	}
	return rest != s && strings.TrimSpace(rest) == ""
}

// indexWhere is a CREATE INDEX statement's partial index predicate on one line, empty when it has none.
func indexWhere(create string) string {
	_, rest, ok := firstList(create)
	rest = strings.TrimSpace(rest)
	if !ok || !keywordAt(rest, 0, "WHERE") {
		return ""
	}
	return oneLine(strings.TrimSuffix(strings.TrimSpace(rest[len("WHERE"):]), ";"))
}

// sqliteGenerated is the expression of generated column col in a CREATE TABLE statement, on one line; empty when
// the statement does not show it.
func sqliteGenerated(create, col string) string {
	defs, _, _ := firstList(create)
	for _, def := range defs {
		if strings.EqualFold(leadingName(def), col) {
			// The first list after AS: a type's width or a CHECK comes before it, inside parentheses of its own.
			if lists := keywordLists(def, "AS"); len(lists) > 0 {
				l := lists[0].list
				return strings.TrimSpace(l[1 : len(l)-1])
			}
		}
	}
	return ""
}

// leadingName is the name a column definition starts with, unquoted.
func leadingName(def string) string {
	if def == "" {
		return ""
	}
	if j := skipQuoted(def, 0); j > 0 {
		q, end := def[0], def[0]
		if q == '[' {
			end = ']'
		}
		if j < 2 || def[j-1] != end { // unterminated: the rest is the name
			return def[1:]
		}
		inner := def[1 : j-1]
		if q == '[' {
			return inner
		}
		return strings.ReplaceAll(inner, string(q)+string(q), string(q))
	}
	name, _, _ := strings.Cut(def, " ")
	return name
}

// sqliteChecks is the CHECK constraints of a CREATE TABLE statement, column and table level, in the order written,
// each as CHECK (<expression>) on one line, CONSTRAINT <name> CHECK (…) when named: SQLite's error names it, and a
// migration rebuilding the table looks for it.
func sqliteChecks(create string) []string {
	defs, _, _ := firstList(create)
	var out []string
	for _, def := range defs {
		out = append(out, checkLists(def)...)
	}
	return out
}

// checkLists is def's CHECK constraints at its top level (outside parentheses, literals and quoted names), a name
// given by CONSTRAINT <name> right before kept as written.
func checkLists(def string) []string {
	var out []string
	for _, l := range keywordLists(def, "CHECK") {
		c := "CHECK " + l.list
		if name := constraintName(def[:l.at]); name != "" {
			c = "CONSTRAINT " + name + " " + c
		}
		out = append(out, c)
	}
	return out
}

// constraintName is the name of the CONSTRAINT <name> before ends with, as written (quotes kept); "" when before
// ends otherwise.
func constraintName(before string) string {
	s := strings.TrimRight(before, " ")
	start, end := len(s), len(s)
	if q := strings.IndexByte(`"'`+"`]", lastByte(s)); q >= 0 {
		open := (`"'` + "`[")[q]
		// The opening quote, past doubled ones ("a""b"); brackets have none.
		for start = end - 2; start >= 0; start-- {
			if s[start] != open {
				continue
			}
			if open != '[' && start > 0 && s[start-1] == open {
				start--
				continue
			}
			break
		}
		if start < 0 {
			return ""
		}
	} else {
		for start > 0 && identByte(s[start-1]) {
			start--
		}
	}
	rest := strings.TrimRight(s[:start], " ")
	const kw = "CONSTRAINT"
	if start == end || len(rest) < len(kw) || !keywordAt(rest, len(rest)-len(kw), kw) {
		return ""
	}
	return s[start:end]
}

// lastByte is s's last byte, 0 when empty.
func lastByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[len(s)-1]
}

// keywordLists is the parenthesised lists, parentheses included, that follow keyword kw at the top level of def (a
// definition on one line: outside parentheses, literals and quoted names).
func keywordLists(def, kw string) []keywordList {
	var out []keywordList
	for i := 0; i < len(def); {
		if j := skipQuoted(def, i); j != i {
			i = j
			continue
		}
		open := -1
		switch {
		case def[i] == '(':
			open = i
		case keywordAt(def, i, kw):
			p := i + len(kw)
			for p < len(def) && def[p] == ' ' {
				p++
			}
			if p < len(def) && def[p] == '(' {
				open = p
			}
		}
		if open < 0 {
			i++
			continue
		}
		_, end := parenList(def, open)
		if end < 0 {
			return out
		}
		if open != i {
			out = append(out, keywordList{at: i, list: def[open : end+1]})
		}
		i = end + 1
	}
	return out
}

// keywordList is a parenthesised list after a keyword, parentheses included; at is the keyword's offset.
type keywordList struct {
	at   int
	list string
}

// keywordAt reports whether the keyword kw, any case, stands at s[i] as a word of its own.
func keywordAt(s string, i int, kw string) bool {
	if i+len(kw) > len(s) || !strings.EqualFold(s[i:i+len(kw)], kw) {
		return false
	}
	return (i == 0 || !identByte(s[i-1])) && (i+len(kw) == len(s) || !identByte(s[i+len(kw)]))
}
