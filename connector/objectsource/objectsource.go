// Package objectsource is a connector that reads an S3 compatible bucket.
//
// It is the second reference connector and the first one that talks to a
// network service, which is where most of what a real connector has to get
// right lives: paging, signing, a source that answers slowly, and a listing
// that is ordered by key rather than by when anything changed.
//
// The protocol is S3's and the services that speak it are many. Amazon's own,
// MinIO, Ceph, Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces and
// several others all answer to ListObjectsV2 and GetObject signed with
// signature version 4, which is why this is one connector rather than eight.
// What differs between them is where the bucket goes in the URL, which is
// [Config.PathStyle], and whether they keep access control lists at all.
//
// # Permissions come from a policy
//
// The connector maps objects to documents. It does not decide who may read
// them, for the same reason the filesystem connector does not: a bucket says
// very little about access on its own. Access control lists on objects are off
// by default on new buckets and the real answer is usually in an identity
// policy naming roles this index has never heard of, so [Policy] is where that
// answer is plugged in.
//
// There is no permissive default. A source built without a policy quarantines
// everything, which is loud and safe, rather than publishing a bucket to
// everybody, which is quiet and not.
//
// # Prefix scoping
//
// A bucket is not usually all one thing. [WithPrefix] narrows a source to one
// part of one, and it narrows the listing rather than filtering it afterwards,
// so a source pointed at reports/ in a bucket of a hundred million objects
// costs what reports/ costs. Several sources can read the same bucket under
// different prefixes with different policies, which is how a bucket where one
// prefix is public and another is not gets indexed correctly.
//
// # Incremental sync
//
// The cursor is the newest modification time the last run saw. A later run
// still lists everything under the prefix, because ListObjectsV2 is ordered by
// key and there is no change feed to ask instead, and it fetches only what is
// newer. That is the same trade the filesystem connector makes: the listing
// cannot be avoided, the reads can, and the listing is a thousand objects per
// request against one request per object.
//
// A run that is interrupted resumes from the key it got to rather than from the
// beginning, because the listing is ordered by key and that makes the last key
// emitted a safe place to carry on from.
//
// An object whose content did not change can still have had its permissions
// rewritten, and nothing in a listing says so. A [Policy] that can say when its
// answer last changed turns that into a permission change carrying no body,
// which is how a revocation reaches the index without the bucket being read
// again.
package objectsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // registers the GIF config decoder
	_ "image/jpeg" // registers the JPEG config decoder
	_ "image/png"  // registers the PNG config decoder
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/extract"
)

// DefaultMaxObjectSize is the largest object read into a document body.
//
// Past this it is almost never prose somebody wants to search. A bucket is a
// place people put build artefacts and database dumps, so the limit matters
// more here than it does on a source tree.
const DefaultMaxObjectSize = 1 << 20

// DefaultMaxImageSize is the largest image read into a document's content. It
// is larger than the body limit because the two are about different things: a
// one megabyte text file is almost certainly not prose and a one megabyte
// screenshot is a screenshot.
const DefaultMaxImageSize = 4 << 20

// DefaultMaxDocumentSize is the largest PDF or Office file read for extraction.
// The bytes are compressed and the text inside them is a fraction of them, so
// the number that actually bounds the work is [extract.Options.MaxDecompressed]
// rather than this one.
const DefaultMaxDocumentSize = 16 << 20

// Source reads documents out of one bucket, or out of one prefix of one.
type Source struct {
	client *Client
	name   string
	prefix string
	policy Policy

	pageSize  int
	maxSize   int64
	maxImage  int64
	maxDoc    int64
	includeIf func(key string) bool
	skipped   func(key string, reason error)
}

// Option configures a source.
type Option func(*Source)

// WithPrefix narrows the source to the objects whose key starts with p.
//
// It is the prefix the listing is made with, not a filter applied to the
// results, so it costs nothing to scope a source tightly inside a very large
// bucket. It is matched exactly as the API matches it, which means a prefix of
// "docs" also covers "docs-archive/", and a prefix meant to be a folder should
// end in a slash.
func WithPrefix(p string) Option {
	return func(s *Source) { s.prefix = strings.TrimPrefix(p, "/") }
}

// WithMaxObjectSize sets the largest object that will be read. A value below
// one selects [DefaultMaxObjectSize].
func WithMaxObjectSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxSize = n
		}
	}
}

// WithMaxImageSize sets the largest image read into a document's content. A
// value below one selects [DefaultMaxImageSize].
func WithMaxImageSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxImage = n
		}
	}
}

