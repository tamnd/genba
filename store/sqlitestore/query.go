package sqlitestore

import (
	"encoding/json"
	"strings"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// clause is a fragment of SQL together with the arguments it binds.
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
		return "1"
	}
	return strings.Join(c.sql, " AND ")
}

// visible is the permission rule, in SQL.
//
// It is acl.Permissions.Allows, in the same order, over the same strings: an
// unresolved descriptor denies, then a deny list denies, then the owner is
// allowed, then the mode decides. The principal's keys go in as bound JSON
// arrays and json_each turns them into a set the query can test membership
// against, so the whole decision happens while SQLite walks its own rows.
//
// Nothing calls this and then filters again afterwards. That is the point: a
// count, a facet or a snippet computed from these rows is computed from rows
// the asker may read, and there is no second place for the rule to be forgotten.
func visible(p *acl.Principal) *clause {
	users, _ := json.Marshal(nonEmpty(p.UserKeys()))
	groups, _ := json.Marshal(nonEmpty(p.GroupKeys()))

	// The two arrays are bound several times over, which is why they are built
	// once here rather than at each use.
	const (
		matchUser  = `r.scope = 0 AND r.key IN (SELECT value FROM json_each(?))`
		matchGroup = `r.scope = 1 AND r.key IN (SELECT value FROM json_each(?))`
	)

	c := &clause{}
	c.add(`d.queryable = 1`)
	c.add(`d.tenant = ?`, p.Tenant)
	c.add(`NOT EXISTS (
		SELECT 1 FROM document_ref r
		WHERE r.doc_id = d.id AND r.effect = 1
		  AND ((`+matchUser+`) OR (`+matchGroup+`))
	)`, string(users), string(groups))
	c.add(`(
		(d.owner_key <> '' AND d.owner_key IN (SELECT value FROM json_each(?)))
		OR d.mode = ?
		OR (d.mode = ? AND EXISTS (
			SELECT 1 FROM document_ref r
			WHERE r.doc_id = d.id AND r.effect = 0
			  AND ((`+matchUser+`) OR (`+matchGroup+`))
		))
	)`, string(users), int(acl.ModePublicToTenant), int(acl.ModeACL), string(users), string(groups))
	return c
}

// reachable is the same rule as [visible], written for the one query that has
// to apply it to every row rather than to a candidate set.
//
// Every other read path here reaches [visible] with a match set already in
// hand, so the correlated subqueries in it run over the few hundred documents a
// query produced. Counting what somebody can reach has no match set, and the
// same clause over a whole corpus re-reads the principal's keys once per
// document, which on twenty thousand documents is most of a tenth of a second
// for an answer of six numbers.
//
// So the keys are read once into a set and joined against the reference table
// once, and what comes out is the two document sets the rule asks about: the
// documents a deny names and the documents an allow names. The order after that
// is the order [visible] applies and the order acl.Permissions.Allows applies:
// unresolved is already excluded by queryable, then a deny denies, then the
// owner is allowed, then the mode decides.
//
// It is a second expression of the rule and that is the thing to be careful
// about, so it is held to the first by storetest.RunReachable, which walks the
// order clause by clause against every driver.
func reachable(p *acl.Principal) (string, []any) {
	users, _ := json.Marshal(nonEmpty(p.UserKeys()))
	groups, _ := json.Marshal(nonEmpty(p.GroupKeys()))

	// The two halves are separate SELECTs rather than one with an OR because an
	// OR over two columns cannot use document_ref_key and a scan of the whole
	// reference table is what this is avoiding. MATERIALIZED is not decoration
	// either: without it SQLite is free to inline the whole thing back into the
	// places it is used, which is the shape being replaced.
	const q = `
		WITH named(doc_id, effect) AS MATERIALIZED (
			SELECT doc_id, effect FROM document_ref
			WHERE scope = 0 AND key IN (SELECT value FROM json_each(?))
			UNION
			SELECT doc_id, effect FROM document_ref
			WHERE scope = 1 AND key IN (SELECT value FROM json_each(?))
		)
		SELECT d.source, count(*) FROM document d
		WHERE d.queryable = 1
		  AND d.tenant = ?
		  AND d.id NOT IN (SELECT doc_id FROM named WHERE effect = 1)
		  AND (
		    (d.owner_key <> '' AND d.owner_key IN (SELECT value FROM json_each(?)))
		    OR d.mode = ?
		    OR (d.mode = ? AND d.id IN (SELECT doc_id FROM named WHERE effect = 0))
		  )
		GROUP BY d.source`

	return q, []any{
		string(users), string(groups),
		p.Tenant,
		string(users),
		int(acl.ModePublicToTenant),
		int(acl.ModeACL),
	}
}

// filters is store.Request without the terms, in SQL.
//
// Each field is a membership test against a bound JSON array, which keeps the
// statement text the same whatever the caller ticked. That matters for more
// than tidiness: SQLite caches a prepared statement by its text, so a query
// whose shape does not change with the number of selected sources is prepared
// once instead of once per combination.
func filters(r store.Request, c *clause) {
	if len(r.Sources) > 0 {
		c.add(`d.source IN (SELECT value FROM json_each(?))`, jsonList(r.Sources))
	}
	if len(r.Kinds) > 0 {
		kinds := make([]string, len(r.Kinds))
		for i, k := range r.Kinds {
			kinds[i] = string(k)
		}
		c.add(`d.kind IN (SELECT value FROM json_each(?))`, jsonList(kinds))
	}
	if len(r.Containers) > 0 {
		c.add(`d.container_fold <> '' AND d.container_fold IN (SELECT value FROM json_each(?))`, jsonFolded(r.Containers))
	}
	if len(r.Authors) > 0 {
		c.add(`EXISTS (
			SELECT 1 FROM json_each(d.author_keys) k
			WHERE k.value IN (SELECT value FROM json_each(?))
		)`, jsonFolded(r.Authors))
	}
	if len(r.Owners) > 0 {
		c.add(`EXISTS (
			SELECT 1 FROM json_each(d.owner_keys) k
			WHERE k.value IN (SELECT value FROM json_each(?))
		)`, jsonFolded(r.Owners))
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

// match renders the terms as an FTS5 query, or returns false when there are
// none and the full text index should be left out of the statement entirely.
//
// The terms are joined with OR because the match set is documents carrying at
// least one term. Narrowing that to all terms is a ranking decision and it
// belongs above this, where the score can prefer documents with more of them
// without hiding the rest.
func match(terms []string) (string, bool) {
	if len(terms) == 0 {
		return "", false
	}
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		if t == "" {
			continue
		}
		// Quoting makes the term a literal, so a query for "or" or for "near"
		// searches for the word rather than invoking the FTS5 grammar.
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return "", false
	}
	return strings.Join(quoted, " OR "), true
}

func jsonList(values []string) string {
	b, _ := json.Marshal(values)
	return string(b)
}

func jsonInts(values []int64) string {
	if values == nil {
		values = []int64{}
	}
	b, _ := json.Marshal(values)
	return string(b)
}

func jsonFolded(values []string) string {
	folded := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			folded = append(folded, store.Fold(v))
		}
	}
	return jsonList(folded)
}

// nonEmpty guarantees json.Marshal produces an array rather than null, which
// json_each rejects.
func nonEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
