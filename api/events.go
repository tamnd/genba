package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/store"
)

// The event stream exists so that an open tab does not have to poll to find out
// that the corpus moved.
//
// It carries no document content, no ids and no titles. It says that something
// in the caller's own tenant changed, how many documents were involved and
// whether they were removed, and the browser decides what to refetch through
// the endpoints that already apply the permission rule. That is the whole
// design: a push channel that leaks nothing is one that never carries anything
// worth leaking, and every refetch it triggers goes through the same checks a
// first load does.

// HeartbeatInterval is how often the stream sends a comment when nothing has
// happened.
//
// Twenty five seconds, because the proxies in front of a deployment tend to
// close an idle connection at thirty and a stream that gets dropped every
// thirty seconds is a reconnect storm rather than a live interface.
const HeartbeatInterval = 25 * time.Second

// eventBuffer is how many changes are held for a slow client before they start
// being dropped.
//
// Dropping is the correct behaviour here rather than a compromise. Every event
// carries the same instruction, which is to refetch, so a client that missed
// four of them and received the fifth does exactly what a client that received
// all five does. What must not happen is the store's write path blocking on a
// browser on a train.
const eventBuffer = 16

// handleEvents streams index changes for the caller's tenant.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	n, ok := s.store.(store.Notifier)
	if !ok {
		// A driver that cannot report its writes has no stream to offer. It is not
		// an error in the deployment, so it is the same not found any absent
		// resource produces, and the interface falls back to its refresh timer.
		writeError(w, http.StatusNotFound, "not_found", "this deployment does not report index changes")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "this server cannot stream")
		return
	}

	changes := make(chan store.Change, eventBuffer)
	stop := n.OnChange(func(c store.Change) {
		if c.Tenant != p.Tenant {
			return
		}
		select {
		case changes <- c:
		default:
		}
	})
	defer stop()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	// Proxies that buffer a response turn a live stream into a very slow one, and
	// this is the header the common ones read.
	h.Set("X-Accel-Buffering", "no")
	h.Set(varyHeader, varyValue)
	w.WriteHeader(http.StatusOK)

	// A comment before anything has happened, so the browser sees the response
	// start and fires its open event rather than sitting on a connection it
	// cannot tell is working.
	if _, err := w.Write([]byte(": open\n\n")); err != nil {
		return
	}
	flusher.Flush()

	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-changes:
			if _, err := w.Write(indexEvent(p.Tenant, len(c.IDs), c.Deleted, s.now())); err != nil {
				return
			}
			flusher.Flush()
		case <-beat.C:
			if _, err := w.Write([]byte(": beat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// indexEvent is the wire form of one change.
//
// It is written by hand rather than encoded, because the shape is four fields
// that cannot fail to encode and because an event stream frame is a text format
// with rules of its own about newlines. None of the values can contain one: the
// tenant comes from the authenticated principal and is quoted, the count is an
// integer, the timestamp is RFC 3339 and the last is a boolean.
func indexEvent(tenant string, documents int, deleted bool, at time.Time) []byte {
	return []byte(`event: index
data: {"tenant":` + strconv.Quote(tenant) + `,"at":"` + at.UTC().Format(time.RFC3339) +
		`","documents":` + strconv.Itoa(documents) +
		`,"deleted":` + strconv.FormatBool(deleted) + "}\n\n")
}
