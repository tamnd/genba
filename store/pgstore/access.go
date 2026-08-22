package pgstore

import (
	"context"
	"fmt"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

var _ store.Access = (*Store)(nil)

// Reachable counts what one principal may read, by source.
//
// The permission rule is the same clause every read path here builds, so there
// is one definition of who may see what and this is not a second one that can
// drift from it. The counting is an aggregate over it rather than a walk: the
// answer to "how much of this corpus can this person reach" is a handful of
// numbers, and reading a million documents to arrive at a handful of numbers is
// how a screen that has to answer in ten milliseconds ends up taking a second.
func (s *Store) Reachable(ctx context.Context, p *acl.Principal) ([]store.Reach, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, genba.ErrNoPrincipal
	}

	var out []store.Reach
	err := s.retry(ctx, func(ctx context.Context) error {
		c := visible(p)
		rows, err := s.query(ctx, `
			SELECT d.source, count(*) FROM document d
			WHERE `+c.where()+`
			GROUP BY d.source`, c.args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		// Cleared on every attempt, for the same reason the quarantine listing
		// clears its own: a retry that appended to what the failed attempt had
		// already read would count half the corpus twice.
		out = nil
		for rows.Next() {
			var r store.Reach
			if err := rows.Scan(&r.Source, &r.Documents); err != nil {
				return err
			}
			s.counters.rows.Add(1)
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgstore: reachable: %w", err)
	}
	return out, nil
}
