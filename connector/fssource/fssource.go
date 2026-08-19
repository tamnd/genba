// Package fssource is a connector that reads a directory tree.
//
// It is the reference connector, the one the interface was written against and
// the one to read first when writing another. It also does real work: pointed
// at a checkout of a documentation repository it produces a corpus with real
// titles, real bodies, real modification times and, with the right policy, real
// access control lists.
//
// # Permissions come from a policy, not from the walk
//
// The connector maps files to documents. It does not decide who may read them.
// That split exists because a directory tree says almost nothing about access
// on its own: the mode bits describe the account the crawler is running as, not
// the people in the company. Somewhere above the filesystem there is a real
// answer, in an OWNERS file, in a group export or in a share database, and a
// [Policy] is where that answer is plugged in.
//
// There is no permissive default. A source built without a policy quarantines
// everything, which is loud and safe, rather than publishing a directory tree
// to everybody, which is quiet and not.
//
// # Incremental sync
//
// The cursor is the highest modification time the last run saw. A later run
// walks the same tree and skips anything not newer, so the cost of a no change
// sync is a stat of every file rather than a read of every file. That is the
// honest limit of what a plain filesystem supports: without a change feed there
// is no way to avoid the walk, only the read.
package fssource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // registers the GIF config decoder
	_ "image/jpeg" // registers the JPEG config decoder
	_ "image/png"  // registers the PNG config decoder
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/doc"
)

// DefaultMaxFileSize is the largest file read into a document body.
//
// Past this the file is almost never prose somebody wants to search, and it is
// often a checked in binary that would cost far more to index than it is worth.
const DefaultMaxFileSize = 1 << 20

// DefaultMaxImageSize is the largest image read into a document's content.
//
// It is separate from the body limit and larger, because the two limits are
// about different things. A one megabyte text file is almost certainly not
// prose. A one megabyte screenshot is a screenshot.
const DefaultMaxImageSize = 4 << 20

// Policy decides who may read a file.
//
// It is called once per file with the path relative to the root, using forward
// slashes on every platform so that a policy written against a repository
// layout does not have to care where it is running.
//
// Returning an error quarantines that one document and does not stop the walk,
// because one unreadable share should not cost a whole sync.
type Policy interface {
	Permissions(ctx context.Context, relPath string) (acl.Permissions, error)
}

// PolicyFunc adapts a function to [Policy].
type PolicyFunc func(ctx context.Context, relPath string) (acl.Permissions, error)

// Permissions calls f.
func (f PolicyFunc) Permissions(ctx context.Context, relPath string) (acl.Permissions, error) {
	return f(ctx, relPath)
}

// PublicToTenant is a policy where every file is readable by everybody in the
// tenant.
//
// It is correct for a public documentation corpus and wrong for almost
// everything else, so it has to be asked for by name.
func PublicToTenant(source string) Policy {
	return PolicyFunc(func(context.Context, string) (acl.Permissions, error) {
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: source}, nil
	})
}

// Source reads documents out of a directory tree.
type Source struct {
	root   string
	name   string
	policy Policy

	maxSize   int64
	maxImage  int64
	skipDir   func(name string) bool
	includeIf func(name string) bool
	skipped   func(path string, reason error)
}

// Option configures a source.
type Option func(*Source)

// WithMaxFileSize sets the largest file that will be read. A value below one
// selects [DefaultMaxFileSize].
func WithMaxFileSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxSize = n
		}
	}
}

// WithSkipped installs a callback for files the walk passed over.
//
// A sync does not abandon a tree because one file in it could not be read. A
// file whose owner revoked the permission, or that was deleted between the
// listing and the stat, is a fact about the tree and not a reason to lose the
// hundred thousand files after it.
//
// What it must not be is silent. An index quietly missing the files nobody
// could read looks exactly like an index that is complete, and the difference
// only shows up when somebody cannot find a document they know exists. This is
// how a caller finds out: it is called once per skipped file with the path and
// the reason, and the default does nothing.
func WithSkipped(f func(path string, reason error)) Option {
	return func(s *Source) {
		if f != nil {
			s.skipped = f
		}
	}
}

// WithMaxImageSize sets the largest image that will be read into a document's
// content. A value below one selects [DefaultMaxImageSize].
func WithMaxImageSize(n int64) Option {
	return func(s *Source) {
		if n > 0 {
			s.maxImage = n
		}
	}
}

// WithSkipDir replaces the rule for directories that are not descended into.
// The argument is the base name.
func WithSkipDir(f func(name string) bool) Option {
	return func(s *Source) {
		if f != nil {
			s.skipDir = f
		}
	}
}

// WithInclude replaces the rule for which file names are read. The argument is
// the base name.
func WithInclude(f func(name string) bool) Option {
	return func(s *Source) {
		if f != nil {
			s.includeIf = f
		}
	}
}

