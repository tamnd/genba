package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// lockNamespace keeps this driver's advisory locks out of the way of whatever
// else is using them in a database genba shares with an application:
// 0x67656e62 is "genb" in ASCII. writeLock is the one key in it.
const (
	lockNamespace = 0x67656e62
	writeLock     = 1
)

// prior is what the stored version of a document contributed, read before it is
// overwritten or removed.
type prior struct {
	tenant      string
	titleTokens int64
	bodyTokens  int64
	queryable   bool
}

// posting is one row of the posting table, on its way to a COPY.
type posting struct {
	docID   string
	term    string
	titleTF int
	bodyTF  int
}

// write inserts or replaces a batch of documents.
//
// It is written as a handful of statements over the whole batch rather than a
// handful per document, because a statement against Postgres is a round trip
// and a round trip is most of the cost. Five hundred documents cost four
// exchanges with the server: read what is there, replace it, copy the postings
// in, fold the batch into the statistics.
func write(ctx context.Context, tx pgx.Tx, docs []doc.Document) error {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	priors, err := readPriors(ctx, tx, ids)
	if err != nil {
		return err
	}

	b := &pgx.Batch{}
	retire(b, priors, ids)
	// The rows a rewrite replaces rather than updates. Both are written as a
	// whole set per document, so the old set goes first: a document whose ACL
	// lost a group would otherwise keep the group, and one that stopped being an
	// image would keep the image.
	b.Queue(`DELETE FROM document_ref WHERE doc_id = ANY($1::text[])`, ids)
	b.Queue(`DELETE FROM document_content WHERE doc_id = ANY($1::text[])`, ids)

	rows := make([]posting, 0, len(docs)*64)
	written := map[string][]string{}
	tokens := map[string][2]int64{}
	for _, d := range docs {
		a := d.Analyze()
		if err := replace(b, d, a); err != nil {
			return err
		}
		if !d.Queryable() {
			continue
		}
		rows = appendPostings(rows, d.ID, a)
		written[d.Tenant] = append(written[d.Tenant], d.ID)
		t := tokens[d.Tenant]
		tokens[d.Tenant] = [2]int64{t[0] + int64(a.TitleTokens), t[1] + int64(a.BodyTokens)}
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}

	if len(rows) > 0 {
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"posting"},
			[]string{"doc_id", "term", "title_tf", "body_tf"},
			pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
				r := rows[i]
				return []any{r.docID, r.term, r.titleTF, r.bodyTF}, nil
			}))
		if err != nil {
			return fmt.Errorf("copy postings: %w", err)
		}
	}

	// After the postings are in, because the term counts are read back out of
	// them. Deriving them from the documents instead would be re-analysing text
	// that was analysed a moment ago, and the two would disagree the day the
	// analyzer changed.
	b = &pgx.Batch{}
	for _, tenant := range sortedKeys(written) {
		b.Queue(`
			INSERT INTO term_stat (tenant, term, documents)
			SELECT $1, term, count(*) FROM posting WHERE doc_id = ANY($2::text[]) GROUP BY term
			ON CONFLICT (tenant, term) DO UPDATE SET documents = term_stat.documents + excluded.documents`,
			tenant, written[tenant])
		t := tokens[tenant]
		b.Queue(`
			INSERT INTO corpus (tenant, documents, title_tokens, body_tokens) VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant) DO UPDATE SET
				documents    = corpus.documents + excluded.documents,
				title_tokens = corpus.title_tokens + excluded.title_tokens,
				body_tokens  = corpus.body_tokens + excluded.body_tokens`,
			tenant, int64(len(written[tenant])), t[0], t[1])
	}
	return tx.SendBatch(ctx, b).Close()
}

// remove deletes documents by id and reports which of them were there, by
// tenant.
//
// The tenant is read rather than taken from the caller because the caller does
// not have it: a deletion sweep has ids and nothing else, and a change reported
// with no tenant on it is a change a subscriber cannot act on.
func remove(ctx context.Context, tx pgx.Tx, ids []string) (map[string][]string, error) {
	priors, err := readPriors(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	if len(priors) == 0 {
		// Deleting ids that are not there is not an error, which is what makes
		// a sweep safe to run twice.
		return nil, nil
	}

	found := make([]string, 0, len(priors))
	removed := map[string][]string{}
	for _, id := range ids {
		p, ok := priors[id]
		if !ok {
			continue
		}
		found = append(found, id)
		removed[p.tenant] = append(removed[p.tenant], id)
	}

	b := &pgx.Batch{}
	retire(b, priors, found)
	// The row, and everything that references it. The foreign keys cascade, so
	// the postings, references, data and content go with it.
	b.Queue(`DELETE FROM document WHERE id = ANY($1::text[])`, found)
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return nil, err
	}
	return removed, nil
}

