package pgstore

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
// tens at the outside, so this reads them all rather than paging. It is inside
// the retry because it collects the whole answer before returning it, so a
// second attempt has not already handed the caller half a list.
func (s *Store) Feeds(ctx context.Context, tenant string) ([]store.Feed, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}

	var out []store.Feed
	err := s.retry(ctx, func(ctx context.Context) error {
		rows, err := s.query(ctx, `
			SELECT source, kind, enabled, config, by_subject, created, updated
			FROM feed WHERE tenant = ?`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()

		out = nil
		for rows.Next() {
			f := store.Feed{Tenant: tenant}
			var config string
			var created, updated int64
			if err := rows.Scan(&f.Source, &f.Kind, &f.Enabled, &config, &f.By, &created, &updated); err != nil {
				return err
			}
			f.Config = json.RawMessage(config)
			f.Created = time.Unix(0, created).UTC()
			f.Updated = time.Unix(0, updated).UTC()
			s.counters.rows.Add(1)
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: feeds: %w", err)
	}
	return out, nil
}

// SaveFeed inserts or replaces one feed.
func (s *Store) SaveFeed(ctx context.Context, f store.Feed) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := f.Check(); err != nil {
		return fmt.Errorf("pgstore: save feed: %w", err)
	}

	now := time.Now().UTC().UnixNano()
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		// One statement rather than a read and a write, so that the row keeps
		// its own age without two operators editing the same connector racing
		// for which of them read the older created. The answer to that is in
		// the database and this leaves it there.
		_, err := s.pool.Exec(ctx, rebind(`
			INSERT INTO feed (tenant, source, kind, enabled, config, by_subject, created, updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant, source) DO UPDATE SET
				kind = excluded.kind,
				enabled = excluded.enabled,
				config = excluded.config,
				by_subject = excluded.by_subject,
				updated = excluded.updated`),
			f.Tenant, f.Source, f.Kind, f.Enabled, string(f.Config), f.By, now, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: save feed %s: %w", f.Source, err)
	}
	return nil
}

// DropFeed forgets one feed, and forgets nothing else.
//
// There is no reference from document to feed and this statement touches one
// table, which is the point: the documents that feed indexed are still there
// afterwards, because forgetting how a corpus was read is not a decision to
// delete the corpus.
func (s *Store) DropFeed(ctx context.Context, tenant, source string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	err := s.retry(ctx, func(ctx context.Context) error {
		s.counters.statements.Add(1)
		_, err := s.pool.Exec(ctx, rebind(`DELETE FROM feed WHERE tenant = ? AND source = ?`), tenant, source)
		return err
	})
	if err != nil {
		return fmt.Errorf("pgstore: drop feed %s: %w", source, err)
	}
	return nil
}
