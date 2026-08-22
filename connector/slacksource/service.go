package slacksource

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/connector/threadsource"
	"github.com/tamnd/genba/doc"
)

// DefaultACLRefresh is how often a private channel's membership is reapplied to
// the documents in it even though nothing said it had changed.
//
// Slack moves a channel's updated field when the channel is renamed, archived
// or converted between public and private, and leaves it alone when somebody is
// removed from a private one. That last case is the one that matters, because
// it is a revocation, and a revocation that reaches the index whenever somebody
// happens to post is not a revocation.
//
// So the rule is reapplied on a schedule as well. A day is the default: it
// bounds how long a removed member keeps seeing a private channel's threads in
// their results, and it costs one write per thread in the private channels once
// a day rather than a read of any of them.
const DefaultACLRefresh = 24 * time.Hour

// WithACLRefresh sets how often a private channel's membership is reapplied.
// Zero selects [DefaultACLRefresh].
func WithACLRefresh(d time.Duration) Option {
	return func(s *Service) { s.aclRefresh = d }
}

// Containers lists the channels this token can see, public and private, with
// who may read each.
//
// Direct messages and group messages are not asked for. There is no rule that
// makes one safe to put in a shared index, and skipping them at the request is
// better than filtering them afterwards, because a filter is a thing that can
// be got wrong once and then be wrong for ever.
func (s *Service) Containers(ctx context.Context) ([]threadsource.Container, error) {
	form := url.Values{
		"types":            {"public_channel,private_channel"},
		"exclude_archived": {"true"},
		"limit":            {strconv.Itoa(s.page)},
	}

	type listing struct {
		Channels []channel `json:"channels"`
	}
	var found []channel
	if err := pages(ctx, s, "conversations.list", form, func(page listing) bool {
		found = append(found, page.Channels...)
		return true
	}); err != nil {
		return nil, err
	}

	containers := make([]threadsource.Container, 0, len(found))
	for _, ch := range found {
		if ch.IsIM || ch.IsMPIM || ch.IsArchived {
			continue
		}
		access, at, err := s.access(ctx, ch)
		if err != nil {
			return nil, err
		}
		containers = append(containers, threadsource.Container{
			ID:       ch.ID,
			Name:     ch.Name,
			Access:   access,
			AccessAt: at,
		})
	}
	return containers, nil
}

// access works out who may read a channel, and when that answer last moved.
func (s *Service) access(ctx context.Context, ch channel) (acl.Permissions, time.Time, error) {
	// Slack reports the channel's own last change in milliseconds, and a
	// channel that has never been touched since it was made reports nothing.
	var at time.Time
	if ch.Updated > 0 {
		at = time.UnixMilli(ch.Updated).UTC()
	}

	if !ch.IsPrivate {
		// Everybody in the workspace may read a public channel whether or not
		// they are in it, which is the definition of one.
		return acl.Permissions{
			Mode:    acl.ModePublicToTenant,
			Source:  s.name,
			Version: uint64(ch.Updated),
		}, at, nil
	}

	members, err := s.members(ctx, ch.ID)
	switch {
	case err == nil:
	case refused(err, "not_in_channel"), refused(err, "channel_not_found"):
		// The token can see that the channel exists and cannot see who is in
		// it, so there is no answer to give. Quarantining is the only safe
		// thing and staying quiet about it is the only unsafe one.
		s.skip(ch.ID, fmt.Errorf("who may read #%s: %w", ch.Name, err))
		// No schedule is applied here. Reapplying a rule means listing the
		// channel, this token cannot list it, and there is nothing in the index
		// from it to reapply anything to: the same refusal keeps its
		// conversations out in the first place.
		return connector.Unresolved(s.name, fmt.Sprintf("who may read #%s: %s", ch.Name, err)), at, nil
	default:
		return acl.Permissions{}, time.Time{}, err
	}

	allow := make([]acl.Ref, 0, len(members))
	for _, m := range members {
		allow = append(allow, acl.Ref{Source: s.name, Value: m})
	}
	return acl.Permissions{
		Mode:       acl.ModeACL,
		Source:     s.name,
		AllowUsers: allow,
		Version:    uint64(ch.Updated),
	}, s.aclTick(at), nil
}

