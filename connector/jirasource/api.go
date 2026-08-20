package jirasource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tamnd/genba/connector/limit"
)

// limited builds the client every request goes out on.
//
// One bucket rather than the per method tiers a source with published rates
// gets, because Jira publishes none: it bills by the cost of what was asked
// for, and the way a client is told it has spent too much is a 429 with a
// Retry-After on it. Obeying that header is [limit]'s job and it is the same
// code every other connector here uses.
func limited(email, token string, l limit.Limits) *http.Client {
	if l.Burst == 0 {
		// A page of a search followed immediately by the comments on what was on
		// it is a burst, and spacing those out would be slower for no benefit to
		// anybody. A sustained burst is what a rate limit is for and that is
		// still limited.
		l.Burst = 10
	}
	return &http.Client{Transport: &basic{
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
		base: limit.NewTransport(l),
	}}
}

// basic puts the credentials on every request.
//
// Jira Cloud authenticates an API token with basic authentication over the
// account's email address, which is a header rather than a query parameter, and
// that is the half of it worth being deliberate about: a token in a query
// string ends up in a server log, in a recording and in a bug report.
type basic struct {
	auth string
	base http.RoundTripper
}

var _ http.RoundTripper = (*basic)(nil)

func (b *basic) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", b.auth)
	}
	return b.base.RoundTrip(req)
}

// Stats reports what this site has cost in requests, retries and waiting. A
// client the caller supplied has no limiter in it and reports nothing.
func (s *Service) Stats() limit.TransportStats {
	b, ok := s.http.Transport.(*basic)
	if !ok {
		return limit.TransportStats{}
	}
	t, ok := b.base.(*limit.Transport)
	if !ok {
		return limit.TransportStats{}
	}
	return t.Stats()
}

// failure is the shape Jira refuses in.
type failure struct {
	Messages []string          `json:"errorMessages"`
	Errors   map[string]string `json:"errors"`
}

// call asks the site one question and decodes the answer into out.
//
// Every call is a GET. Two of the endpoints used here have a POST form that
// takes a larger request, and neither is worth it: a request carrying a body
// cannot be replayed by a transport that has already handed the body away, so
// [limit] will not retry one, and a 429 is the normal way this API asks a
// crawler to slow down rather than an unusual event.
func (s *Service) call(ctx context.Context, path string, form url.Values, out any) error {
	u := s.base + path
	if len(form) > 0 {
		u += "?" + form.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("jirasource: building the %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("jirasource: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Four megabytes is far more than any of these endpoints returns at the page
	// sizes this adapter asks for, and a bound is what keeps a site that has
	// started answering with something else from being a memory problem. It is
	// larger than the one the chat adapter uses because a page of issues carries
	// a hundred descriptions and a chat page carries two hundred sentences.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("jirasource: reading the %s response: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Path: path, Status: resp.StatusCode}
		var f failure
		if json.Unmarshal(raw, &f) == nil {
			e.Messages = append(e.Messages, f.Messages...)
			for field, why := range f.Errors {
				e.Messages = append(e.Messages, field+": "+why)
			}
		}
		return e
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("jirasource: decoding the %s response: %w", path, err)
		}
	}
	return nil
}

// window is what every offset paged Jira listing carries alongside its items.
//
// Three endpoints report the end of a listing three different ways: a total to
// compare against, an isLast flag, or nothing at all. All three are handled
// rather than one of them, because a crawl that guessed wrong either stops
// early and quietly indexes half a project or loops for ever.
type window struct {
	StartAt    int  `json:"startAt"`
	MaxResults int  `json:"maxResults"`
	Total      int  `json:"total"`
	IsLast     bool `json:"isLast"`
}

func (w window) page() window { return w }

// windowed is anything with a [window] embedded in it, which is every listing
// this adapter reads.
type windowed interface{ page() window }