// WithMaxDocumentSize sets the largest PDF or Office file read for extraction.
// A value below one selects [DefaultMaxDocumentSize].
func WithMaxDocumentSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxDoc = n
		}
	}
}

// WithPageSize sets how many objects one listing asks for. A value below one
// selects [DefaultPageSize].
func WithPageSize(n int) Option {
	return func(s *Source) {
		if n > 0 {
			s.pageSize = n
		}
	}
}

// WithInclude replaces the rule for which keys are read.
//
// The default reads the formats the extractor understands and the images a
// preview can show, decided from the key's extension. That is a guess about a
// name, and it is made before anything has been fetched on purpose: the
// alternative in a bucket is downloading a terabyte of archives to find out
// none of them were documents.
func WithInclude(f func(key string) bool) Option {
	return func(s *Source) {
		if f != nil {
			s.includeIf = f
		}
	}
}

// WithSkipped installs a callback for objects the sync passed over.
//
// A sync does not abandon a bucket because one object in it could not be read.
// What it must not be is silent: an index quietly missing everything nobody
// could read looks exactly like an index that is complete, and the difference
// only shows up when somebody cannot find a document they know exists. This is
// how a caller finds out, and the default does nothing.
func WithSkipped(f func(key string, reason error)) Option {
	return func(s *Source) {
		if f != nil {
			s.skipped = f
		}
	}
}