// aclTick raises a private channel's rule time to the start of the current
// refresh interval, which is what makes the schedule in [DefaultACLRefresh]
// happen.
//
// The interval is quantised rather than measured from the last sync so that the
// answer does not depend on when the syncs happened to run. Two servers reading
// the same workspace an hour apart agree about whether the rule has moved, and
// a sync every five minutes still only reapplies it once.
func (s *Service) aclTick(at time.Time) time.Time {
	every := s.aclRefresh
	if every <= 0 {
		every = DefaultACLRefresh
	}
	tick := s.now().UTC().Truncate(every)
	if tick.After(at) {
		return tick
	}
	return at
}

// members lists the people in a private channel.
func (s *Service) members(ctx context.Context, id string) ([]string, error) {
	form := url.Values{"channel": {id}, "limit": {strconv.Itoa(s.page)}}
	type listing struct {
		Members []string `json:"members"`
	}
	var all []string
	if err := pages(ctx, s, "conversations.members", form, func(page listing) bool {
		all = append(all, page.Members...)
		return true
	}); err != nil {
		return nil, err
	}
	// The order Slack lists members in is not something anybody promised, and a
	// list that reorders itself between syncs is a permission change that never
	// happened, written to every document in the channel.
	slices.Sort(all)
	return slices.Compact(all), nil
}

// Threads walks the conversations in a channel that changed at or after since.
//
// Slack has no endpoint for that. History is ordered by when a message was
// posted, and a reply to an old thread moves nothing but that thread's
// latest_reply, so the history is read back to the older of since and the reply
// window and each thread is judged on its newest message. What that misses is
// the reply to a thread older than the window, and the sweep is what catches it.
func (s *Service) Threads(ctx context.Context, c threadsource.Container, since time.Time, fn func(context.Context, threadsource.Thread) error) error {
	parents, err := s.history(ctx, c.ID, s.oldest(since))
	switch {
	case err == nil:
	case refused(err, "not_in_channel"), refused(err, "channel_not_found"):
		// A bot that is in the workspace but not in the channel. There is
		// nothing to index and nothing that went wrong with the run, and the
		// only thing worse than not indexing it is not saying so.
		s.skip(c.ID, fmt.Errorf("reading #%s: %w", c.Name, err))
		return nil
	default:
		return err
	}

	changed := make([]message, 0, len(parents))
	for _, m := range parents {
		if !changedAt(m).Before(since) {
			changed = append(changed, m)
		}
	}
	// Oldest change first, which is what the cursor above this is built on: a
	// run that stopped halfway has to have emitted everything before the point
	// it reached.
	slices.SortFunc(changed, func(a, b message) int {
		if d := changedAt(a).Compare(changedAt(b)); d != 0 {
			return d
		}
		return strings.Compare(a.TS, b.TS)
	})

	for _, m := range changed {
		th, err := s.assemble(ctx, c.ID, m)
		if err != nil {
			if refused(err, "thread_not_found") || refused(err, "message_not_found") {
				// Deleted between the listing and the read, which is a race
				// every crawl has and not a reason to fail the channel. The
				// sweep removes it from the index.
				s.skip(s.threadID(c.ID, m.TS), err)
				continue
			}
			return err
		}
		if err := fn(ctx, th); err != nil {
			return err
		}
	}
	return nil
}

// oldest is how far back a sync reads.
//
// A first sync reads everything. Any other one reads back to the reply window
// as well as to the cursor, because a thread whose parent is inside the window
// may have been replied to since the cursor without moving.
func (s *Service) oldest(since time.Time) time.Time {
	if since.IsZero() {
		return time.Time{}
	}
	window := s.now().UTC().Add(-s.window)
	if window.Before(since) {
		return window
	}
	return since
}