// pages walks an offset paged endpoint, decoding each page and handing it to
// fn, which reports how many items were on it and whether to carry on.
//
// The page is a fresh value on every turn of the loop, and that is not
// tidiness. Decoding JSON into a slice that already has elements in it reuses
// those elements and only overwrites the fields the new document mentions, so a
// second page decoded into the first page's slice inherits every field the
// second page left out.
func pages[T windowed](ctx context.Context, s *Service, path string, form url.Values, fn func(T) (count int, more bool)) error {
	at := 0
	for {
		q := url.Values{}
		for k, v := range form {
			q[k] = append([]string(nil), v...)
		}
		q.Set("startAt", strconv.Itoa(at))
		q.Set("maxResults", strconv.Itoa(s.page))

		var page T
		if err := s.call(ctx, path, q, &page); err != nil {
			return err
		}

		got, more := fn(page)
		w := page.page()

		// What was asked for is not what was granted. Every one of these
		// endpoints caps the page size at whatever the site is configured to
		// allow and says so in maxResults, and a crawl that compared a fifty
		// item page against the hundred it asked for would decide the listing
		// had ended and index half a project.
		size := s.page
		if w.MaxResults > 0 && w.MaxResults < size {
			size = w.MaxResults
		}

		switch {
		case !more, got == 0, w.IsLast:
			return nil
		case w.Total > 0 && at+got >= w.Total:
			return nil
		case got < size:
			// A short page is the end of the listing on the endpoints that
			// report neither a total nor a flag, and it is the end on the others
			// too. Asking again would be a request for nothing.
			return nil
		}
		at += got
	}
}

// project is one entry of the project listing.
type project struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// account is a person, as Jira reports one.
//
// The account id is the identifier and the display name is a label. They are
// not the same thing and the difference matters: a display name is chosen by
// its owner, two people may have the same one, and a permission rule written
// against one would be a permission rule anybody could grant themselves.
type account struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"emailAddress"`
	Active      bool   `json:"active"`
}

// issue is one ticket, with the fields this adapter asks for.
type issue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Created     string          `json:"created"`
		Updated     string          `json:"updated"`
		Reporter    *account        `json:"reporter"`
		Creator     *account        `json:"creator"`
		Assignee    *account        `json:"assignee"`
		Status      *struct {
			Name string `json:"name"`
		} `json:"status"`
		IssueType *struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority"`
		Labels []string `json:"labels"`

		// Security is the issue security level, and it is the field that makes
		// this connector interesting. An issue that has one is readable by that
		// level's members and by nobody else, whatever the project says.
		Security *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"security"`

		Comment *comments `json:"comment"`
	} `json:"fields"`
}

// comment is one entry in the argument underneath a ticket.
type comment struct {
	ID      string          `json:"id"`
	Author  *account        `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
	Updated string          `json:"updated"`

	// Visibility is the restriction a single comment can carry, which limits it
	// to one role or group inside a project everybody else can read.
	Visibility *struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"visibility"`
}

// comments is the paged listing of them, which arrives both inline on an issue
// and on its own endpoint.
type comments struct {
	window
	Comments []comment `json:"comments"`
}

// stamp turns a Jira timestamp into a time.
//
// Jira writes them as ISO 8601 with milliseconds and a numeric zone and no
// colon in it, which is not RFC 3339 and will not parse as one. Both layouts
// are tried, because a site behind a proxy that rewrites them, or a future
// version that fixes it, should not be a crawl that thinks every issue changed
// at the zero time.
func stamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999-0700",
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// stampMillis renders a date node's timestamp, which arrives as milliseconds
// since the epoch and is a date rather than an instant.
func stampMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// jqlTime is the format JQL wants a time in.
//
// It has no seconds. JQL compares to the minute and rejects anything finer, so
// the time is rounded down rather than truncated to the minute and rounded up,
// because the direction that costs a re-read of a few issues is the one that
// does not lose them.
func jqlTime(t time.Time) string {
	return t.UTC().Truncate(time.Minute).Format("2006-01-02 15:04")
}