// New returns a source reading c's bucket, naming itself name, and asking
// policy who may read each object.
//
// A nil policy is allowed and quarantines every document. That is deliberate:
// it makes "I have not thought about permissions yet" a visible state in the
// stats rather than an invisible one in the index.
func New(c *Client, name string, policy Policy, opts ...Option) (*Source, error) {
	if c == nil {
		return nil, errors.New("objectsource: nil client")
	}
	if name == "" {
		return nil, errors.New("objectsource: empty source name")
	}

	s := &Source{
		client:    c,
		name:      name,
		policy:    policy,
		pageSize:  DefaultPageSize,
		maxSize:   DefaultMaxObjectSize,
		maxImage:  DefaultMaxImageSize,
		maxDoc:    DefaultMaxDocumentSize,
		includeIf: defaultInclude,
		skipped:   func(string, error) {},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

var _ connector.Connector = (*Source)(nil)

// Source returns the connector's name.
func (s *Source) Source() string { return s.name }

// Close releases nothing. The HTTP client is shared and outlives the source,
// because the policy answering for this source is usually holding it too.
func (s *Source) Close() error { return nil }

// syncPoint is what this connector puts in a cursor.
//
// Since is how far the last completed run got through the store's modification
// times, and is what decides whether an object is fetched. After is the key an
// interrupted run reached, and is only set on the cursors carried by individual
// changes. A run that finishes clears it, so a cursor with a key in it is
// exactly the record of a run that did not.
//
// Perms is the same high water mark for permission changes, and it is a
// separate field because the two are read off different clocks and only one of
// them is held back. A modification time comes from the store and has a second
// of resolution, so Since is kept a second behind the store's clock, and a
// permission change is something this process noticed rather than something the
// store recorded. Folding the second into the first would mean either losing a
// write made in the same second or rewriting the permissions of every object in
// the bucket twice for every change, depending on which way the fold went.
type syncPoint struct {
	Since time.Time `json:"since"`
	Perms time.Time `json:"perms,omitempty"`
	After string    `json:"after,omitempty"`
}

// Sync lists the prefix and emits every object modified after the cursor.
func (s *Source) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	start, err := parseCursor(from)
	if err != nil {
		return connector.Cursor{}, err
	}

	// A policy that caches its answer is told the sync is starting, so that a
	// bucket whose access control list was rewritten since the last run is read
	// again. Without this a long running process would keep serving the list
	// the bucket had when it started, which is the failure mode where a
	// revocation appears to have been applied and has not.
	if r, ok := s.policy.(Reloader); ok {
		r.Reload()
	}

	var (
		highest = start.Since
		latest  = start.Perms
		clock   time.Time
		token   string
	)
	for {
		page, err := s.client.list(ctx, s.prefix, start.After, token, s.pageSize)
		if err != nil {
			return connector.Cursor{}, err
		}
		if page.at.After(clock) {
			clock = page.at
		}

		for _, o := range page.objects {
			if err := ctx.Err(); err != nil {
				return connector.Cursor{}, err
			}
			if !s.wanted(o) {
				continue
			}
			if o.LastModified.After(highest) {
				highest = o.LastModified
			}

			// Not After rather than Before, so an object written in the same
			// second as the cursor is not emitted again on every later run.
			if !start.Since.IsZero() && !o.LastModified.After(start.Since) {
				at, err := s.refresh(ctx, o, start, emit)
				if err != nil {
					return connector.Cursor{}, err
				}
				if at.After(latest) {
					latest = at
				}
				continue
			}

			// The cursor has to cover when the permissions last changed as well
			// as when the content did, because the next run compares both
			// against it. An error here is the policy's problem and read below
			// reports it.
			if at, err := s.changedAt(ctx, o.Key); err == nil && at.After(latest) {
				latest = at
			}

			document, err := s.read(ctx, o)
			if err != nil {
				s.skipped(o.Key, err)
				continue
			}
			if err := emit(ctx, connector.Change{
				Document: document,
				Cursor:   resume(start, o.Key),
			}); err != nil {
				return connector.Cursor{}, err
			}
		}

		if !page.more || page.next == "" {
			break
		}
		token = page.next
	}

	// The store's clock has a second of resolution and a listing of a large
	// bucket takes a great deal longer than that, so an object written later in
	// the same second as the newest one this run saw would be filed under a
	// time the cursor has already passed, and would never be read again.
	// Holding the cursor a second behind the store's own clock leaves that one
	// second to be looked at once more on the next run. It costs re-reading a
	// handful of objects on the run after a write, it costs nothing at all on a
	// bucket that has been quiet, and it buys never silently losing one.
	if !clock.IsZero() {
		if edge := clock.Add(-time.Second); highest.After(edge) {
			highest = edge
		}
	}
	if highest.IsZero() && latest.IsZero() {
		return from, nil
	}
	return cursorAt(syncPoint{Since: highest, Perms: latest}, highest), nil
}

// refresh emits a permission change for an object whose content did not change
// but whose access control list did, and returns when that rule last changed.
//
// This is the whole of "a revocation takes effect without a resync" on object
// storage. A list rewritten on the bucket changes who may read every object
// under it without any of them being touched, and a sync that only compares
// modification times sees nothing at all.
func (s *Source) refresh(ctx context.Context, o object, start syncPoint, emit func(context.Context, connector.Change) error) (time.Time, error) {
	at, err := s.changedAt(ctx, o.Key)
	if err != nil {
		// A policy that cannot answer for one object is not a reason to abandon
		// the bucket. It is reported, and the document keeps the access control
		// list it already had until something can say otherwise.
		s.skipped(o.Key, err)
		return time.Time{}, nil
	}
	if at.IsZero() || !at.After(start.Perms) {
		return at, nil
	}

	perms, err := s.permissions(ctx, o.Key)
	if err != nil {
		// The rule that used to govern this object stopped resolving.
		// Quarantining is the only safe reading: a document nobody can
		// currently say who may read is not one to keep serving on last week's
		// answer.
		perms = connector.Unresolved(s.name)
		s.skipped(o.Key, err)
	}
	return at, emit(ctx, connector.Change{
		Document:        doc.Document{ID: s.id(o.Key), Permissions: perms},
		PermissionsOnly: true,
		Cursor:          resume(start, o.Key),
	})
}

// wanted decides whether an object is part of the corpus, and reports the ones
// it turns away for a reason worth knowing about.
func (s *Source) wanted(o object) bool {
	// A key ending in a slash is the empty object a console writes when
	// somebody makes a folder. There is nothing in it and there never was.
	if o.Key == "" || strings.HasSuffix(o.Key, "/") {
		return false
	}
	if !s.includeIf(o.Key) {
		return false
	}
	if limit := s.limitFor(o.Key); o.Size > limit {
		s.skipped(o.Key, fmt.Errorf("%d bytes is over the limit of %d", o.Size, limit))
		return false
	}
	return true
}

// permissions asks the policy who may read an object.
//
// A nil policy is not an oversight and not an error. It is the state a source
// built without one is in, and the answer is that nothing resolved, which
// quarantines every document rather than publishing the bucket.
func (s *Source) permissions(ctx context.Context, key string) (acl.Permissions, error) {
	if s.policy == nil {
		return connector.Unresolved(s.name), nil
	}
	return s.policy.Permissions(ctx, key)
}

// changedAt asks the policy when the rule governing an object last changed, and
// returns the zero time for a policy that cannot say.
func (s *Source) changedAt(ctx context.Context, key string) (time.Time, error) {
	if p, ok := s.policy.(Versioned); ok {
		return p.ChangedAt(ctx, key)
	}
	return time.Time{}, nil
}

// limitFor is the size limit that applies to one key. An image gets the image
// limit, a format that has to be extracted gets the document limit, and
// everything else gets the body limit.
func (s *Source) limitFor(key string) int64 {
	switch {
	case imageMedia(key) != "":
		return s.maxImage
	case needsExtraction(extract.MediaByName(key)):
		return s.maxDoc
	default:
		return s.maxSize
	}
}

// read fetches one object and turns it into a document.
func (s *Source) read(ctx context.Context, o object) (doc.Document, error) {
	raw, fetched, err := s.client.get(ctx, o.Key, s.limitFor(o.Key))
	if err != nil {
		return doc.Document{}, err
	}
	return s.document(ctx, merge(o, fetched), raw)
}

// document turns bytes into a document.
func (s *Source) document(ctx context.Context, o object, raw []byte) (doc.Document, error) {
	var (
		media   = mediaOf(raw, o.Key)
		text    string
		title   string
		content *doc.Content
		pages   int
		partial bool
	)
	if strings.HasPrefix(media, "image/") {
		// An image has no body. Its key is what a query can match, which is how
		// somebody finds architecture.png by typing architecture, and the bytes
		// are what the preview shows.
		content = &doc.Content{Bytes: raw}
		content.Width, content.Height = pixels(raw)
	} else {
		out, err := extract.Extract(ctx, bytes.NewReader(raw), o.Key)
		if err != nil {
			// One unreadable object is one document skipped and reported. A
			// hostile PDF does not cost the sync the hundred thousand objects
			// after it.
			return doc.Document{}, err
		}
		text, title, pages, partial = out.Text, out.Title, out.Pages, out.Truncated
		if out.Media != "" {
			// What the bytes say beats what the key says, because a key is
			// whatever somebody typed when they uploaded.
			media = out.Media
		}
	}

	rel := s.relative(o.Key)
	if title == "" {
		title = path.Base(rel)
	}
	props := map[string]string{
		"key":         o.Key,
		"bucket":      s.client.Bucket(),
		"path":        rel,
		"extension":   strings.TrimPrefix(path.Ext(o.Key), "."),
		doc.MediaType: media,
		"size_bytes":  strconv.FormatInt(o.Size, 10),
	}
	if tag := strings.Trim(o.ETag, `"`); tag != "" {
		props["etag"] = tag
	}
	if o.StorageClass != "" {
		props["storage_class"] = o.StorageClass
	}
	if pages > 0 {
		props["pages"] = strconv.Itoa(pages)
	}
	if partial {
		// The body is a prefix of the document rather than the whole of it, and
		// a reader that cannot tell the difference will report a document as
		// missing a phrase that is in it.
		props["truncated"] = "true"
	}

	perms, err := s.permissions(ctx, o.Key)
	if err != nil {
		// The document keeps the unresolved descriptor and is quarantined by
		// the pipeline. Failing to answer is not permission to publish.
		perms = connector.Unresolved(s.name)
		s.skipped(o.Key, err)
	}

	return doc.Document{
		ID:        s.id(o.Key),
		Kind:      kindOf(media),
		Title:     title,
		Body:      text,
		URL:       s.client.url(o.Key),
		Container: containerOf(rel),
		// Object storage keeps one time and calls it the last modification. An
		// object that was overwritten has no record of when it first appeared,
		// so saying the two are the same is the honest answer rather than
		// leaving a creation date empty and having every result sort last.
		ModifiedAt:   o.LastModified,
		CreatedAt:    o.LastModified,
		SourceUpdate: o.version(),
		Permissions:  perms,
		Properties:   props,
		Content:      content,
	}, nil
}

// relative is a key with the source's prefix taken off, which is what a person
// reading a result row wants to see. The prefix is a deployment's decision
// about where in a bucket the corpus lives and is the same on every row.
func (s *Source) relative(key string) string {
	return strings.TrimPrefix(strings.TrimPrefix(key, s.prefix), "/")
}

// merge fills in from a fetched object whatever a listed one did not carry.
//
// Both paths into a document go through here. A sync has the listing's
// description and a repair after reconciliation has only the response headers,
// and the difference should not reach the document.
func merge(listed, fetched object) object {
	if listed.Key == "" {
		listed.Key = fetched.Key
	}
	if listed.LastModified.IsZero() {
		listed.LastModified = fetched.LastModified
	}
	if listed.ETag == "" {
		listed.ETag = fetched.ETag
	}
	if listed.Size == 0 {
		listed.Size = fetched.Size
	}
	if listed.StorageClass == "" {
		listed.StorageClass = fetched.StorageClass
	}
	return listed
}

// parseCursor reads a sync point out of a cursor.
func parseCursor(c connector.Cursor) (syncPoint, error) {
	if c.IsZero() {
		return syncPoint{}, nil
	}
	var p syncPoint
	if err := json.Unmarshal([]byte(c.Value), &p); err != nil {
		// A cursor this connector cannot read is one written by a different
		// version or a different connector. Refusing is better than silently
		// resyncing the whole bucket, which on a large one looks like a hang
		// and costs real money.
		return syncPoint{}, fmt.Errorf("objectsource: unreadable cursor %q: %w", c.Value, err)
	}
	return p, nil
}

// cursorAt encodes a sync point.
func cursorAt(p syncPoint, at time.Time) connector.Cursor {
	// The only way this fails is a time that cannot be formatted, and there is
	// no such time. An empty value would be read back as no cursor at all,
	// which resyncs the bucket rather than losing anything.
	raw, err := json.Marshal(p)
	if err != nil {
		return connector.Cursor{}
	}
	return connector.Cursor{Value: string(raw), Time: at}
}

// resume is the cursor carried by one change, and is where an interrupted run
// picks up.
//
// It holds the key rather than the time, and that is the whole reason it is
// safe. A listing is ordered by key and not by when anything changed, so the
// times attached to the changes go up and down as the run proceeds. A cursor
// that stored the time of the change just written would tell the next run to
// skip every object with an older time, and the ones further down the listing
// that had not been read yet would be skipped for ever.
//
// Both high water marks are carried through unchanged. They belong to the run
// that is still going and only move when it finishes, so a run resumed from
// here compares against exactly what it was comparing against before.
func resume(start syncPoint, key string) connector.Cursor {
	return cursorAt(syncPoint{Since: start.Since, Perms: start.Perms, After: key}, start.Since)
}

// mediaOf is what an object actually is.
//
// The bytes decide. A key is whatever somebody typed when they uploaded, and
// the content type the store reports is whatever their tool sent with it, which
// is application/octet-stream far more often than it is true. The name only
// gets a say for the formats that announce nothing in their first bytes, which
// is the vector image formats and every text format there is.
func mediaOf(raw []byte, key string) string {
	media := extract.Detect(raw, key)
	switch media {
	case "text/plain", "application/octet-stream":
		if m := imageMedia(key); m != "" {
			return m
		}
	}
	return media
}

// imageMedia is the image type a key claims, or empty.
//
// It is by name because that is all there is at the point it is asked, which is
// while deciding whether to fetch the object at all.
func imageMedia(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	default:
		return ""
	}
}

