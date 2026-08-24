package confluencesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/adf"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// expanded is what a crawl asks for on every page.
//
// It is written down rather than left as the default, because the default is a
// page with no body on it and the body is the document. Every name here is
// something that ends up in what is indexed or in who may read it, and the
// comments are on the list so that most pages are one request rather than two.
var expanded = strings.Join([]string{
	"body.atlas_doc_format",
	"version",
	"history",
	"space",
	"container",
	"ancestors",
	"metadata.labels",
	"restrictions.read.restrictions.user",
	"restrictions.read.restrictions.group",
	"children.comment.body.atlas_doc_format",
	"children.comment.version",
	"children.comment.history",
}, ",")

// Threads walks the pages in a space that changed at or after since.
//
// The query asks for comments as well as pages, and that is the interesting part
// of this adapter. Confluence dates a comment and does not date the page it is
// on, so a page that was answered and never edited again is a page that a query
// about pages alone reports as unchanged for ever. A comment that comes back is
// resolved to the page it is on and the page is what gets emitted, because a
// comment is a sentence in a document rather than a document.
func (s *Service) Threads(ctx context.Context, c threadsource.Container, since time.Time, fn func(context.Context, threadsource.Thread) error) error {
	cql := "(type = page or type = comment) and status = current and space = " + quote(c.ID)
	if !since.IsZero() {
		// At or after rather than after, because CQL compares to the minute and
		// a page changed in the same minute as the cursor is one an exclusive
		// query loses for ever. The cursor's edge set is what stops the repeat
		// from being emitted twice.
		cql += " and lastModified >= " + quote(cqlTime(since))
	}
	cql += " order by lastModified asc"

	// What has already been read during this walk, so that a page edited and
	// then commented on inside one sync window is fetched once. It is still
	// emitted twice, with the later time on the second, because that time is
	// what moves the cursor past the comment.
	built := make(map[string]threadsource.Thread)

	return s.search(ctx, cql, expanded, func(ctx context.Context, hit content) error {
		at := changed(hit)
		if at.Before(since) {
			// CQL rounds to the minute and this does not, so a query asked for
			// the minute the cursor is in comes back with what changed in the
			// first half of it as well. Dropping those here rather than widening
			// the query is what keeps the request cheap and the answer exact.
			return nil
		}

		id, err := s.pageOf(hit)
		if err != nil {
			return err
		}
		if id == "" {
			return nil
		}

		th, ok := built[id]
		if !ok {
			var err error
			switch hit.Type {
			case "page":
				th, err = s.build(ctx, hit)
			default:
				th, err = s.Read(ctx, id)
			}
			switch {
			case errors.Is(err, connector.ErrGone):
				// The page went away between the search and the read, which is a
				// race every crawl has. The sweep is what takes it out of the
				// index, and it is already there to do that.
				return nil
			case err != nil:
				return err
			}
			built[id] = th
		}

		th.Updated = at
		return fn(ctx, th)
	})
}

// pageOf is the page a search hit is about, which for a comment is the page it
// was written on.
func (s *Service) pageOf(hit content) (string, error) {
	if hit.Type == "page" {
		return hit.ID, nil
	}
	if hit.Container == nil || hit.Container.Type != "page" || hit.Container.ID == "" {
		// A comment on something that is not a page, or one whose container the
		// site declined to expand. Neither is a document and neither is an
		// error, and saying so out loud is what keeps it from looking like an
		// index that is complete.
		s.skip(hit.ID, fmt.Errorf("a %s came back from the page query with no page under it", hit.Type))
		return "", nil
	}
	return hit.Container.ID, nil
}

// listed is what a listing asks for, which is more than the version.
//
// The version is what the sweep compares. The restrictions and the ancestors are
// what keep a scheduled permission refresh from writing the space's rule over a
// page that was restricted out of the space, and they are asked for here because
// here is the one place they can be had without reading the pages: a refresh
// that had to read a space to find out which pages in it override the space is a
// recrawl, which is what the refresh exists instead of.
var listed = strings.Join([]string{
	"version",
	"ancestors",
	"restrictions.read.restrictions.user",
	"restrictions.read.restrictions.group",
}, ",")

