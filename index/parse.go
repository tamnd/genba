package index

import (
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/doc"
)

// Parse turns what somebody typed into a [Query].
//
// The grammar is the small set of prefixes people already expect from a search
// box: app:, type:, in:, from:, owner:, updated:, and quoted phrases. Anything
// that is not an operator is text. An operator nobody recognises is text too,
// because a colon in a sentence is far more common than a typo in an operator,
// and treating "note: this broke" as a filter on a field called note finds
// nothing and explains nothing.
//
// Repeating an operator widens: app:slack app:gdrive searches both. Combining
// different operators narrows. That is the same rule the facet sidebar follows,
// which is deliberate, because ticking a box and typing the operator produce the
// same query and a person who learns one has learned the other.
func Parse(input string) Query {
	var (
		q    Query
		text []string
	)
	for _, tok := range tokens(input) {
		field, value, ok := strings.Cut(tok.text, ":")
		if !ok || tok.quoted || value == "" {
			text = append(text, tok.text)
			continue
		}
		value = strings.Trim(value, `"`)
		switch strings.ToLower(field) {
		case "app", "source":
			q.Sources = append(q.Sources, strings.ToLower(value))
		case "type", "kind":
			q.Kinds = append(q.Kinds, doc.Kind(strings.ToLower(value)))
		case "in", "container":
			q.Containers = append(q.Containers, value)
		case "from", "author", "by":
			q.Authors = append(q.Authors, value)
		case "owner":
			q.Owners = append(q.Owners, value)
		case "updated", "modified":
			if since, until, ok := parseWhen(value, time.Now()); ok {
				q.Since, q.Until = since, until
				continue
			}
			text = append(text, tok.text)
		case "sort":
			if strings.EqualFold(value, "recent") || strings.EqualFold(value, "recency") {
				q.Sort = ByRecent
				continue
			}
			text = append(text, tok.text)
		default:
			text = append(text, tok.text)
		}
	}
	q.Text = strings.Join(text, " ")
	return q
}

// token is one whitespace separated piece of the input, remembering whether it
// arrived inside quotes.
type token struct {
	text   string
	quoted bool
}

// tokens splits on whitespace, keeping quoted runs together. A quoted run is
// never read as an operator, so searching for the literal string "app: down"
// is possible and does not silently become a filter.
func tokens(input string) []token {
	var (
		out    []token
		cur    strings.Builder
		inQ    bool
		quoted bool
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, token{text: cur.String(), quoted: quoted})
			cur.Reset()
		}
		quoted = false
	}
	for _, r := range input {
		switch {
		case r == '"':
			inQ = !inQ
			quoted = true
		case !inQ && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseWhen reads the value of updated:.
//
// It accepts a relative window (today, week, month, year, 7d, 3w), an exact day
// (2026-08-19), a bound (>2026-01-01, <2026-01-01) and a range
// (2026-01-01..2026-03-31). Relative windows come first because they are what
// people type, and the exact forms exist because they are what a shared link
// needs in order to still mean the same thing next week.
func parseWhen(value string, now time.Time) (since, until time.Time, ok bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "today":
		return now.AddDate(0, 0, -1), time.Time{}, true
	case "week", "this-week":
		return now.AddDate(0, 0, -7), time.Time{}, true
	case "month", "this-month":
		return now.AddDate(0, -1, 0), time.Time{}, true
	case "quarter":
		return now.AddDate(0, -3, 0), time.Time{}, true
	case "year", "this-year":
		return now.AddDate(-1, 0, 0), time.Time{}, true
	}

	if lo, hi, found := strings.Cut(value, ".."); found {
		since, okLo := parseDay(lo)
		until, okHi := parseDay(hi)
		if okLo && okHi {
			return since, endOfDay(until), true
		}
		return time.Time{}, time.Time{}, false
	}
	if after, found := strings.CutPrefix(value, ">"); found {
		if t, okT := parseDay(after); okT {
			return t, time.Time{}, true
		}
		return time.Time{}, time.Time{}, false
	}
	if before, found := strings.CutPrefix(value, "<"); found {
		if t, okT := parseDay(before); okT {
			return time.Time{}, endOfDay(t), true
		}
		return time.Time{}, time.Time{}, false
	}
	if d, okD := parseSpan(value, now); okD {
		return d, time.Time{}, true
	}
	if t, okT := parseDay(value); okT {
		return t, endOfDay(t), true
	}
	return time.Time{}, time.Time{}, false
}

// parseSpan reads a shorthand duration such as 7d, 3w, 6m or 2y. Go's own
// duration syntax stops at hours, and nobody filters a search by hours.
func parseSpan(value string, now time.Time) (time.Time, bool) {
	if len(value) < 2 {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	switch value[len(value)-1] {
	case 'd':
		return now.AddDate(0, 0, -n), true
	case 'w':
		return now.AddDate(0, 0, -7*n), true
	case 'm':
		return now.AddDate(0, -n, 0), true
	case 'y':
		return now.AddDate(-n, 0, 0), true
	}
	return time.Time{}, false
}

func parseDay(value string) (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// endOfDay turns a day into the last instant of it, so that updated:2026-08-19
// includes documents changed that afternoon rather than only the ones changed
// exactly at midnight.
func endOfDay(t time.Time) time.Time { return t.AddDate(0, 0, 1).Add(-time.Nanosecond) }
