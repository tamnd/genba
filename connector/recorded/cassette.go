package recorded

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A recording is a directory of numbered files rather than one file holding
// everything.
//
// One file per exchange is what makes the set reviewable. A connector's crawl
// is twenty or thirty requests, a change to one of them should be a change to
// one file, and a person looking for what the source says when a channel is
// private should be able to find it by reading the file names. A single large
// file would put every refresh of the fixtures into one diff hunk and would
// make the interesting response the four hundredth line of it.
//
// The number in front is the order the requests were made in, which is the
// other half of what a reader needs: a crawl is a sequence, and a listing that
// sorted alphabetically would put the second page of results before the first.
const nameFormat = "%04d-%s.json"

// written matches the files Save writes, and is what tells them apart from
// anything else somebody has put in the directory.
var written = regexp.MustCompile(`^\d{4}-.*\.json$`)

// Load reads the exchanges recorded in a directory, in the order they were
// made.
func Load(dir string) ([]Exchange, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("recorded: reading %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("recorded: %s holds no recordings", dir)
	}
	slices.Sort(names)

	out := make([]Exchange, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("recorded: %w", err)
		}
		var e Exchange
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("recorded: %s: %w", name, err)
		}
		if e.Request.URL == "" {
			return nil, fmt.Errorf("recorded: %s records no request", name)
		}
		out = append(out, e)
	}
	return out, nil
}

// Save writes what has been recorded into a directory, creating it if it is not
// there.
//
// Files left over from an earlier recording are removed, so that refreshing a
// fixture set against a service that no longer has an endpoint leaves the
// directory describing the crawl as it is now. Only the files Save itself
// writes are touched: a README next to them stays where it is.
func (t *Transport) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("recorded: %w", err)
	}
	if err := removeSaved(dir); err != nil {
		return err
	}

	for i, e := range t.Exchanges() {
		raw, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return fmt.Errorf("recorded: %w", err)
		}
		name := fmt.Sprintf(nameFormat, i+1, slug(e.Request))
		if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o644); err != nil {
			return fmt.Errorf("recorded: %w", err)
		}
	}
	return nil
}

// removeSaved removes the files a previous Save wrote.
func removeSaved(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("recorded: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !written.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("recorded: %w", err)
		}
	}
	return nil
}

// slug is the readable half of a file name: enough of the request to find the
// file by eye, and nothing that would make the name depend on the machine it
// was recorded on.
func slug(r Request) string {
	u, err := url.Parse(r.URL)
	if err != nil {
		return "request"
	}

	parts := []string{strings.ToLower(r.Method)}
	for _, seg := range strings.Split(path.Clean(u.Path), "/") {
		if seg == "" || seg == "." {
			continue
		}
		parts = append(parts, seg)
	}

	// A path of identifiers is a path where every file would otherwise be
	// called the same thing, so the query is what tells two pages of the same
	// listing apart.
	if q := u.Query(); len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			parts = append(parts, k+"-"+q.Get(k))
		}
	}

	out := clean(strings.Join(parts, "-"))
	if out == "" {
		return "request"
	}
	return trim(out, 80)
}

// clean reduces a string to the characters that are the same on every operating
// system a test might run on.
func clean(s string) string {
	var b strings.Builder
	var dash bool
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// trim shortens a name without cutting it in the middle of a word where it can
// help it.
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndex(s, "-"); i > n/2 {
		s = s[:i]
	}
	return strings.Trim(s, "-")
}
