package objectsource

import (
	"context"
	"path"
	"strings"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
)

var (
	_ connector.Enumerator = (*Source)(nil)
	_ connector.Fetcher    = (*Source)(nil)
	_ connector.Counted    = (*Source)(nil)
)

// Enumerate calls fn for every object the source would index, with its entity
// tag as the version.
//
// It is the same listing [Source.Sync] makes and it fetches nothing. That is
// the whole difference in price: a thousand objects described per request
// against one object read per request, which on a real bucket is a minute
// against an afternoon, and it is the reason the reconciliation sweep can run
// on the refresh interval rather than nightly.
func (s *Source) Enumerate(ctx context.Context, fn func(connector.Item) bool) error {
	var token string
	for {
		page, err := s.client.list(ctx, s.prefix, "", token, s.pageSize)
		if err != nil {
			return err
		}
		for _, o := range page.objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !s.wanted(o) {
				continue
			}
			if !fn(connector.Item{ID: s.id(o.Key), Version: o.version()}) {
				// A listing this one stopped on purpose is not a failed
				// listing, and reporting it as one would make a reconciliation
				// that used an early exit delete the entire index.
				return nil
			}
		}
		if !page.more || page.next == "" {
			return nil
		}
		token = page.next
	}
}

// Fetch reads one object by document id.
//
// It returns [connector.ErrGone] for an object that is no longer there, which
// is the normal answer on a bucket people are writing to rather than a failure.
func (s *Source) Fetch(ctx context.Context, id string) (doc.Document, error) {
	if err := ctx.Err(); err != nil {
		return doc.Document{}, err
	}
	key, ok := s.key(id)
	if !ok {
		// An id this source did not mint names an object it does not have.
		// Saying so is the same answer as a deleted object, and it is the safe
		// one: the caller drops it from the index rather than storing something
		// read from a key an id was allowed to steer.
		return doc.Document{}, connector.ErrGone
	}

	raw, fetched, err := s.client.get(ctx, key, s.limitFor(key))
	if err != nil {
		return doc.Document{}, gone(err)
	}
	return s.document(ctx, fetched, raw)
}

// Counters returns what this source has spent on the store.
//
// The numbers come from the client, so a permission policy sharing that client
// is counted here too. That is the point: the question an operator is asking is
// what the sync cost, and a policy reading an access control list per object is
// the most expensive thing in it.
func (s *Source) Counters() connector.Counters { return s.client.Counters() }

// id is the document id for one key.
func (s *Source) id(key string) string { return s.name + ":" + key }

// key is the inverse of id, and is where a key that tried to leave the prefix
// is stopped.
//
// The cleaning is not tidiness. An id arrives from the index, the index was
// written by a connector, and a connector is a lot of code to trust with a
// string that turns into a path in a URL. A key holding a dot dot segment could
// be rewritten by a proxy on the way and end up naming something outside the
// bucket, so anything that does not survive being cleaned is refused rather
// than repaired. That also turns away the handful of legal keys with an empty
// path segment in them, which is a fair price for the rule being one line.
func (s *Source) key(id string) (string, bool) {
	key, ok := strings.CutPrefix(id, s.name+":")
	if !ok || key == "" {
		return "", false
	}
	if strings.TrimPrefix(path.Clean("/"+key), "/") != key {
		return "", false
	}
	// The prefix is the whole of what this source was pointed at. An id naming
	// something outside it belongs to a different source over the same bucket,
	// and answering for it would let one prefix's index quietly hold another's
	// documents under another's permissions.
	if !strings.HasPrefix(key, s.prefix) {
		return "", false
	}
	return key, true
}
