package pgstore

import (
	"context"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Access = (*Store)(nil)

// Reachable counts what one principal may read, by source and by kind.
//
// The permission rule is the same clause every read path here builds, so there
// is one definition of who may see what and this is not a second one that can
// drift from it. The counting is an aggregate over it rather than a walk: the
// answer to "how much of this corpus can this person reach" is a handful of
// numbers, and reading a million documents to arrive at a handful of numbers is
// how a screen that has to answer in ten milliseconds ends up taking a second.
//
// The documents the rule admits are materialised once and grouped twice, rather
// than the clause being run once per grouping. The pass over the corpus is all
// of the cost and there is no reason to pay for it twice to count the same rows
// two ways.
func (s *Store) Reachable(ctx context.Context, p *acl.Principal) (store.Reach, error) {
	if err := s.ready(ctx); err != nil {
		return store.Reach{}, err
	}
	if p == nil {
		return store.Reach{}, genba.ErrNoPrincipal
	}

	var out store.Reach
	err := s.retry(ctx, func(ctx context.Context) error {
		c := visible(p)
		rows, err := s.query(ctx, `
			WITH mine AS MATERIALIZED (
				SELECT d.source AS source, d.kind AS kind FROM document d
				WHERE `+c.where()+`
			)
			SELECT 'source' AS field, source AS value, count(*) AS n FROM mine GROUP BY source
			UNION ALL
			SELECT 'kind', kind, count(*) FROM mine GROUP BY kind`, c.args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		// Cleared on every attempt, for the same reason the quarantine listing
		// clears its own: a retry that appended to what the failed attempt had
		// already read would count half the corpus twice.
		out = store.Reach{}
		for rows.Next() {
			var (
				field string
				f     store.Facet
			)
			if err := rows.Scan(&field, &f.Value, &f.Count); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			switch field {
			case "source":
				out.Sources = append(out.Sources, f)
			case "kind":
				out.Kinds = append(out.Kinds, f)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return store.Reach{}, fmt.Errorf("pgstore: reachable: %w", err)
	}
	return out, nil
}
