package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/thumb"
)

// ThumbnailCacheSize is how many rendered thumbnails the process holds.
//
// A 256 pixel PNG of a photograph is around eighty kilobytes and the two
// smaller sizes are a few, so five hundred entries is a few tens of megabytes
// for a working set of a couple of hundred images, which is what a person
// paging through a corpus actually touches. It is a cache rather than a store
// because a thumbnail is derived data: losing it costs one decode.
const ThumbnailCacheSize = 512

// ThumbnailCacheExpiry bounds how stale a thumbnail can be.
//
// The cache key carries the document's version, so a document that says when it
// changed gets a new key and never serves the old picture. A connector that
// records no version at all has nothing to key on, and this is what stops that
// case from being permanent.
const ThumbnailCacheExpiry = time.Hour

// handleThumbnail serves a small version of a document that is an image.
//
// The permission check is the document lookup, which happens before anything is
// decoded and on the cached path as well as the cold one, so a thumbnail is
// never served to somebody who may not see the document it is of. What is
// cached is the picture rather than the permission: the bytes are a pure
// function of the document's content, and who may look at them is decided again
// on every request.
//
// The lookup also means a cache hit never touches the content. That is the
// point of keying on the document's version rather than on a hash of its bytes:
// hashing two megabytes to discover that we already have the four kilobyte
// answer would be most of the cost the endpoint exists to avoid.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request, p *acl.Principal) {
	size, ok := thumbnailSize(r)
	if !ok {
		// The one case that is not a 404. A size outside the enumeration is a
		// caller mistake rather than a statement about a document, it tells
		// nobody anything they did not already know, and answering it with a
		// nearest match would make an arbitrary size a valid request.
		writeError(w, http.StatusBadRequest, "bad_request", "size must be 48, 96 or 256")
		return
	}

	id := r.PathValue("id")
	// A thumbnail is a rendering of the document and it is enough of one to read
	// a page over somebody's shoulder, so every answer this endpoint gives is on
	// the trail, including the ones that give nothing.
	refused := func() {
		s.accessed(r, p, audit.Record{
			Action:    audit.Thumbnail,
			Outcome:   audit.Refused,
			Documents: []audit.Item{{ID: id}},
		})
		notThere(w)
	}

	cs, ok := s.store.(store.ContentStore)
	if !ok {
		refused()
		return
	}

	d, err := s.store.Get(r.Context(), p, id)
	if err != nil {
		if !errors.Is(err, genba.ErrNotFound) {
			s.log.Error("thumbnail lookup failed", "error", err)
			s.accessed(r, p, audit.Record{
				Action:    audit.Thumbnail,
				Outcome:   audit.Failed,
				Documents: []audit.Item{{ID: id}},
			})
			writeError(w, http.StatusInternalServerError, "internal", "the document could not be read")
			return
		}
		refused()
		return
	}

	// The source is asked before anything is decoded, so a document somebody has
	// lost access to costs a lookup rather than a render, and a picture of it is
	// never put in the cache on their behalf either.
	if !s.stillReadable(r, p, d) {
		refused()
		return
	}

	// Concurrent requests for the same thumbnail render it once and all get what
	// that one produced, which matters here because the first paint of a grid
	// asks for twenty four of these at once and a browser will happily open six
	// connections to do it.
	ctx := r.Context()
	got, err := s.thumbs.Do(thumbnailKey(d, size), func() (thumb.Thumbnail, error) {
		c, err := cs.Content(ctx, p, id)
		if err != nil {
			return thumb.Thumbnail{}, err
		}
		return thumb.Render(ctx, c.Bytes, size)
	})
	if err != nil {
		// A document with no bytes, a document in a format nobody decodes and a
		// file that is not an image at all are the same answer, and it is the
		// same answer a document that does not exist gets. The interface falls
		// back to the icon for the kind, which is what it draws anyway.
		refused()
		return
	}

	s.accessed(r, p, audit.Record{
		Action:    audit.Thumbnail,
		Outcome:   audit.Served,
		Documents: []audit.Item{item(d)},
		Count:     1,
		Rule:      ruleOf(p, d),
		Bytes:     int64(len(got.Bytes)),
	})

	sum := sha256.Sum256(got.Bytes)
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	h.Set("X-Content-Dimensions", strconv.Itoa(got.Width)+"x"+strconv.Itoa(got.Height))
	h.Set("Cache-Control", thumbnailCacheControl(r))
	http.ServeContent(w, r, "", d.ModifiedAt, bytes.NewReader(got.Bytes))
}

// notThere is the answer to every question this endpoint will not answer.
func notThere(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "no such document")
}

// thumbnailSize reads the size out of the request.
//
// An absent size is the largest, because a URL somebody typed by hand wants the
// picture rather than the tile, and every request the interface makes names one.
func thumbnailSize(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("size")
	if raw == "" {
		return thumb.Sizes[len(thumb.Sizes)-1], true
	}
	size, err := strconv.Atoi(raw)
	if err != nil || !thumb.Valid(size) {
		return 0, false
	}
	return size, true
}

// thumbnailCacheControl decides how long a browser may keep this.
//
// Immutability is a property of the URL rather than of the picture. A request
// that names a version is asking for the thumbnail of that version, which will
// never be a different picture, so it gets a year and no revalidation. A
// request without one is asking for whatever the document is now, which can
// change, so it gets ten minutes and an entity tag to check against.
func thumbnailCacheControl(r *http.Request) string {
	// Private in both cases, because which document this is depends on who
	// asked. A shared cache that kept it would be handing one tenant's
	// screenshot to another.
	if r.URL.Query().Get("v") != "" {
		return "private, max-age=31536000, immutable"
	}
	return "private, max-age=600"
}

// thumbnailKey names one picture of one version of one document at one size.
func thumbnailKey(d doc.Document, size int) string {
	return d.ID + "\x00" + documentVersion(d) + "\x00" + strconv.Itoa(size)
}

// documentVersion is whatever the source said about this revision.
//
// The source's own cursor first, because that is the thing the source promises
// changes when the document does. The modification time is the fallback, and an
// empty string is honest about a connector that records neither.
func documentVersion(d doc.Document) string {
	switch {
	case d.SourceUpdate != "":
		return d.SourceUpdate
	case !d.ModifiedAt.IsZero():
		return strconv.FormatInt(d.ModifiedAt.UnixNano(), 10)
	default:
		return ""
	}
}

// newThumbnailCache is where the one cache of rendered images is made.
func newThumbnailCache() *cache.Cache[thumb.Thumbnail] {
	return cache.New[thumb.Thumbnail](ThumbnailCacheSize, ThumbnailCacheExpiry)
}