// lockWrites takes the write lock for the rest of the transaction.
//
// It is a lock on writing rather than a lock per tenant or per document, and
// that is a real limit worth naming: two servers ingesting different tenants
// into the same database take turns. The reason is the corpus and term
// statistics. They are maintained rather than derived, which is what turns "how
// many documents carry this term" from an aggregate over a posting list into a
// primary key hit, and maintaining them means reading what a document
// contributed before overwriting it. That read and the write that follows it
// are separate statements, and two transactions interleaving them leave the
// counters wrong by however many writes raced.
//
// The SQLite driver takes the same decision with a mutex, for the same reason.
// This is that mutex somewhere every server in a deployment can see it. Reads
// never take it, so a deployment ingesting flat out still serves queries at
// full speed, and within one tenant the ingestion pipeline is a single writer
// sending batches of hundreds, so the lock is held for four bulk statements and
// contended by nobody. Making it finer grained means keying the counters per
// tenant and locking per tenant, which is a change to the schema rather than to
// this function, and it is not worth making until somebody is actually
// ingesting two tenants at once into one Postgres.
func lockWrites(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, int32(lockNamespace), int32(writeLock)); err != nil {
		return fmt.Errorf("take the write lock: %w", err)
	}
	return nil
}

// readPriors reads what the stored versions of these documents contributed.
func readPriors(ctx context.Context, tx pgx.Tx, ids []string) (map[string]prior, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant, title_tokens, body_tokens, queryable
		FROM document WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the stored documents: %w", err)
	}
	defer rows.Close()

	out := make(map[string]prior, len(ids))
	for rows.Next() {
		var (
			id string
			p  prior
		)
		if err := rows.Scan(&id, &p.tenant, &p.titleTokens, &p.bodyTokens, &p.queryable); err != nil {
			return nil, fmt.Errorf("read the stored documents: %w", err)
		}
		out[id] = p
	}
	return out, rows.Err()
}

// retire takes what the stored versions contributed back out of the statistics,
// and drops the rows a rewrite would otherwise leave behind.
//
// It runs before anything is overwritten, because the numbers it reads are the
// ones about to be replaced. A quarantined document was never counted, so
// taking it out again would count it backwards, and it is left alone here.
func retire(b *pgx.Batch, priors map[string]prior, ids []string) {
	byTenant := map[string][]string{}
	tokens := map[string][2]int64{}
	for _, id := range ids {
		p, ok := priors[id]
		if !ok || !p.queryable {
			continue
		}
		byTenant[p.tenant] = append(byTenant[p.tenant], id)
		t := tokens[p.tenant]
		tokens[p.tenant] = [2]int64{t[0] + p.titleTokens, t[1] + p.bodyTokens}
	}

	for _, tenant := range sortedKeys(byTenant) {
		// The set of terms comes from the postings that are still in place, so
		// it is exactly the set that was counted when the document was written.
		b.Queue(`
			UPDATE term_stat t SET documents = t.documents - c.n
			FROM (SELECT term, count(*) AS n FROM posting WHERE doc_id = ANY($1::text[]) GROUP BY term) c
			WHERE t.tenant = $2 AND t.term = c.term`, byTenant[tenant], tenant)
		t := tokens[tenant]
		b.Queue(`
			UPDATE corpus SET
				documents    = documents - $1,
				title_tokens = title_tokens - $2,
				body_tokens  = body_tokens - $3
			WHERE tenant = $4`, int64(len(byTenant[tenant])), t[0], t[1], tenant)
	}

	// The postings go whether the document was queryable or not, because a
	// document that has just become quarantined has to stop being findable.
	b.Queue(`DELETE FROM posting WHERE doc_id = ANY($1::text[])`, ids)
}

