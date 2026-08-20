// Package thread turns a conversation into one document.
//
// Chat, ticket trackers and wikis all keep the same shape underneath the
// vocabulary. Something is said, and then people say things about it: a message
// and its replies, an issue and its comments, a page and the discussion at the
// bottom of it. The source stores those as separate rows because that is how
// they were written, one at a time, by different people on different days.
//
// An index that copies that shape is an index that answers badly. Ask it why
// the gearbox order was cancelled and it returns fourteen rows from the same
// conversation, ranked against each other, none of which is the answer on its
// own. The reply that says "supplier could not meet the date" scores nothing at
// all for the word gearbox, because the word gearbox was in the message above
// it and was never repeated. Worse, the fourteen rows crowd out the thirteen
// other conversations that should have been on the page.
//
// So a conversation is one document. It is one result, it ranks as a whole,
// and every word anybody said in it is a word that can find it.
//
// # What this package does not do
//
// It does not talk to anything. A connector fetches the messages, resolves who
// wrote them and works out who may read the result, and hands the pieces here.
// That is what makes this the one place the assembly is defined rather than the
// same assembly written three times with three sets of small differences
// nobody meant.
//
// It does not decide permissions either, and [Conversation.Document] takes them
// rather than filling them in. A conversation is assembled out of messages from
// a container whose access rules the connector had to read anyway, and a
// signature that lets the caller forget the answer is a signature that invites
// a thread being indexed without one.
package thread

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// DefaultMaxBody is how much of a conversation goes into a document body.
//
// It is generous because a conversation is the one thing in a corpus that has
// no natural size: a thread runs for as long as people keep replying, and the
// long ones are usually the ones worth finding. Past this the body stops
// growing and the document says so.
const DefaultMaxBody = 128 << 10

// TitleLimit is how long a title derived from the first message may get.
//
// Chat has no titles. A thread's title has to be made out of what somebody
// typed first, and what somebody typed first is sometimes four paragraphs.
const TitleLimit = 80

// Message is one thing somebody said.
type Message struct {
	// ID is the source's own identifier for the message. It has to be unique
	// within the conversation and stable across reads, because it is what
	// deduplicates a paged reply listing.
	ID string

	// Author is who wrote it, resolved as far as the connector managed. A
	// message whose author could not be resolved at all is still worth
	// indexing: the text is the part somebody is searching for.
	Author doc.Person

	// At is when it was written.
	At time.Time

	// Edited is when it was last changed, and is zero for a message nobody went
	// back to. It matters more than it looks: a message edited in place changes
	// what the document says without any reply being added, and a version
	// derived only from reply times would leave the index serving the old text
	// until somebody happened to answer.
	Edited time.Time

	// Text is what was said, as plain text. Turning a source's own markup into
	// this is the connector's job, because every one of them has a different
	// one.
	Text string
}

// last is when this message last changed.
func (m Message) last() time.Time {
	if m.Edited.After(m.At) {
		return m.Edited
	}
	return m.At
}

// Conversation is a root message and everything said in reply to it.
type Conversation struct {
	// ID is the document id, already in whatever form the connector files
	// documents under. This package does not build it, because the shape of an
	// id is the connector's business and every one of them prefixes its own
	// name.
	ID string

	// Kind is the document kind, and defaults to [doc.KindMessage].
	Kind doc.Kind

	// Title is the source's own title, for the sources that have one. An issue
	// has a summary and a wiki page has a heading. Chat has neither, and a
	// conversation with no title gets one made out of its first message.
	Title string

	// Container is the channel, project or space the conversation lives in.
	Container string

	// URL links to the conversation at the source.
	URL string

	// Root is the message the conversation is about.
	Root Message

	// Replies is everything said after it, in any order. Duplicates are
	// dropped, which is not a nicety: a paged reply listing at more than one
	// source repeats the parent message on every page, so a connector that
	// concatenates the pages hands over the root three times.
	Replies []Message

	// Revision is the source's own version of the conversation, for a source
	// that keeps one. A source that does not leaves it empty and gets a version
	// derived from when the conversation last changed.
	Revision string

	// Properties are the source specific fields worth faceting on, such as a
	// ticket status. They are copied into the document and anything this
	// package sets is only set if the caller did not.
	Properties map[string]string
}

