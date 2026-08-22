package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tamnd/genba/store"
)

var _ store.Feeds = (*Store)(nil)

// Feeds returns every configured feed for one tenant.
//
// There are as many rows here as somebody has configured connectors, which is
// tens at the outside, so this reads them all rather than paging. A deployment
// that needs a page of connectors has a different problem than this method.
func (s *Store) Feeds(ctx context.Context, tenant string) ([]store.Feed, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `
		SELECT source, kind, enabled, config, by_subject, created, updated
		FROM feed WHERE tenant = ?`, tenant)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: feeds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Feed
	for rows.Next() {
		f := store.Feed{Tenant: tenant}
		var config string
		var created, updated int64
		if err := rows.Scan(&f.Source, &f.Kind, &f.Enabled, &config, &f.By, &created, &updated); err != nil {
			return nil, fmt.Errorf("sqlitestore: feeds: %w", err)
		}
		f.Config = json.RawMessage(config)
		f.Created = time.Unix(0, created).UTC()
		f.Updated = time.Unix(0, updated).UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: feeds: %w", err)
	}
	return out, nil
}

// SaveFeed inserts or replaces one feed.
func (s *Store) SaveFeed(ctx context.Context, f store.Feed) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := f.Check(); err != nil {
		return fmt.Errorf("sqlitestore: save feed: %w", err)
	}

	now := time.Now().UTC().UnixNano()
	// The upsert keeps the row's own age and moves nothing else backwards. It
	// is one statement rather than a read and a write because two operators
	// editing the same connector at once would otherwise race for which of
	// them read the older created, and the answer to that is in the database.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feed (tenant, source, kind, enabled, config, by_subject, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant, source) DO UPDATE SET
			kind = excluded.kind,
			enabled = excluded.enabled,
			config = excluded.config,
			by_subject = excluded.by_subject,
			updated = excluded.updated`,
		f.Tenant, f.Source, f.Kind, f.Enabled, string(f.Config), f.By, now, now)
	if err != nil {
		return fmt.Errorf("sqlitestore: save feed %s: %w", f.Source, err)
	}
	return nil
}

// DropFeed forgets one feed, and forgets nothing else.
//
// There is no foreign key from document to feed and this statement touches one
// table, which is the point: the documents that feed indexed are still there
// afterwards, because forgetting how a corpus was read is not a decision to
// delete the corpus.
func (s *Store) DropFeed(ctx context.Context, tenant, source string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM feed WHERE tenant = ? AND source = ?`, tenant, source); err != nil {
		return fmt.Errorf("sqlitestore: drop feed %s: %w", source, err)
	}
	return nil
}
