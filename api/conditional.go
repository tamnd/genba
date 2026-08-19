package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Every authenticated response is revalidated rather than reused.
//
// The interface keeps its own copy of what it has already seen, so the thing
// worth optimising is not the first request, it is the ninth time somebody
// comes back to a tab that is still showing the right answer. That is what the
// entity tag is for: the client asks whether what it has is current, and a
// server that has not changed its mind replies in a few hundred bytes with no
// body at all.
//
// The caching directives are deliberately conservative. Nothing here may be
// stored by a shared cache, because every one of these responses depends on who
// asked, and a proxy that kept one would hand one person's search results to
// the next. must-revalidate says the browser may keep a copy but may not use it
// without asking, which is exactly the contract the client cache in the
// interface is written against.
const (
	// cacheControl is sent on every authenticated response.
	cacheControl = "private, max-age=0, must-revalidate"

	// varyHeader and varyValue say what the response depends on besides the URL.
	// Without it, a cache anywhere between here and the browser is entitled to
	// treat two people's requests for the same path as the same request.
	varyHeader = "Vary"
	varyValue  = "Authorization, Cookie"
)

// writeConditional encodes v, tags it, and answers 304 when the caller already
// has it.
//
// identity is what the tag is computed over, and it is separate from the body
// because some responses carry a field that changes on every request without
// the answer having changed. A search reports how long it took, and hashing
// that would produce a tag that never matches, which is a revalidation that can
// never succeed and therefore a cache that never works. A caller passes the
// same value with those fields left out, and nil means the body itself.
func writeConditional(w http.ResponseWriter, r *http.Request, status int, v, identity any) {
	if identity == nil {
		identity = v
	}
	body, err := json.Marshal(v)
	if err != nil {
		// None of these bodies can fail to encode, and a caller who has found the
		// one that can is better served by an error than by half a response.
		writeError(w, http.StatusInternalServerError, "internal", "the response could not be encoded")
		return
	}

	tag := etag(identity)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", cacheControl)
	h.Set(varyHeader, varyValue)
	h.Set("ETag", tag)

	if status == http.StatusOK && matches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// etag is a strong tag over the canonical encoding of v.
//
// It is strong rather than weak because it is computed from the bytes that
// would have been sent, so two responses with the same tag really are byte for
// byte the same response, which is what lets the interface skip a repaint on a
// revalidation and keep the scroll position and the keyboard cursor where they
// were.
func etag(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// matches reports whether an If-None-Match header names tag.
//
// The header is a comma separated list and a client is allowed to send the
// wildcard, which means it has some copy and would like to be told if anything
// at all is current. A weak comparison is used, since a weak tag from a proxy
// that rewrote the body is still the same resource as far as this decision
// goes.
func matches(header, tag string) bool {
	if header == "" || tag == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for candidate := range strings.SplitSeq(header, ",") {
		if weak(strings.TrimSpace(candidate)) == weak(tag) {
			return true
		}
	}
	return false
}

func weak(tag string) string { return strings.TrimPrefix(tag, "W/") }