// Option adjusts how a conversation is turned into a document.
type Option func(*options)

type options struct {
	maxBody int
}

// WithMaxBody sets how much of a conversation goes into the body. A value below
// one selects [DefaultMaxBody].
func WithMaxBody(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxBody = n
		}
	}
}

// Document assembles the conversation into one document that perm governs.
//
// Everything the conversation knows is filled in. The tenant and the source
// name are not, because those belong to the run and to the connector rather
// than to the conversation.
func (c Conversation) Document(perm acl.Permissions, opts ...Option) doc.Document {
	o := options{maxBody: DefaultMaxBody}
	for _, opt := range opts {
		opt(&o)
	}

	msgs, rooted := c.ordered()
	body, omitted := assemble(msgs, o.maxBody)

	replies := len(msgs)
	if rooted {
		replies--
	}

	kind := c.Kind
	if kind == "" {
		kind = doc.KindMessage
	}

	props := make(map[string]string, len(c.Properties)+5)
	for k, v := range c.Properties {
		props[k] = v
	}
	setIfAbsent(props, doc.MediaType, "text/plain")
	setIfAbsent(props, "messages", strconv.Itoa(len(msgs)))
	setIfAbsent(props, "replies", strconv.Itoa(replies))
	setIfAbsent(props, "participants", strconv.Itoa(participants(msgs)))
	if omitted > 0 {
		// The body is part of the conversation rather than the whole of it. A
		// reader that cannot tell the difference will report a thread as
		// missing a sentence that is in it.
		setIfAbsent(props, "truncated", "true")
		setIfAbsent(props, "omitted_messages", strconv.Itoa(omitted))
	}

	created, modified := span(msgs)
	revision := c.Revision
	if revision == "" && !modified.IsZero() {
		// A conversation whose source keeps no version of its own still needs
		// one, or an incremental sync has no way to tell a thread that gained a
		// reply from one that did not. When the last thing in it happened is
		// the honest answer and it moves for an edit as well as for a reply.
		revision = modified.UTC().Format(time.RFC3339Nano)
	}

	return doc.Document{
		ID:           c.ID,
		Kind:         kind,
		Title:        c.title(),
		Body:         body,
		URL:          c.URL,
		Author:       c.Root.Author,
		Container:    c.Container,
		CreatedAt:    created,
		ModifiedAt:   modified,
		SourceUpdate: revision,
		Permissions:  perm,
		Properties:   props,
	}
}