// needsExtraction reports whether a media type is one whose text has to be
// extracted rather than read.
func needsExtraction(media string) bool {
	switch media {
	case "application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return true
	default:
		return false
	}
}

// kindOf maps a media type onto a document kind. Anything unrecognised is a
// file, which is the kind that promises the least.
func kindOf(media string) doc.Kind {
	switch {
	case strings.HasPrefix(media, "image/"):
		return doc.KindImage
	case strings.HasPrefix(media, "text/x-"), media == "text/javascript":
		return doc.KindCode
	case strings.HasPrefix(media, "text/"):
		return doc.KindPage
	default:
		return doc.KindFile
	}
}

// containerOf is the folder an object lives in, relative to the prefix. An
// object at the top has no container rather than one called ".", because that
// dot travels all the way to the result row and to the facet list, where it
// means nothing to anybody.
func containerOf(rel string) string {
	if dir := path.Dir(rel); dir != "." && dir != "/" {
		return dir
	}
	return ""
}

// defaultInclude reads the formats the extractor understands and the images a
// preview can show.
func defaultInclude(key string) bool {
	return imageMedia(key) != "" || extract.MediaByName(key) != ""
}

// pixels reads the dimensions out of an encoded image without decoding it.
//
// The standard library answers for png, jpeg and gif. It does not answer for
// webp or svg, and rather than pull in a decoder for each, those record a zero,
// which the interface reads as no box to reserve.
func pixels(raw []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
