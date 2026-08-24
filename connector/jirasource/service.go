package jirasource

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/adf"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// fields is what a crawl asks for on every issue.
//
// It is written down rather than left as everything, because everything on a
// site with three hundred custom fields is a page of search results the size of
// a small database and a request the site bills accordingly. Every name here is
// something that ends up in the document.
var fields = strings.Join([]string{
	"summary", "description", "created", "updated",
	"reporter", "creator", "assignee",
	"status", "issuetype", "priority", "labels",
	"security", "comment",
}, ",")

// Threads walks the issues in a project that changed at or after since.
//
// This is the part chat cannot do. Jira's updated field moves when anything
// about an issue moves, including a comment on it, and JQL will filter and
// order by it, so what a sync needs is exactly one query and there is no window
// to widen and nothing to guess at.
func (s *Service) Threads(ctx context.Context, c threadsource.Container, since time.Time, fn func(context.Context, threadsource.Thread) error) error {
	jql := "project = " + quote(c.ID)
	if !since.IsZero() {
		// At or after rather than after, because JQL compares to the minute and
		// an issue updated in the same minute as the cursor is one an exclusive
		// query loses for ever. The cursor's edge set is what stops the repeat
		// from being emitted twice.
		jql += ` AND updated >= "` + jqlTime(since) + `"`
	}
	jql += " ORDER BY updated ASC"

	return s.search(ctx, c, jql, fields, func(ctx context.Context, is issue) error {
		if !stamp(is.Fields.Updated).Before(since) {
			th, err := s.build(ctx, is)
			if err != nil {
				return err
			}
			return fn(ctx, th)
		}
		// JQL rounds to the minute and this does not, so a query asked for the
		// minute the cursor is in comes back with the issues from the first half
		// of it as well. Dropping them here rather than widening the query is
		// what keeps the request cheap and the answer exact.
		return nil
	})
}

// listFields is what a listing asks for, which is two fields rather than one.
//
// The updated time is the version the sweep compares. The security level is what
// keeps a scheduled permission refresh from writing the project's rule over an
// issue that was restricted out of the project, and it is asked for here because
// here is the one place it can be had without reading anything: a refresh that
// had to read an issue to find out whether it overrides its project is a
// recrawl, which is what the refresh exists instead of.
const listFields = "updated,security"