// replace queues the statements that write one document.
func replace(b *pgx.Batch, d doc.Document, a doc.Analysis) error {
	// The content comes off the document before it is encoded, so the data
	// column stays the size of the text and Get cannot hand image bytes back to
	// a query path that has no use for them.
	content := d.Content
	d.Content = nil

	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode %s: %w", d.ID, err)
	}

	var ownerKey string
	if d.Permissions.Owner.Value != "" {
		ownerKey = d.Permissions.Owner.UserKey()
	}
	// A quarantined document is not indexed at all. The visibility predicate
	// already excludes it, and leaving it out of the index means a mistake in
	// that predicate cannot turn into a search result either.
	terms := ""
	if d.Queryable() {
		terms = tsvector(a)
	}

	b.Queue(`
		INSERT INTO document (
			id, tenant, source, kind, container_fold, author_keys, owner_keys,
			modified_at, mode, owner_key, queryable, source_update,
			title_tokens, body_tokens, container, author_name, terms
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::tsvector)
		ON CONFLICT (id) DO UPDATE SET
			tenant         = excluded.tenant,
			source         = excluded.source,
			kind           = excluded.kind,
			container_fold = excluded.container_fold,
			author_keys    = excluded.author_keys,
			owner_keys     = excluded.owner_keys,
			modified_at    = excluded.modified_at,
			mode           = excluded.mode,
			owner_key      = excluded.owner_key,
			queryable      = excluded.queryable,
			source_update  = excluded.source_update,
			title_tokens   = excluded.title_tokens,
			body_tokens    = excluded.body_tokens,
			container      = excluded.container,
			author_name    = excluded.author_name,
			terms          = excluded.terms`,
		d.ID, d.Tenant, d.Source, string(d.Kind),
		store.Fold(d.Container), nonEmpty(store.PersonKeys(d.Author)), nonEmpty(store.PersonKeys(d.Owner)),
		nullableNanos(d.ModifiedAt), int16(d.Permissions.Mode), ownerKey, d.Queryable(), d.SourceUpdate,
		a.TitleTokens, a.BodyTokens, d.Container, d.Author.Display(), terms)

	b.Queue(`
		INSERT INTO document_data (doc_id, data) VALUES ($1, $2)
		ON CONFLICT (doc_id) DO UPDATE SET data = excluded.data`, d.ID, string(data))

	refs := refsOf(d.Permissions)
	if len(refs) > 0 {
		effects := make([]int16, len(refs))
		scopes := make([]int16, len(refs))
		keys := make([]string, len(refs))
		for i, r := range refs {
			effects[i], scopes[i], keys[i] = r.effect, r.scope, r.key
		}
		b.Queue(`
			INSERT INTO document_ref (doc_id, effect, scope, key)
			SELECT $1, e, s, k FROM unnest($2::smallint[], $3::smallint[], $4::text[]) AS t(e, s, k)`,
			d.ID, effects, scopes, keys)
	}
	if content != nil {
		b.Queue(`INSERT INTO document_content (doc_id, width, height, bytes) VALUES ($1, $2, $3, $4)`,
			d.ID, content.Width, content.Height, content.Bytes)
	}
	return nil
}

// appendPostings adds one document's term vector to the rows waiting to be
// copied.
//
// The terms are sorted, so that the copy lands in one ascending run of the
// primary key rather than scattering across it, and so that map iteration order
// stops being something the write cost depends on.
func appendPostings(rows []posting, id string, a doc.Analysis) []posting {
	terms := make([]string, 0, len(a.Terms))
	for term := range a.Terms {
		terms = append(terms, term)
	}
	slices.Sort(terms)
	for _, term := range terms {
		c := a.Terms[term]
		rows = append(rows, posting{docID: id, term: term, titleTF: c.Title, bodyTF: c.Body})
	}
	return rows
}

// ref is one row of document_ref.
type ref struct {
	effect int16 // 0 allow, 1 deny
	scope  int16 // 0 user,  1 group
	key    string
}

// refsOf flattens a permission descriptor into rows, in the key forms acl
// compares.
func refsOf(perm acl.Permissions) []ref {
	var out []ref
	add := func(effect, scope int16, refs []acl.Ref, key func(acl.Ref) string) {
		for _, r := range refs {
			if k := key(r); k != "" {
				out = append(out, ref{effect: effect, scope: scope, key: k})
			}
		}
	}
	add(0, 0, perm.AllowUsers, acl.Ref.UserKey)
	add(0, 1, perm.AllowGroups, acl.Ref.GroupKey)
	add(1, 0, perm.DenyUsers, acl.Ref.UserKey)
	add(1, 1, perm.DenyGroups, acl.Ref.GroupKey)
	return out
}

// sortedKeys keeps the statements in a batch in the same order every time, so
// that two transactions taking the same row locks take them in the same order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