// List reports every thread the channel currently holds, with a version derived
// from its newest message.
//
// This is the only thing that ever removes a deleted thread from the index, and
// the version is what repairs a thread whose late reply the change feed could
// not see.
func (s *Service) List(ctx context.Context, c threadsource.Container, fn func(connector.Item) bool) error {
	parents, err := s.history(ctx, c.ID, time.Time{})
	switch {
	case err == nil:
	case refused(err, "not_in_channel"), refused(err, "channel_not_found"):
		// Listing nothing here would be a claim that the channel is empty, and
		// the sweep above treats an empty channel as a channel whose documents
		// should all be deleted. So this is an error rather than a silence.
		return fmt.Errorf("slacksource: listing #%s: %w", c.Name, err)
	default:
		return err
	}
	for _, m := range parents {
		if !fn(connector.Item{
			ID:      s.threadID(c.ID, m.TS),
			Version: version(m),
		}) {
			return nil
		}
	}
	return nil
}

// Read fetches one thread by the id this adapter made for it.
func (s *Service) Read(ctx context.Context, id string) (threadsource.Thread, error) {
	ch, ts, ok := strings.Cut(id, ":")
	if !ok || ch == "" || ts == "" {
		return threadsource.Thread{}, fmt.Errorf("%w: %q", errBadID, id)
	}

	replies, err := s.replies(ctx, ch, ts)
	switch {
	case err == nil:
	case refused(err, "thread_not_found"), refused(err, "message_not_found"), refused(err, "channel_not_found"):
		return threadsource.Thread{}, connector.ErrGone
	default:
		return threadsource.Thread{}, err
	}
	if len(replies) == 0 {
		return threadsource.Thread{}, connector.ErrGone
	}

	th, err := s.build(ctx, ch, replies[0], replies[1:])
	if err != nil {
		return threadsource.Thread{}, err
	}
	return th, nil
}

