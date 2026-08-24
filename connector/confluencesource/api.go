package confluencesource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/connector/limit"
)

// limited builds the client every request goes out on.
//
// One bucket rather than the per method tiers a source with published rates
// gets, because Atlassian publishes none: it bills by the cost of what was asked
// for, and the way a client is told it has spent too much is a 429 with a
// Retry-After on it. Obeying that header is [limit]'s job and it is the same
// code every other connector here uses.
func limited(email, token string, l limit.Limits) *http.Client {
	if l.Burst == 0 {
		// A page of search results followed immediately by the comments on
		// something that was on it is a burst, and spacing those out would be
		// slower for no benefit to anybody. A sustained burst is what a rate
		// limit is for and that is still limited.
		l.Burst = 10
	}
	return &http.Client{Transport: &basic{
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
		base: limit.NewTransport(l),
	}}
}

// basic puts the credentials on every request.
//
// Confluence Cloud authenticates an API token with basic authentication over the
// account's email address, which is a header rather than a query parameter, and
// that is the half of it worth being deliberate about: a token in a query string
// ends up in a server log, in a recording and in a bug report.
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

// failure is the shape Confluence refuses in.
type failure struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// why is the sentence out of a refusal, which is the part worth keeping.
func (f failure) why() string {
	switch {
	case f.Message != "" && f.Reason != "":
		return f.Reason + ": " + f.Message
	case f.Message != "":
		return f.Message
	default:
		return f.Reason
	}
}

// call asks the site one question and decodes the answer into out.
//
// Every call is a GET. The search endpoint has a POST form that takes a longer
// query, and it is not worth it: a request carrying a body cannot be replayed by
// a transport that has already handed the body away, so [limit] will not retry
// one, and a 429 is the normal way this API asks a crawler to slow down rather
// than an unusual event.
func (s *Service) call(ctx context.Context, path string, form url.Values, out any) error {
	u := s.wiki + path
	if len(form) > 0 {
		u += "?" + form.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("confluencesource: building the %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("confluencesource: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Eight megabytes is far more than any of these endpoints returns at the page
	// sizes this adapter asks for, and a bound is what keeps a site that has
	// started answering with something else from being a memory problem. It is
	// larger than the ticket adapter's because a page of wiki results carries
	// fifty documents and a page of tickets carries a hundred summaries.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("confluencesource: reading the %s response: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Path: path, Status: resp.StatusCode}
		var f failure
		if json.Unmarshal(raw, &f) == nil {
			e.Message = f.why()
		}
		return e
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("confluencesource: decoding the %s response: %w", path, err)
		}
	}
	return nil
}

// links is what every Confluence listing carries alongside its results.
type links struct {
	Next string `json:"next"`
}

// pages walks a listing and hands each page of results to fn, which reports
// whether to carry on.
//
// The end of a listing is the absence of a next link rather than a count, and
// what is done with that link is the one thing worth explaining. It is not
// followed. Only its query is taken, and the next request goes to the same path
// with it, because the link itself is not something to rely on: version one
// writes it relative to a context path, version two writes it relative to the
// host, and a site behind a proxy writes it relative to whatever the proxy
// thinks it is. The query is the only part of it that is information, and it is
// the same query whether the endpoint pages by cursor or by offset.
func pages[T any](ctx context.Context, s *Service, path string, form url.Values, fn func([]T) bool) error {
	q := url.Values{}
	for k, v := range form {
		q[k] = append([]string(nil), v...)
	}
	q.Set("limit", strconv.Itoa(s.page))

	// A listing that hands back a query it has already been asked is a listing
	// that would be walked for ever, which is a thing a proxy rewriting links
	// can produce out of a site that is behaving.
	seen := map[string]bool{q.Encode(): true}

	for {
		var page struct {
			Results []T   `json:"results"`
			Links   links `json:"_links"`
		}
		if err := s.call(ctx, path, q, &page); err != nil {
			return err
		}
		if len(page.Results) == 0 {
			return nil
		}
		if !fn(page.Results) {
			return nil
		}
		if page.Links.Next == "" {
			return nil
		}

		next, err := url.Parse(page.Links.Next)
		if err != nil {
			return fmt.Errorf("confluencesource: the %s listing carried an unreadable next link: %w", path, err)
		}
		q = next.Query()
		if len(q) == 0 || seen[q.Encode()] {
			return nil
		}
		seen[q.Encode()] = true
	}
}