// List reports every issue the project currently holds, with the version the
// sweep compares and the rule the issue has of its own.
//
// Nothing in JQL reports an issue that was deleted or moved to another project,
// so this is the only thing that ever takes one out of the index.
func (s *Service) List(ctx context.Context, c threadsource.Container, fn func(threadsource.Item) bool) error {
	err := s.search(ctx, c, "project = "+quote(c.ID)+" ORDER BY key ASC", listFields, func(ctx context.Context, is issue) error {
		item := threadsource.Item{Item: connector.Item{ID: is.Key, Version: is.Fields.Updated}}
		if sec := is.Fields.Security; sec != nil && sec.ID != "" {
			// The same cache the sync resolves levels through, so a project with
			// a security scheme on thousands of issues costs one request per
			// level rather than one per issue.
			item.Access = s.levels.level(ctx, sec.ID, sec.Name)
		}
		if !fn(item) {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errStop) {
		// A listing the caller ended on purpose is not a failed listing, and
		// reporting it as one is how a reconciliation sweep ends up emptying an
		// index.
		return nil
	}
	return err
}

// errStop is how a caller that has seen enough gets out of a walk, and it never
// leaves this package.
var errStop = errors.New("jirasource: enough")

// search runs a JQL query and hands each issue to fn.
//
// The one thing worth knowing is what it does about a project this account
// cannot browse: nothing, deliberately. Jira answers a query about a project
// you may not see with an error rather than with no issues, and the caller of
// this decides what that means, because it means different things to a sync and
// to a sweep.
func (s *Service) search(ctx context.Context, c threadsource.Container, jql, want string, fn func(context.Context, issue) error) error {
	type results struct {
		window
		Issues []issue `json:"issues"`
	}

	var stopped error
	err := pages(ctx, s, "/rest/api/3/search", url.Values{
		"jql":    {jql},
		"fields": {want},
	}, func(page results) (int, bool) {
		for _, is := range page.Issues {
			if err := fn(ctx, is); err != nil {
				stopped = err
				return len(page.Issues), false
			}
		}
		return len(page.Issues), true
	})
	if stopped != nil {
		return stopped
	}
	return err
}

// Read fetches one issue by its key.
func (s *Service) Read(ctx context.Context, id string) (threadsource.Thread, error) {
	if !looksLikeKey(id) {
		return threadsource.Thread{}, fmt.Errorf("%w: %q", errBadKey, id)
	}

	var is issue
	err := s.call(ctx, "/rest/api/3/issue/"+url.PathEscape(id), url.Values{
		"fields": {fields},
	}, &is)
	switch {
	case err == nil:
	case missing(err):
		// Deleted, or moved somewhere this token cannot follow. From the
		// index's point of view those are the same event.
		return threadsource.Thread{}, connector.ErrGone
	default:
		return threadsource.Thread{}, err
	}

	return s.build(ctx, is)
}

// build turns an issue into the conversation the crawl above wants.
func (s *Service) build(ctx context.Context, is issue) (threadsource.Thread, error) {
	all, err := s.comments(ctx, is)
	if err != nil {
		return threadsource.Thread{}, err
	}

	replies := make([]thread.Message, 0, len(all))
	for _, c := range all {
		replies = append(replies, thread.Message{
			ID:     c.ID,
			Author: person(c.Author, s.name),
			At:     stamp(c.Created),
			Edited: edited(c),
			Text:   adf.Render(c.Body),
		})
	}

	conv := thread.Conversation{
		ID:    is.Key,
		Kind:  doc.KindTicket,
		Title: is.Key + " " + strings.TrimSpace(is.Fields.Summary),
		URL:   s.browseURL(is.Key),
		Root: thread.Message{
			ID: is.Key,
			// The reporter rather than the creator. On a site with a service
			// desk in front of it the creator is the automation that filed the
			// ticket and the reporter is the person it is about, and the person
			// is who a search for their tickets means.
			Author: person(reporterOf(is), s.name),
			At:     stamp(is.Fields.Created),
			Text:   adf.Render(is.Fields.Description),
		},
		Replies:    replies,
		Revision:   is.Fields.Updated,
		Properties: properties(is),
	}

	th := threadsource.Thread{
		Conversation: conv,
		Container:    projectOf(is.Key),
		Updated:      stamp(is.Fields.Updated),
	}

	// The security level, if there is one, replaces the project's rule rather
	// than adding to it. That is the whole point of a security level, and it is
	// the reason the crawl above lets a conversation override its container.
	if sec := is.Fields.Security; sec != nil && sec.ID != "" {
		th.Access = s.levels.level(ctx, sec.ID, sec.Name)
	}

	return th, nil
}

// comments reads the argument underneath a ticket.
//
// A search returns the first page of them inline, which is most issues in one
// request rather than two, and the rest are paged for separately. Asking for
// them unconditionally would be a request per issue in the site to be told what
// we were already holding.
func (s *Service) comments(ctx context.Context, is issue) ([]comment, error) {
	inline := is.Fields.Comment
	if inline == nil {
		return nil, nil
	}
	got := inline.Comments
	if inline.Total <= len(got) {
		return got, nil
	}

	type listing struct {
		window
		Comments []comment `json:"comments"`
	}
	var all []comment
	err := pages(ctx, s, "/rest/api/3/issue/"+url.PathEscape(is.Key)+"/comment", url.Values{
		"orderBy": {"created"},
	}, func(page listing) (int, bool) {
		all = append(all, page.Comments...)
		return len(page.Comments), true
	})
	switch {
	case err == nil:
		return all, nil
	case missing(err):
		// The issue went away between the search and this request, which is a
		// race every crawl has. What is in hand is still a ticket worth
		// indexing, and the sweep removes it if it really is gone.
		return got, nil
	default:
		return nil, err
	}
}

// properties are the fields a person filters on rather than reads.
//
// The assignee is here rather than being a second author because "tickets Sam
// reported" and "tickets Sam is working on" are different questions, and an
// index that conflated them would answer neither.
func properties(is issue) map[string]string {
	out := make(map[string]string, 6)
	if f := is.Fields.Status; f != nil && f.Name != "" {
		out["status"] = f.Name
	}
	if f := is.Fields.IssueType; f != nil && f.Name != "" {
		out["type"] = f.Name
	}
	if f := is.Fields.Priority; f != nil && f.Name != "" {
		out["priority"] = f.Name
	}
	if a := is.Fields.Assignee; a != nil && a.DisplayName != "" {
		out["assignee"] = a.DisplayName
	}
	if len(is.Fields.Labels) > 0 {
		out["labels"] = strings.Join(is.Fields.Labels, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// person turns a Jira account into who a document says wrote something.
//
// The account id is the identity and the display name is a label, and they are
// kept apart on purpose. A permission rule written against a display name is a
// permission rule anybody can grant themselves by renaming.
func person(a *account, source string) doc.Person {
	if a == nil || a.AccountID == "" {
		return doc.Person{}
	}
	name := a.DisplayName
	if name == "" {
		name = a.AccountID
	}
	return doc.Person{
		Subject:  a.AccountID,
		Identity: acl.Identity{Source: source, Value: a.AccountID},
		Name:     name,
		Email:    a.Email,
	}
}

// reporterOf is who the ticket is by, falling back to who filed it.
func reporterOf(is issue) *account {
	if is.Fields.Reporter != nil {
		return is.Fields.Reporter
	}
	return is.Fields.Creator
}

// edited is when a comment was last changed, and nothing when it never was.
func edited(c comment) time.Time {
	at, was := stamp(c.Updated), stamp(c.Created)
	if at.After(was) {
		return at
	}
	return time.Time{}
}

// browseURL is where a person goes to read the ticket at the source.
func (s *Service) browseURL(key string) string {
	return s.base + "/browse/" + url.PathEscape(key)
}

// projectOf is the project key an issue key belongs to, which is everything in
// front of the dash.
func projectOf(key string) string {
	k, _, _ := strings.Cut(key, "-")
	return k
}

// errBadKey is what an id that this source could never have produced comes back
// as, which is different from an id it produced and no longer has.
var errBadKey = errors.New("jirasource: not an issue key")

// looksLikeKey reports whether an id is shaped like a Jira issue key, which is
// a project key, a dash and a number.
//
// This is a check on our own ids rather than a validation of Jira's, and the
// reason it is here is that the alternative is putting whatever the caller
// passed into a URL path.
func looksLikeKey(id string) bool {
	project, number, ok := strings.Cut(id, "-")
	if !ok || project == "" || number == "" {
		return false
	}
	for _, r := range project {
		upper := r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !upper && !digit && r != '_' {
			return false
		}
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// quote puts a project key into a JQL query.
//
// Jira accepts a bare key and this quotes it anyway, because a key is
// configurable, one of them will one day be a JQL keyword, and a query that
// broke on the day somebody made a project called ORDER is not a thing to debug
// twice.
func quote(key string) string {
	return `"` + strings.ReplaceAll(key, `"`, `\"`) + `"`
}
