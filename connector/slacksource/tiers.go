package slacksource

import (
	"net/http"
	"path"

	"github.com/tamnd/genba/connector/limit"
)

// Slack publishes a rate limit per method rather than per token, and sorts the
// methods into tiers. The numbers below are the documented sustained rates in
// requests per minute, and they differ by a factor of a hundred across the
// methods one crawl uses.
//
// One bucket for all of them has to be set to the slowest tier or it is set too
// fast for it, and neither is acceptable: the first spends a crawl's whole
// budget waiting to list users, and the second is refused. So each tier gets a
// bucket, and a request is routed to one by the method it names.
const (
	tier1 = 1.0 / 60
	tier2 = 20.0 / 60
	tier3 = 50.0 / 60
	tier4 = 100.0 / 60
)

// method is the last path segment of a Slack API URL, which is the method name.
func method(u string) string { return path.Base(u) }

// tierOf says which bucket a method draws from. An unknown method is treated as
// the slowest useful tier rather than the fastest, because being wrong in that
// direction costs time and being wrong in the other costs a refusal.
func tierOf(m string) float64 {
	switch m {
	case "conversations.history", "conversations.replies", "conversations.info":
		return tier3
	case "users.info", "users.list":
		return tier4
	case "conversations.list", "conversations.members":
		return tier2
	default:
		return tier2
	}
}

// tiers routes each request to the limiter for the tier its method is in.
//
// It is a round tripper rather than four clients because the alternative is
// making every call site pick a client, and a call site that picks the wrong one
// is a bug that only shows up as throttling on somebody else's workspace.
type tiers struct {
	token string
	byURL map[float64]http.RoundTripper
}

var _ http.RoundTripper = (*tiers)(nil)

// tiered builds a client whose requests are rate limited per Slack tier and
// authenticated with the token.
//
// The limits given are scaled: the rate is treated as a multiplier on each
// tier's published rate, so a caller who has been asked to halve their traffic
// says so once. A zero rate leaves the published rates alone.
func tiered(token string, l limit.Limits) *http.Client {
	scale := l.Rate
	if scale <= 0 {
		scale = 1
	}
	t := &tiers{token: token, byURL: make(map[float64]http.RoundTripper, 4)}
	for _, rate := range []float64{tier1, tier2, tier3, tier4} {
		per := l
		per.Rate = rate * scale
		if per.Burst == 0 {
			// A page of a listing followed immediately by the threads on it is
			// a burst, and a limiter with no burst would space them out for no
			// reason. Slack's own guidance is that a short burst is fine and a
			// sustained one is not.
			per.Burst = 5
		}
		t.byURL[rate] = limit.NewTransport(per)
	}
	return &http.Client{Transport: t}
}

func (t *tiers) RoundTrip(req *http.Request) (*http.Response, error) {
	// The token goes on here rather than at the call sites so that there is one
	// place it can be forgotten, and it is a bearer header rather than a form
	// field because a form field ends up in a recording.
	if req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.byURL[tierOf(method(req.URL.Path))].RoundTrip(req)
}

// stats reports what every tier has spent, which is what an operator watches
// when a workspace starts taking longer than it did yesterday.
func (t *tiers) stats() limit.TransportStats {
	var out limit.TransportStats
	for _, rt := range t.byURL {
		tr, ok := rt.(*limit.Transport)
		if !ok {
			continue
		}
		s := tr.Stats()
		out.Requests += s.Requests
		out.Retries += s.Retries
		out.Trips += s.Trips
		out.Open = out.Open || s.Open
		out.Limiter.Waits += s.Limiter.Waits
		out.Limiter.Waited += s.Limiter.Waited
		out.Limiter.Pauses += s.Limiter.Pauses
	}
	return out
}

// Stats reports what this workspace has cost in requests, retries and waiting.
// A client the caller supplied has no limiter in it and reports nothing.
func (s *Service) Stats() limit.TransportStats {
	t, ok := s.http.Transport.(*tiers)
	if !ok {
		return limit.TransportStats{}
	}
	return t.stats()
}