// List reports every page the space currently holds, with the version the sweep
// compares and the rule the page has of its own.
//
// Nothing in CQL reports a page that was deleted, archived or moved to a space
// this token cannot see, so this is the only thing that ever takes one out of
// the index.
func (s *Service) List(ctx context.Context, c threadsource.Container, fn func(threadsource.Item) bool) error {
	cql := "type = page and status = current and space = " + quote(c.ID) + " order by id asc"
	err := s.search(ctx, cql, listed, func(ctx context.Context, p content) error {
		item := threadsource.Item{Item: connector.Item{ID: p.ID, Version: revision(p)}}
		if rule, restricted := s.restricted.rule(ctx, p); restricted {
			item.Access = rule
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
var errStop = errors.New("confluencesource: enough")

// search runs a CQL query and hands each result to fn.
func (s *Service) search(ctx context.Context, cql, want string, fn func(context.Context, content) error) error {
	var stopped error
	err := pages(ctx, s, "/rest/api/content/search", url.Values{
		"cql":    {cql},
		"expand": {want},
	}, func(results []content) bool {
		for _, hit := range results {
			if err := fn(ctx, hit); err != nil {
				stopped = err
				return false
			}
		}
		return true
	})
	if stopped != nil {
		return stopped
	}
	return err
}

// Read fetches one page by its id.
func (s *Service) Read(ctx context.Context, id string) (threadsource.Thread, error) {
	if !looksLikeID(id) {
		return threadsource.Thread{}, fmt.Errorf("%w: %q", errBadID, id)
	}

	var p content
	err := s.call(ctx, "/rest/api/content/"+url.PathEscape(id), url.Values{
		"expand": {expanded},
	}, &p)
	switch {
	case err == nil:
	case missing(err):
		// Deleted, archived, or moved somewhere this token cannot follow. From
		// the index's point of view those are the same event.
		return threadsource.Thread{}, connector.ErrGone
	default:
		return threadsource.Thread{}, err
	}

	if p.Status != "" && p.Status != "current" {
		// An archived page is still readable by id and is not a page the wiki
		// is offering anybody. Reporting it as gone is what takes it out.
		return threadsource.Thread{}, connector.ErrGone
	}
	return s.build(ctx, p)
}

// build turns a page into the conversation the crawl above wants.
func (s *Service) build(ctx context.Context, p content) (threadsource.Thread, error) {
	body, err := s.text(ctx, p)
	if err != nil {
		return threadsource.Thread{}, err
	}

	all, err := s.comments(ctx, p)
	if err != nil {
		return threadsource.Thread{}, err
	}
	replies := make([]thread.Message, 0, len(all))
	for _, c := range all {
		said, err := s.text(ctx, c)
		if err != nil {
			return threadsource.Thread{}, err
		}
		replies = append(replies, thread.Message{
			ID:     c.ID,
			Author: authorOf(createdBy(c), s.name),
			At:     created(c),
			Edited: edited(c),
			Text:   said,
		})
	}

	conv := thread.Conversation{
		ID:    p.ID,
		Kind:  doc.KindPage,
		Title: strings.TrimSpace(p.Title),
		URL:   s.webURL(p),
		Root: thread.Message{
			ID: p.ID,
			// Who wrote the page rather than who touched it last. A page a
			// hundred people have tidied is still the page its author wrote,
			// and who edited it last is a property for the people who want to
			// ask that instead.
			Author: authorOf(createdBy(p), s.name),
			At:     created(p),
			Edited: edited(p),
			Text:   body,
		},
		Replies:    replies,
		Revision:   revision(p),
		Properties: properties(p),
	}

	th := threadsource.Thread{
		Conversation: conv,
		Container:    spaceOf(p),
		Updated:      changed(p),
	}

	// A read restriction, if there is one anywhere above this page or on it,
	// replaces the space's rule rather than adding to it. That is what a
	// restriction is, and it is the reason the crawl above lets a conversation
	// override its container.
	access, restricted := s.restricted.rule(ctx, p)
	if restricted {
		th.Access = access
	}
	return th, nil
}

// comments reads the argument underneath a page.
//
// A search returns the first page of them inline, which is most pages in one
// request rather than two, and the rest are paged for separately. Asking for
// them unconditionally would be a request per page in the space to be told what
// we were already holding.
func (s *Service) comments(ctx context.Context, p content) ([]content, error) {
	if p.Children == nil || p.Children.Comment == nil {
		return nil, nil
	}
	inline := p.Children.Comment
	if inline.Links.Next == "" {
		return inline.Results, nil
	}

	var all []content
	err := pages(ctx, s, "/rest/api/content/"+url.PathEscape(p.ID)+"/child/comment", url.Values{
		"expand":   {"body.atlas_doc_format,version,history"},
		"depth":    {"all"},
		"location": {"footer,inline"},
	}, func(results []content) bool {
		all = append(all, results...)
		return true
	})
	switch {
	case err == nil:
		return all, nil
	case missing(err):
		// The page went away between the search and this request. What is in
		// hand is still worth indexing, and the sweep removes it if it really is
		// gone.
		return inline.Results, nil
	default:
		return nil, err
	}
}

// text is what a page or a comment says, as Markdown.
//
// The body arrives as an Atlassian document, which is [adf]'s job, and the one
// surprise is that it arrives as a string holding JSON rather than as JSON.
//
// A page written before the editor changed has no Atlassian document to give,
// and what it has instead is the storage format, which is XHTML. The site will
// send that too and it is not asked for on the crawl, because asking means the
// body of every page on a migrated site arriving twice. It is asked for one page
// at a time, only for the pages that came back with nothing, which costs one
// extra request per legacy page and one per genuinely empty page.
func (s *Service) text(ctx context.Context, c content) (string, error) {
	if b := c.Body.ADF; b != nil && strings.TrimSpace(b.Value) != "" {
		return adf.Render(json.RawMessage(b.Value)), nil
	}
	if b := c.Body.Storage; b != nil && strings.TrimSpace(b.Value) != "" {
		return storage(b.Value), nil
	}
	return s.legacy(ctx, c.ID)
}

// legacy asks for one body in the storage format.
func (s *Service) legacy(ctx context.Context, id string) (string, error) {
	if !looksLikeID(id) {
		return "", nil
	}

	var got content
	err := s.call(ctx, "/rest/api/content/"+url.PathEscape(id), url.Values{
		"expand": {"body.storage"},
	}, &got)
	switch {
	case err == nil:
	case missing(err):
		// It went away while we were reading it. An empty body is the right
		// answer here rather than a failed sync, because the sweep is what takes
		// the page out and it is already going to.
		return "", nil
	default:
		return "", err
	}

	if b := got.Body.Storage; b != nil {
		return storage(b.Value), nil
	}
	return "", nil
}

// properties are the fields a person filters on rather than reads.
//
// The last editor is here rather than being a second author because "pages Sam
// wrote" and "pages Sam last touched" are different questions, and an index that
// conflated them would answer neither.
func properties(p content) map[string]string {
	out := make(map[string]string, 2)
	if v := p.Version; v != nil && v.By != nil {
		if name := nameOf(v.By); name != "" {
			out["editor"] = name
		}
	}
	if m := p.Metadata; m != nil && m.Labels != nil {
		labels := make([]string, 0, len(m.Labels.Results))
		for _, l := range m.Labels.Results {
			if l.Name != "" {
				labels = append(labels, l.Name)
			}
		}
		if len(labels) > 0 {
			out["labels"] = strings.Join(labels, ", ")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// authorOf turns a Confluence account into who a document says wrote
// something.
//
// The account id is the identity and the display name is a label, and they are
// kept apart on purpose. A permission rule written against a display name is a
// permission rule anybody can grant themselves by renaming.
func authorOf(a *person, source string) doc.Person {
	if a == nil || a.AccountID == "" {
		return doc.Person{}
	}
	name := nameOf(a)
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

// nameOf is what to call somebody.
//
// The display name is the one a site shows, and the public name is what is left
// when a person has asked the site not to show the other one. Falling back to it
// is the difference between a search result attributed to somebody and one
// attributed to an account id.
func nameOf(a *person) string {
	if a == nil {
		return ""
	}
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.PublicName
}

// createdBy is who wrote something, which is on the history rather than on the
// version, because the version is who touched it last.
func createdBy(c content) *person {
	if c.History == nil {
		return nil
	}
	return c.History.CreatedBy
}

// created is when something was written.
func created(c content) time.Time {
	if c.History == nil {
		return time.Time{}
	}
	return stamp(c.History.CreatedDate)
}

// changed is when something was last touched, falling back to when it was
// written for the sites and the objects that do not say.
func changed(c content) time.Time {
	if at := versionAt(c); !at.IsZero() {
		return at
	}
	return created(c)
}

// edited is when something was last changed, and nothing when it never was.
func edited(c content) time.Time {
	at, was := versionAt(c), created(c)
	if at.After(was) {
		return at
	}
	return time.Time{}
}

func versionAt(c content) time.Time {
	if c.Version == nil {
		return time.Time{}
	}
	return stamp(c.Version.When)
}

// revision is the version the listing and the read have to agree on.
//
// It is the page's own version number and nothing else. A comment does not move
// it, which would be a hole if the sync did not ask about comments directly, and
// it does. Deriving it from the comments as well would mean the sweep expanding
// every comment on every page in the space to find out what it already knows.
func revision(p content) string {
	if p.Version == nil || p.Version.Number == 0 {
		return ""
	}
	return strconv.Itoa(p.Version.Number)
}

// spaceOf is the space key a page belongs to.
func spaceOf(p content) string {
	if p.Space == nil {
		return ""
	}
	return p.Space.Key
}

// webURL is where a person goes to read the page at the source.
//
// The site says where that is, because the address of a page has the title in it
// and a title is a thing people change. The fallback is the one address that
// never moves, which is worth having for a site behind something that does not
// send the link.
func (s *Service) webURL(p content) string {
	if p.Links.WebUI != "" {
		return s.wiki + p.Links.WebUI
	}
	return s.wiki + "/pages/viewpage.action?pageId=" + url.QueryEscape(p.ID)
}

// errBadID is what an id that this source could never have produced comes back
// as, which is different from an id it produced and no longer has.
var errBadID = errors.New("confluencesource: not a page id")

// looksLikeID reports whether an id is shaped like a Confluence content id,
// which is a number.
//
// This is a check on our own ids rather than a validation of Confluence's, and
// the reason it is here is that the alternative is putting whatever the caller
// passed into a URL path.
func looksLikeID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