// person is somebody, as Confluence reports one.
//
// The account id is the identifier and the display name is a label. They are not
// the same thing and the difference matters: a display name is chosen by its
// owner, two people may have the same one, and a permission rule written against
// one would be a permission rule anybody could grant themselves.
type person struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	PublicName  string `json:"publicName"`
	Email       string `json:"email"`
}

// group is a group, which is named rather than numbered because a name is what
// an identity provider hands back when it is asked what somebody is in.
type group struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// subjects is who a permission or a restriction names.
//
// The size is carried alongside the results because these lists are paged like
// everything else here, and a restriction the site sent half of is a restriction
// we do not know. The two numbers being different is the only way to find that
// out, since a short page of results and the end of the list look the same.
type subjects struct {
	User struct {
		Results []person `json:"results"`
		Size    int      `json:"size"`
	} `json:"user"`
	Group struct {
		Results []group `json:"results"`
		Size    int     `json:"size"`
	} `json:"group"`
}

// space is one entry of the space listing, with its permissions expanded.
type space struct {
	ID          int               `json:"id"`
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	Permissions []spacePermission `json:"permissions"`
}

// spacePermission is one grant on a space.
type spacePermission struct {
	Subjects  subjects `json:"subjects"`
	Operation struct {
		Operation  string `json:"operation"`
		TargetType string `json:"targetType"`
	} `json:"operation"`

	// AnonymousAccess is a space that is readable without signing in, and
	// UnlicensedAccess is one readable by everybody a service desk lets in.
	// Neither is an access control list of nobody, which is what an adapter
	// ignoring them would produce.
	AnonymousAccess  bool `json:"anonymousAccess"`
	UnlicensedAccess bool `json:"unlicensedAccess"`
}

// representation is a body in one of the formats Confluence keeps.
type representation struct {
	Value          string `json:"value"`
	Representation string `json:"representation"`
}

// restriction is who may do one thing to one page.
type restriction struct {
	Operation    string   `json:"operation"`
	Restrictions subjects `json:"restrictions"`
}

// content is a page or a comment, which are the same type to this API and are
// told apart by the type field.
type content struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Title  string `json:"title"`

	Space *struct {
		ID   int    `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`

	Body struct {
		// ADF is the Atlassian document format, and it arrives as a string
		// holding JSON rather than as JSON, which is the one surprise in this
		// whole response.
		ADF     *representation `json:"atlas_doc_format"`
		Storage *representation `json:"storage"`
	} `json:"body"`

	History *struct {
		CreatedBy   *person `json:"createdBy"`
		CreatedDate string  `json:"createdDate"`
	} `json:"history"`

	Version *struct {
		By     *person `json:"by"`
		When   string  `json:"when"`
		Number int     `json:"number"`
	} `json:"version"`

	// Container is what a comment is on, which is the only reason this field is
	// asked for: a comment is a sentence in a page rather than a document, and
	// the page it is a sentence in is what gets indexed.
	Container *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"container"`

	// Ancestors are the pages above this one, outermost first, and they are here
	// because a read restriction on any of them restricts this one.
	Ancestors []struct {
		ID string `json:"id"`
	} `json:"ancestors"`

	Restrictions *struct {
		Read *restriction `json:"read"`
	} `json:"restrictions"`

	Metadata *struct {
		Labels *struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`

	Children *struct {
		Comment *struct {
			Results []content `json:"results"`
			Links   links     `json:"_links"`
		} `json:"comment"`
	} `json:"children"`

	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

// stamp turns a Confluence timestamp into a time.
//
// The API writes them as ISO 8601 with milliseconds and an offset with a colon
// in it, which is RFC 3339. The layout without the colon is tried as well,
// because that is what the ticket half of the same product sends and a site
// behind something that rewrites them should not be a crawl that thinks every
// page changed at the zero time.
func stamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999-0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// cqlTime is the format CQL wants a time in.
//
// It has no seconds. CQL compares to the minute and rejects anything finer, so
// the time is rounded down rather than truncated to the minute and rounded up,
// because the direction that costs a re-read of a few pages is the one that does
// not lose them.
func cqlTime(t time.Time) string {
	return t.UTC().Truncate(time.Minute).Format("2006/01/02 15:04")
}

// quote puts a value into a CQL query.
//
// A space key is configurable, one of them will one day be a CQL keyword, and a
// query that broke on the day somebody made a space called ORDER is not a thing
// to debug twice.
func quote(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}
