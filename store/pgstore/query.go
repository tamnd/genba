package pgstore

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// clause is a fragment of SQL together with the arguments it binds.
//
// Fragments are written with question marks and renumbered into $1, $2 on the
// way out. Postgres only takes the numbered form, and numbering by hand in a
// query assembled from optional pieces is a bug waiting for the day somebody
// adds a filter in the middle.
type clause struct {
	sql  []string
	args []any
}

func (c *clause) add(sql string, args ...any) {
	c.sql = append(c.sql, sql)
	c.args = append(c.args, args...)
}

// where renders the fragments, or a true constant when there are none, so the
// caller can always concatenate a WHERE onto its query.
func (c *clause) where() string {
	if len(c.sql) == 0 {
		return "true"
	}
	return strings.Join(c.sql, " AND ")
}

// rebind turns the question marks in a finished statement into $1, $2 and so
// on.
//
// It runs over the whole statement rather than over each fragment, because the
// numbering depends on what came before and a fragment does not know that. The
// statement text this produces is stable for a given shape of query, which is
// what lets pgx cache the prepared statement behind it.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 0
	for {
		i := strings.IndexByte(query, '?')
		if i < 0 {
			b.WriteString(query)
			return b.String()
		}
		n++
		b.WriteString(query[:i])
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
		query = query[i+1:]
	}
}

// visible is the permission rule, in SQL.
//
// It is acl.Permissions.Allows, in the same order, over the same strings: an
// unresolved descriptor denies, then a deny list denies, then the owner is
// allowed, then the mode decides. The principal's keys go in as bound text
// arrays and the whole decision happens while Postgres walks its own rows.
//
// Nothing calls this and then filters again afterwards. That is the point: a
// count, a facet or a snippet computed from these rows is computed from rows
// the asker may read, and there is no second place for the rule to be
// forgotten.
func visible(p *acl.Principal) *clause {
	users := nonEmpty(p.UserKeys())
	groups := nonEmpty(p.GroupKeys())

	const (
		matchUser  = `r.scope = 0 AND r.key = ANY(?::text[])`
		matchGroup = `r.scope = 1 AND r.key = ANY(?::text[])`
	)

	c := &clause{}
	c.add(`d.queryable`)
	c.add(`d.tenant = ?`, p.Tenant)
	c.add(`NOT EXISTS (
		SELECT 1 FROM document_ref r
		WHERE r.doc_id = d.id AND r.effect = 1
		  AND ((`+matchUser+`) OR (`+matchGroup+`))
	)`, users, groups)
	c.add(`(
		(d.owner_key <> '' AND d.owner_key = ANY(?::text[]))
		OR d.mode = ?
		OR (d.mode = ? AND EXISTS (
			SELECT 1 FROM document_ref r
			WHERE r.doc_id = d.id AND r.effect = 0
			  AND ((`+matchUser+`) OR (`+matchGroup+`))
		))
	)`, users, int16(acl.ModePublicToTenant), int16(acl.ModeACL), users, groups)
	return c
}

// filters is store.Request without the terms, in SQL.
//
// Every field is a membership test against a bound array, which keeps the
// statement text the same whatever the caller ticked. That matters for more
// than tidiness: pgx caches a prepared statement by its text, so a query whose
// shape does not change with the number of selected sources is prepared once
// instead of once per combination.
func filters(r store.Request, c *clause) {
	if len(r.Sources) > 0 {
		c.add(`d.source = ANY(?::text[])`, r.Sources)
	}
	if len(r.Kinds) > 0 {
		kinds := make([]string, len(r.Kinds))
		for i, k := range r.Kinds {
			kinds[i] = string(k)
		}
		c.add(`d.kind = ANY(?::text[])`, kinds)
	}
	if len(r.Containers) > 0 {
		c.add(`d.container_fold <> '' AND d.container_fold = ANY(?::text[])`, folded(r.Containers))
	}
	if len(r.Authors) > 0 {
		c.add(`d.author_keys && ?::text[]`, folded(r.Authors))
	}
	if len(r.Owners) > 0 {
		c.add(`d.owner_keys && ?::text[]`, folded(r.Owners))
	}
	if !r.Since.IsZero() {
		// A document whose date the source never gave us is not known to have
		// changed since anything, so it is out. The Go rule reaches the same
		// answer by comparing against the zero time.
		c.add(`d.modified_at IS NOT NULL AND d.modified_at >= ?`, r.Since.UnixNano())
	}
	if !r.Until.IsZero() {
		c.add(`(d.modified_at IS NULL OR d.modified_at <= ?)`, r.Until.UnixNano())
	}
}