// New returns a source reading root, naming itself name, and asking policy who
// may read each file.
//
// A nil policy is allowed and quarantines every document. That is deliberate:
// it makes "I have not thought about permissions yet" a visible state in the
// stats rather than an invisible one in the index.
func New(root, name string, policy Policy, opts ...Option) (*Source, error) {
	if root == "" {
		return nil, errors.New("fssource: empty root")
	}
	if name == "" {
		return nil, errors.New("fssource: empty source name")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fssource: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fssource: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fssource: %s is not a directory", abs)
	}

	s := &Source{
		root:      abs,
		name:      name,
		policy:    policy,
		maxSize:   DefaultMaxFileSize,
		maxImage:  DefaultMaxImageSize,
		skipDir:   defaultSkipDir,
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

// Close releases nothing, because a walk holds nothing open between calls.
func (s *Source) Close() error { return nil }

// Sync walks the tree and emits every file modified after the cursor.
//
// The returned cursor is the highest modification time seen in the whole tree,
// including files that were skipped as unchanged, so that a run which finds
// nothing new still moves the clock forward and a run interrupted halfway does
// not lose the files it already passed.
func (s *Source) Sync(ctx context.Context, from connector.Cursor, emit func(context.Context, connector.Change) error) (connector.Cursor, error) {
	since, err := parseCursor(from)
	if err != nil {
		return connector.Cursor{}, err
	}

	var highest time.Time
	walkErr := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that disappeared under the walk, or one this process
			// may not read, is a fact about the tree rather than a reason to
			// abandon the sync.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != s.root && s.skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !s.includeIf(d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			s.skipped(p, err)
			return nil
		}
		if limit := s.limitFor(d.Name()); info.Size() > limit {
			s.skipped(p, fmt.Errorf("%d bytes is over the limit of %d", info.Size(), limit))
			return nil
		}
		mod := info.ModTime()
		if mod.After(highest) {
			highest = mod
		}
		// Not After rather than Before, so a file written in the same
		// nanosecond as the cursor is not emitted twice on every later run.
		if !since.IsZero() && !mod.After(since) {
			return nil
		}

		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			s.skipped(p, err)
			return nil
		}
		rel = filepath.ToSlash(rel)

		document, err := s.read(ctx, p, rel, info)
		if err != nil {
			s.skipped(p, err)
			return nil
		}
		return emit(ctx, connector.Change{
			Document: document,
			Cursor:   connector.Cursor{Value: mod.UTC().Format(time.RFC3339Nano), Time: mod},
		})
	})
	if walkErr != nil {
		return connector.Cursor{}, walkErr
	}

	if highest.IsZero() {
		return from, nil
	}
	return connector.Cursor{Value: highest.UTC().Format(time.RFC3339Nano), Time: highest}, nil
}

// limitFor is the size limit that applies to one file name. An image gets the
// image limit and everything else gets the body limit.
func (s *Source) limitFor(name string) int64 {
	if isImage(name) {
		return s.maxImage
	}
	return s.maxSize
}

// read turns one file into a document.
func (s *Source) read(ctx context.Context, full, rel string, info fs.FileInfo) (doc.Document, error) {
	raw, err := os.ReadFile(full)
	if err != nil {
		return doc.Document{}, err
	}

	image := isImage(rel)
	// A file that is not valid UTF-8 and is not an image we recognise is a
	// binary this connector has no business pretending to have read. Extraction
	// of real binary formats is a separate job with separate failure modes.
	if !image && !utf8.Valid(raw) {
		return doc.Document{}, errors.New("not text")
	}

	perms := connector.Unresolved(s.name)
	if s.policy != nil {
		got, err := s.policy.Permissions(ctx, rel)
		if err == nil {
			perms = got
		}
		// On an error the document keeps the unresolved descriptor and is
		// quarantined by the pipeline. Failing to answer is not permission to
		// publish.
	}

	var (
		text    string
		content *doc.Content
	)
	// An image has no body. Its file name is what a query can match, which is
	// how somebody finds architecture.png by typing architecture, and the bytes
	// are what the preview shows.
	if image {
		content = &doc.Content{Bytes: raw}
		content.Width, content.Height = pixels(raw)
	} else {
		text = string(raw)
	}

	kind := kindOf(rel)
	return doc.Document{
		ID:           s.name + ":" + rel,
		Kind:         kind,
		Title:        titleOf(rel, text, kind),
		Body:         text,
		URL:          "file://" + filepath.ToSlash(full),
		Container:    containerOf(rel),
		ModifiedAt:   info.ModTime(),
		CreatedAt:    info.ModTime(),
		SourceUpdate: info.ModTime().UTC().Format(time.RFC3339Nano),
		Permissions:  perms,
		Properties: map[string]string{
			"path":        rel,
			"extension":   strings.TrimPrefix(path.Ext(rel), "."),
			doc.MediaType: mediaTypeOf(rel),
			"size_bytes":  strconv.FormatInt(info.Size(), 10),
		},
		Content: content,
	}, nil
}