// history reads a channel's top level messages back to oldest, newest first,
// which is the order Slack returns them in.
func (s *Service) history(ctx context.Context, id string, oldest time.Time) ([]message, error) {
	form := url.Values{
		"channel": {id},
		"limit":   {strconv.Itoa(s.page)},
		// Inclusive matters for the same reason the interface above asks for at
		// or after: a message posted in exactly the second the cursor names is
		// one an exclusive read loses for ever.
		"inclusive": {"true"},
		"oldest":    {slackTime(oldest)},
	}

	type listing struct {
		Messages []message `json:"messages"`
	}
	var found []message
	if err := pages(ctx, s, "conversations.history", form, func(page listing) bool {
		for _, m := range page.Messages {
			if top(m) {
				found = append(found, m)
			}
		}
		return true
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// replies reads a whole thread, parent first.
func (s *Service) replies(ctx context.Context, ch, ts string) ([]message, error) {
	form := url.Values{
		"channel": {ch},
		"ts":      {ts},
		"limit":   {strconv.Itoa(s.page)},
	}

	type listing struct {
		Messages []message `json:"messages"`
	}
	var found []message
	if err := pages(ctx, s, "conversations.replies", form, func(page listing) bool {
		found = append(found, page.Messages...)
		return true
	}); err != nil {
		return nil, err
	}
	// A paged reply listing repeats the parent on every page, and concatenating
	// the pages is how the root message ends up in the body three times.
	// [thread.Conversation] drops the repeat, and dropping it here as well
	// keeps the reply count in the document honest.
	return dedupe(found), nil
}

// assemble reads the replies to a parent message and builds the thread.
//
// A message with no replies is already whole, and asking Slack for the replies
// to it would be one request per message in the workspace to be told what we
// were already holding.
func (s *Service) assemble(ctx context.Context, ch string, parent message) (threadsource.Thread, error) {
	if parent.ReplyCount == 0 {
		return s.build(ctx, ch, parent, nil)
	}
	all, err := s.replies(ctx, ch, parent.TS)
	if err != nil {
		return threadsource.Thread{}, err
	}
	if len(all) == 0 {
		return s.build(ctx, ch, parent, nil)
	}
	return s.build(ctx, ch, all[0], all[1:])
}

// build turns a parent and its replies into what the crawl above wants.
func (s *Service) build(ctx context.Context, ch string, parent message, replies []message) (threadsource.Thread, error) {
	msgs := make([]thread.Message, 0, len(replies))
	for _, m := range replies {
		msgs = append(msgs, s.message(ctx, m))
	}

	// The replies in hand are counted as well as the parent's own account of
	// them. Both agree on a healthy workspace, and when they do not it is
	// because the parent arrived without its thread fields, which is a thread
	// that would otherwise report itself as older than a reply this run is
	// holding and be dropped for being before the cursor.
	at := changedAt(parent)
	for _, m := range replies {
		if r := stamp(m.TS); r.After(at) {
			at = r
		}
	}
	if len(replies) > parent.ReplyCount {
		parent.ReplyCount = len(replies)
	}

	conv := thread.Conversation{
		ID:       s.threadID(ch, parent.TS),
		Kind:     doc.KindMessage,
		Title:    title(parent.Text),
		URL:      s.permalink(ch, parent.TS),
		Root:     s.message(ctx, parent),
		Replies:  msgs,
		Revision: revision(at, parent.ReplyCount),
	}

	return threadsource.Thread{
		Conversation: conv,
		Container:    ch,
		Updated:      at,
	}, nil
}

// message turns one Slack message into one line of a conversation.
func (s *Service) message(ctx context.Context, m message) thread.Message {
	out := thread.Message{
		ID:     m.TS,
		Author: s.users.person(ctx, m),
		At:     stamp(m.TS),
		Text:   m.Text,
	}
	if m.Edited != nil {
		out.Edited = stamp(m.Edited.TS)
	}
	return out
}

// threadID is the id a thread is filed under, which has to name the channel
// because reading a thread back needs one.
func (s *Service) threadID(ch, ts string) string { return ch + ":" + ts }

// permalink is where a person goes to read the thread at the source.
//
// It is built rather than asked for. chat.getPermalink is a request per thread
// for a string with no information in it that we do not already have, and the
// shape of a Slack archive link has not changed in a decade.
func (s *Service) permalink(ch, ts string) string {
	return fmt.Sprintf("https://slack.com/archives/%s/p%s", ch, strings.ReplaceAll(ts, ".", ""))
}

// top reports whether a message is the start of a thread rather than part of
// one.
//
// The two that are not are a reply, which has a thread_ts pointing at somebody
// else, and the noise Slack posts about the channel itself: joins, leaves,
// renames and pinned messages. Indexing "Mei has joined the channel" as a
// document is how a search for a person's name returns four hundred results
// they did not write.
func top(m message) bool {
	if m.Type != "" && m.Type != "message" {
		return false
	}
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		return false
	}
	switch m.Subtype {
	case "", "bot_message", "me_message", "file_share":
		return strings.TrimSpace(m.Text) != ""
	default:
		return false
	}
}

// changedAt is when a thread last changed, which is the newest of the parent
// being posted, the parent being edited and the last reply arriving.
func changedAt(m message) time.Time {
	at := stamp(m.TS)
	if m.Edited != nil {
		if e := stamp(m.Edited.TS); e.After(at) {
			at = e
		}
	}
	if r := stamp(m.LatestReply); r.After(at) {
		at = r
	}
	return at
}

// version is the source's own idea of a revision, and it is what the sweep
// compares. It has to move when anything in the thread moves, which is why it
// is not simply the parent's timestamp.
func version(m message) string {
	return revision(changedAt(m), m.ReplyCount)
}

// revision is the same string built from a time and a count that have already
// been worked out, so that a listing and a read of the same thread agree.
func revision(at time.Time, replies int) string {
	return fmt.Sprintf("%s/%d", slackTime(at), replies)
}

// title is the first line of the first message, shortened.
//
// A Slack thread has no title, and a document with none is a search result with
// nothing on it but a snippet.
func title(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	const most = 80
	if len(line) <= most {
		return line
	}
	// Cut at a word so that the title does not end mid word, and only if there
	// is a word boundary anywhere near the end.
	cut := strings.LastIndex(line[:most], " ")
	if cut < most/2 {
		cut = most
	}
	return strings.TrimSpace(line[:cut])
}

// dedupe drops the repeats a paged listing produces, keeping the first of each.
func dedupe(msgs []message) []message {
	seen := make(map[string]struct{}, len(msgs))
	out := msgs[:0]
	for _, m := range msgs {
		if _, ok := seen[m.TS]; ok {
			continue
		}
		seen[m.TS] = struct{}{}
		out = append(out, m)
	}
	return out
}