// maxLexeme is the longest lexeme a tsvector will hold, less a little room.
//
// Postgres refuses a lexeme over 2046 bytes, and a document can perfectly well
// contain one: a base64 blob pasted into a wiki page tokenizes to a single term
// several kilobytes long. See [lexeme] for what happens to those.
const maxLexeme = 2000

// lexeme is the term as the full text index stores it.
//
// A term short enough to store is stored as it is. One that is not becomes a
// hash, and the query side hashes the same way, so the two still meet and the
// match set is the one store.Request.Matches describes. The prefix is a
// character the tokenizer cannot produce, which is what makes the hashed form
// unable to collide with a real term.
func lexeme(term string) string {
	if len(term) <= maxLexeme {
		return term
	}
	sum := sha256.Sum256([]byte(term))
	return "#" + hex.EncodeToString(sum[:16])
}

// tsquery renders the terms as a tsquery, or returns false when there are none
// and the full text index should be left out of the statement entirely.
//
// The terms are joined with OR because the match set is documents carrying at
// least one term. Narrowing that to all terms is a ranking decision and it
// belongs above this, where the score can prefer documents with more of them
// without hiding the rest.
//
// The result is bound as a parameter and cast, never concatenated into a
// statement, so a term full of punctuation is a value rather than syntax.
func tsquery(terms []string) (string, bool) {
	var b strings.Builder
	for _, t := range terms {
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		quote(&b, lexeme(t))
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// tsvector renders one document's terms as a tsvector literal.
//
// It is built here rather than by to_tsvector because the lexemes have to be
// exactly the terms doc.Tokenize produced. Handing Postgres the text and
// letting its parser have another go at it would put a second tokenizer in the
// middle of the one thing this driver cannot get wrong: a term the Go rule
// finds and the index does not is a document one deployment returns and another
// does not.
//
// The positions are synthesised. There are no real ones to record, because the
// analysis keeps occurrence counts rather than offsets, and a count is what
// ts_rank actually reads. So a term occurring three times in a body gets three
// consecutive positions weighted B, and the title's get A, which is what makes
// the candidate cut order by something better than how many distinct query
// terms a document happens to contain.
func tsvector(a doc.Analysis) string {
	terms := make([]string, 0, len(a.Terms))
	for term := range a.Terms {
		terms = append(terms, term)
	}
	// Sorted so that the same document produces the same literal every time,
	// which keeps a rewrite that changed nothing from looking like a change.
	slices.Sort(terms)

	var (
		b      strings.Builder
		budget = positionBudget
	)
	for _, term := range terms {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		quote(&b, lexeme(term))

		c := a.Terms[term]
		title := min(c.Title, maxPositions)
		body := min(c.Body, maxPositions-title)
		if title+body > budget {
			// A document big enough to run out of budget still matches on every
			// term it carries. What it loses is the frequency signal on the
			// terms at the end of it, which is a ranking detail, and what it
			// avoids is a tsvector Postgres refuses to store at all.
			continue
		}
		budget -= title + body
		pos := 0
		for range title {
			pos++
			b.WriteString(sep(pos))
			b.WriteString(strconv.Itoa(pos))
			b.WriteByte('A')
		}
		for range body {
			pos++
			b.WriteString(sep(pos))
			b.WriteString(strconv.Itoa(pos))
			b.WriteByte('B')
		}
	}
	return b.String()
}

// maxPositions is how many occurrences of one term are worth recording.
//
// Postgres keeps at most 256 positions per lexeme and drops the rest, so
// anything above this is bytes on the wire that the server throws away.
const maxPositions = 256

// positionBudget bounds the whole tsvector. A tsvector may not exceed a
// megabyte, and a position costs two bytes, so this is a large document's worth
// of margin under a limit that is a hard error rather than a warning.
const positionBudget = 100000

func sep(pos int) string {
	if pos == 1 {
		return ":"
	}
	return ","
}

// quote writes a lexeme in the form both tsvector and tsquery input accept:
// single quoted, with a quote doubled and a backslash doubled.
func quote(b *strings.Builder, s string) {
	b.WriteByte('\'')
	for i := range len(s) {
		switch s[i] {
		case '\'':
			b.WriteString("''")
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\'')
}

func folded(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, store.Fold(v))
		}
	}
	return out
}

// nonEmpty guarantees a bound array is an empty array rather than NULL, because
// x = ANY(NULL) is NULL rather than false and a principal with no groups would
// stop matching anything at all.
func nonEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
