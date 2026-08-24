package sqlitestore

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
// The counting is an aggregate rather than a walk: the answer to "how much of
// this corpus can this person reach" is a handful of numbers, and reading a
// hundred thousand documents to arrive at a handful of numbers is how a screen
// that has to answer in ten milliseconds ends up taking a second. The rule it
// aggregates over is [reachable], which is the rule [visible] applies in the
// order it applies it.
//
// Both groupings come back from one statement, labelled by the field they
// grouped on. Two statements would apply the permission rule to the whole
// corpus twice to answer two questions about the same rows.
func (s *Store) Reachable(ctx context.Context, p *acl.Principal) (store.Reach, error) {
	if err := s.ready(ctx); err != nil {
		return store.Reach{}, err
	}
	if p == nil {
		return store.Reach{}, genba.ErrNoPrincipal
	}

	q, args := reachable(p)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return store.Reach{}, fmt.Errorf("sqlitestore: reachable: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out store.Reach
	for rows.Next() {
		var (
			field string
			f     store.Facet
		)
		if err := rows.Scan(&field, &f.Value, &f.Count); err != nil {
			return store.Reach{}, fmt.Errorf("sqlitestore: reachable: %w", err)
		}
		// One per value rather than one per document, which is the difference
		// this method exists for and is worth counting honestly: a change that
		// turned this back into a walk would show up here first.
		s.counters.rows.Add(1)
		switch field {
		case "source":
			out.Sources = append(out.Sources, f)
		case "kind":
			out.Kinds = append(out.Kinds, f)
		}
	}
	if err := rows.Err(); err != nil {
		return store.Reach{}, fmt.Errorf("sqlitestore: reachable: %w", err)
	}
	return out, nil
}