// pixels reads the dimensions out of an encoded image without decoding it.
//
// The standard library answers for png, jpeg and gif. It does not answer for
// webp or svg, and rather than pull in a decoder for each, those record a zero,
// which the interface reads as no box to reserve.
func pixels(raw []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// parseCursor reads the modification time out of a cursor.
func parseCursor(c connector.Cursor) (time.Time, error) {
	if c.IsZero() {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, c.Value)
	if err != nil {
		// A cursor this connector cannot read is one written by a different
		// version or a different connector. Refusing is better than silently
		// resyncing the whole tree, which on a large corpus looks like a hang.
		return time.Time{}, fmt.Errorf("fssource: unreadable cursor %q: %w", c.Value, err)
	}
	return t, nil
}

// titleOf takes the first markdown heading, or the first non empty line if
// there is no heading, and falls back to the file name.
//
// Source files are named by their path instead. Their first line is a licence
// header or a package comment, which is the same in every file in the tree and
// tells a reader scanning a result list nothing about which file they are
// looking at. The path tells them exactly that, and is also how they would say
// it out loud.
func titleOf(rel, body string, kind doc.Kind) string {
	if kind == doc.KindCode {
		return rel
	}
	for line := range strings.SplitSeq(head(body, 4096), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if h, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(h)
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "---") {
			continue
		}
		if len(line) <= 120 {
			return line
		}
		break
	}
	return path.Base(rel)
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// containerOf is the directory a file lives in, relative to the root of the
// tree. A file at the root has no container rather than a container called
// ".", because that dot travels all the way to the result row and to the
// facet list, where it means nothing to anybody.
func containerOf(rel string) string {
	if dir := path.Dir(rel); dir != "." && dir != "/" {
		return dir
	}
	return ""
}

// kindOf maps an extension onto a document kind. Anything unrecognised is a
// file, which is the kind that promises the least.
func kindOf(rel string) doc.Kind {
	if isImage(rel) {
		return doc.KindImage
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown", ".rst", ".adoc", ".txt", ".html":
		return doc.KindPage
	case ".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cc", ".cpp",
		".java", ".rb", ".sh", ".sql", ".yaml", ".yml", ".toml", ".json", ".proto":
		return doc.KindCode
	default:
		return doc.KindFile
	}
}

// mediaTypes is the extension to media type table.
//
// It is a table rather than a call to mime.TypeByExtension because that
// function reads the operating system's mime database, which means the same
// corpus crawled on two machines can produce two different answers, and because
// the code types below are ours rather than registered ones.
var mediaTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".rst":      "text/plain",
	".adoc":     "text/plain",
	".html":     "text/html",
	".css":      "text/css",
	".go":       "text/x-go",
	".rs":       "text/x-rust",
	".py":       "text/x-python",
	".js":       "text/javascript",
	".jsx":      "text/javascript",
	".ts":       "text/x-typescript",
	".tsx":      "text/x-typescript",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".cc":       "text/x-c++",
	".cpp":      "text/x-c++",
	".java":     "text/x-java",
	".rb":       "text/x-ruby",
	".sh":       "text/x-shellscript",
	".sql":      "text/x-sql",
	".yaml":     "text/x-yaml",
	".yml":      "text/x-yaml",
	".toml":     "text/x-toml",
	".json":     "application/json",
	".proto":    "text/x-protobuf",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".svg":      "image/svg+xml",
}

// mediaTypeOf is what a document says it is. An extension nobody recognises is
// text/plain, because that is what the connector actually read and it is the
// type a preview can render without guessing.
func mediaTypeOf(rel string) string {
	if t, ok := mediaTypes[strings.ToLower(path.Ext(rel))]; ok {
		return t
	}
	return "text/plain"
}

// isImage reports whether a file name is one of the image types this connector
// stores bytes for.
func isImage(name string) bool {
	return strings.HasPrefix(mediaTypeOf(name), "image/")
}

// defaultSkipDir skips version control, dependency and build directories, which
// hold far more files than the corpus and none of the content.
func defaultSkipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "target", "dist", "build",
		".venv", "__pycache__", ".idea", ".vscode":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// defaultInclude reads text formats and the image formats a preview can show.
func defaultInclude(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	if name == "OWNERS" || name == "README" || name == "LICENSE" {
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".rst", ".adoc", ".txt", ".html", ".css",
		".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cc", ".cpp",
		".java", ".rb", ".sh", ".sql", ".yaml", ".yml", ".toml", ".json", ".proto",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return true
	}
	return false
}