// ordered is every message in the conversation, root first and replies in the
// order they were written.
//
// The root is pinned to the front rather than sorted with the rest because it
// is the front whatever its timestamp says. Sources do move it: a ticket
// description edited after the first comment carries a later time than the
// comment, and an imported conversation carries whatever time the import wrote.
func (c Conversation) ordered() (msgs []Message, rooted bool) {
	out := make([]Message, 0, len(c.Replies)+1)
	seen := make(map[string]bool, len(c.Replies)+1)

	rooted = !c.Root.empty()
	if rooted {
		out = append(out, c.Root)
		seen[c.Root.ID] = true
	}
	for _, m := range c.Replies {
		if m.empty() {
			continue
		}
		// A message with no id cannot be deduplicated and is kept, because
		// dropping it would lose content over a field the source did not fill
		// in. A message with one is kept once.
		if m.ID != "" {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
		}
		out = append(out, m)
	}

	replies := out
	if rooted {
		replies = out[1:]
	}
	slices.SortStableFunc(replies, func(a, b Message) int {
		if !a.At.Equal(b.At) {
			return a.At.Compare(b.At)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, rooted
}

// empty reports whether a message carries nothing worth keeping.
func (m Message) empty() bool {
	return strings.TrimSpace(m.Text) == "" && m.ID == "" && m.At.IsZero()
}

// assemble renders the messages into a body, and reports how many of them did
// not fit.
//
// What gets dropped is the middle. The root stays because it is what the
// conversation is about, and the end stays because that is where the answer is:
// a thread long enough to be cut is usually one that was cut because it took a
// while to work something out, and the working out is at the bottom. Nothing is
// written in place of what was left out, because a marker in a body is a phrase
// in the index that nobody at the source ever typed.
func assemble(msgs []Message, maxBody int) (body string, omitted int) {
	rendered := make([]string, len(msgs))
	var have int
	for i, m := range msgs {
		rendered[i] = render(m)
		if rendered[i] != "" {
			have++
		}
	}

	keep := make([]bool, len(msgs))
	var size int
	if len(rendered) > 0 && rendered[0] != "" {
		keep[0] = true
		size = len(rendered[0])
	}
	for i := len(rendered) - 1; i > 0; i-- {
		if rendered[i] == "" {
			continue
		}
		// Two for the blank line between messages.
		next := size + len(rendered[i]) + 2
		if next > maxBody {
			break
		}
		keep[i] = true
		size = next
	}

	parts := make([]string, 0, have)
	var kept int
	for i, r := range rendered {
		if r == "" || !keep[i] {
			continue
		}
		parts = append(parts, r)
		kept++
	}

	body = strings.Join(parts, "\n\n")
	if len(body) > maxBody {
		// The first message on its own is over the limit, which the loop above
		// cannot do anything about because a conversation with no body at all
		// is worse than a conversation with a long first message.
		body = cut(body, maxBody)
	}
	return body, have - kept
}

// render is one message as a line of body text.
//
// The author's name goes in front of what they said, which puts it in the index
// as well as on the screen. That is the difference between being able to search
// for what Mei said about the gearbox and only being able to search for the
// gearbox.
func render(m Message) string {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return ""
	}
	if who := display(m.Author); who != "" {
		return who + ": " + text
	}
	return text
}

// display is what to call somebody, in the order of how much it means to a
// person reading a result.
func display(p doc.Person) string {
	switch {
	case p.Name != "":
		return p.Name
	case p.Email != "":
		return p.Email
	default:
		return p.Identity.Value
	}
}

// title is the conversation's own title, or one made out of what was said
// first.
func (c Conversation) title() string {
	if t := strings.TrimSpace(c.Title); t != "" {
		return t
	}
	if t := summarise(c.Root.Text); t != "" {
		return t
	}
	// A conversation with no title and nothing readable in its first message is
	// usually one that opened with a file or an image. The container is a
	// worse title than the thread deserves and it is a great deal better than
	// an empty row.
	return c.Container
}

// summarise makes a title out of the first thing somebody typed.
func summarise(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.Join(strings.Fields(line), " ")
	if utf8.RuneCountInString(line) <= TitleLimit {
		return line
	}

	short := cutRunes(line, TitleLimit)
	// Cutting at the last space keeps the title from ending in half a word.
	// A first line with no space in it at all is a URL or a stack trace, and
	// there is nowhere better to cut it than where the room ran out.
	if i := strings.LastIndex(short, " "); i > TitleLimit/2 {
		short = short[:i]
	}
	return strings.TrimRight(short, " ,.;:") + "..."
}

// span is when the conversation started and when it last changed.
func span(msgs []Message) (created, modified time.Time) {
	for _, m := range msgs {
		if m.At.IsZero() {
			continue
		}
		if created.IsZero() || m.At.Before(created) {
			created = m.At
		}
		if last := m.last(); last.After(modified) {
			modified = last
		}
	}
	return created, modified
}

// participants is how many distinct people said something.
func participants(msgs []Message) int {
	seen := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if who := key(m.Author); who != "" {
			seen[who] = true
		}
	}
	return len(seen)
}

// key identifies a person for counting, preferring the identifiers a source
// keeps stable over the name somebody can change.
func key(p doc.Person) string {
	switch {
	case p.Subject != "":
		return "subject:" + p.Subject
	case p.Identity.Value != "":
		return "identity:" + p.Identity.Source + ":" + p.Identity.Value
	case p.Email != "":
		return "email:" + strings.ToLower(p.Email)
	default:
		return p.Name
	}
}

// cut truncates a string to n bytes without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutRunes truncates a string to n runes.
func cutRunes(s string, n int) string {
	var count int
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func setIfAbsent(m map[string]string, k, v string) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}
