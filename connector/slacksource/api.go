package slacksource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// envelope is what every Slack method wraps its answer in.
//
// A refusal is a 200 with ok false far more often than it is a status code, so
// the body is what decides whether a call worked. A client that only looked at
// the status would treat "you are not in that channel" as an empty channel.
type envelope struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// channel is one entry of conversations.list.
type channel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsIM       bool   `json:"is_im"`
	IsMPIM     bool   `json:"is_mpim"`

	// Updated is when the channel's own settings last changed, in
	// milliseconds. A rename, an archive and a conversion between public and
	// private all move it. Somebody being removed from a private channel does
	// not, which is dealt with elsewhere and is the reason this comment exists.
	Updated int64 `json:"updated"`
}

// message is one entry of conversations.history or conversations.replies.
type message struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	User       string `json:"user"`
	BotID      string `json:"bot_id"`
	Username   string `json:"username"`
	Text       string `json:"text"`
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	ReplyCount int    `json:"reply_count"`
	// LatestReply is the timestamp of the newest reply, and it is the only
	// thing about a parent message that moves when somebody answers it.
	LatestReply string `json:"latest_reply"`
	Edited      *struct {
		TS   string `json:"ts"`
		User string `json:"user"`
	} `json:"edited"`
}

// user is what users.info returns.
type user struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	Profile  struct {
		RealName string `json:"real_name"`
		Email    string `json:"email"`
	} `json:"profile"`
}

// call asks Slack one question and decodes the answer into out.
//
// It is a GET even though half of Slack's examples are POSTs, and the reason is
// retrying. A request carrying a body cannot be replayed by a transport that
// has already handed the body away, so [limit] will not retry one, and being
// throttled is not an unusual event on this API: it is the normal way Slack
// tells a crawler to slow down. Every method used here is a read, every read
// method accepts its parameters in the query string, and a GET is what makes
// the retry, the backoff and the circuit breaker apply to all of them.
//
// out is decoded from the same bytes as the envelope rather than from a second
// read, because a Slack response is small and reading it twice is a second
// allocation for no benefit.
func (s *Service) call(ctx context.Context, name string, form url.Values, out any) (next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+name+"?"+form.Encode(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("slacksource: building the %s request: %w", name, err)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("slacksource: %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A megabyte is far more than any of these methods returns at the page
	// sizes this adapter asks for, and a bound is what keeps a source that has
	// started answering with something else from being a memory problem.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("slacksource: reading the %s response: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("slacksource: %s: %s", name, resp.Status)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("slacksource: decoding the %s response: %w", name, err)
	}
	if !env.OK {
		code := env.Error
		if code == "" {
			code = "no reason given"
		}
		return "", &Error{Method: name, Code: code}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return "", fmt.Errorf("slacksource: decoding the %s response: %w", name, err)
		}
	}
	return env.Metadata.NextCursor, nil
}

// pages walks a cursor paged method, decoding each page and handing it to fn.
//
// fn returns false to stop, which is not an error: a listing the caller ended on
// purpose is not a failed listing, and reporting it as one is how a
// reconciliation sweep ends up emptying an index.
//
// The page is a fresh value on every turn of the loop, and that is not tidiness.
// Decoding JSON into a slice that already has elements in it reuses those
// elements and only overwrites the fields the new document mentions, so a second
// page decoded into the first page's slice inherits every field the second page
// left out. A message with no subtype landing where a channel_join used to be
// becomes a channel_join, and the thread it starts silently stops being indexed.
func pages[T any](ctx context.Context, s *Service, name string, form url.Values, fn func(T) bool) error {
	// The form is copied because a caller reusing it for a second channel would
	// otherwise inherit the cursor of the first, and what that causes is a
	// channel silently starting halfway through.
	q := url.Values{}
	for k, v := range form {
		q[k] = append([]string(nil), v...)
	}
	for {
		var page T
		next, err := s.call(ctx, name, q, &page)
		if err != nil {
			return err
		}
		if !fn(page) {
			return nil
		}
		if next == "" {
			return nil
		}
		q.Set("cursor", next)
	}
}

// people resolves Slack user ids to the person a document says wrote it.
//
// It is a cache because a busy channel is a few dozen people saying thousands
// of things, and a lookup per message would spend the whole rate limit on
// names. It never expires: a display name that changed mid crawl is not worth a
// second request, and the next full sync picks it up.
type people struct {
	svc *Service

	mu sync.Mutex
	by map[string]doc.Person
}

func newPeople(s *Service) *people {
	return &people{svc: s, by: make(map[string]doc.Person)}
}

// person returns who a message is from.
//
// A message with no user is from an application, and the name it posted under
// is the best there is. A lookup that fails still produces a person, with the
// identity filled in and the name missing, because the alternative is failing a
// whole channel over a display name.
func (p *people) person(ctx context.Context, m message) doc.Person {
	if m.User == "" {
		name := m.Username
		if name == "" {
			name = m.BotID
		}
		if name == "" {
			return doc.Person{}
		}
		return doc.Person{
			Name:     name,
			Identity: acl.Identity{Source: p.svc.name, Value: name},
		}
	}

	p.mu.Lock()
	got, ok := p.by[m.User]
	p.mu.Unlock()
	if ok {
		return got
	}

	id := acl.Identity{Source: p.svc.name, Value: m.User}
	found := doc.Person{Subject: m.User, Identity: id, Name: m.User}

	var out struct {
		User user `json:"user"`
	}
	if _, err := p.svc.call(ctx, "users.info", url.Values{"user": {m.User}}, &out); err != nil {
		p.svc.skip(m.User, fmt.Errorf("looking up who wrote a message: %w", err))
	} else {
		name := out.User.Profile.RealName
		if name == "" {
			name = out.User.RealName
		}
		if name == "" {
			name = out.User.Name
		}
		found = doc.Person{
			Subject:  out.User.ID,
			Identity: id,
			Name:     name,
			Email:    out.User.Profile.Email,
		}
	}

	p.mu.Lock()
	p.by[m.User] = found
	p.mu.Unlock()
	return found
}

// stamp turns a Slack timestamp into a time.
//
// A Slack timestamp is seconds with six decimal places, as a string, and it is
// also the message's identifier. Parsing it as a float loses the last digits at
// the far end of the range, so the two halves are parsed separately.
func stamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	whole, frac, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var micros int64
	if frac != "" {
		// Slack pads to six digits. Anything else is padded here rather than
		// rejected, because a shorter fraction is still a time.
		for len(frac) < 6 {
			frac += "0"
		}
		micros, _ = strconv.ParseInt(frac[:6], 10, 64)
	}
	return time.Unix(sec, micros*int64(time.Microsecond)).UTC()
}

// slackTime turns a time back into the string Slack's oldest parameter wants.
func slackTime(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/int(time.Microsecond))
}

// errBadID is what an id that this source could never have produced comes back
// as, which is different from an id it produced and no longer has.
var errBadID = errors.New("slacksource: not an id from this workspace")
