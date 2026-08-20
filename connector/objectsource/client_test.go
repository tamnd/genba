package objectsource

import (
	"testing"
	"time"
)

// Where the bucket goes in the URL is the one thing that differs between the
// services this connector supports, and getting it wrong is a request that
// either goes to a host that does not resolve or reads a bucket called the
// bucket's name inside the bucket.
func TestWhereTheBucketGoesInTheRequest(t *testing.T) {
	for _, c := range []struct {
		name     string
		endpoint string
		style    bool
		key      string
		host     string
		path     string
	}{
		{
			name:     "the hosted style S3 itself wants",
			endpoint: "https://s3.eu-west-1.amazonaws.com",
			key:      "reports/q1.pdf",
			host:     "corpus.s3.eu-west-1.amazonaws.com",
			path:     "/reports/q1.pdf",
		},
		{
			name:     "the path style everything on a single host needs",
			endpoint: "http://127.0.0.1:9000",
			style:    true,
			key:      "reports/q1.pdf",
			host:     "127.0.0.1:9000",
			path:     "/corpus/reports/q1.pdf",
		},
		{
			name:     "a listing, which is the bucket with no key",
			endpoint: "http://127.0.0.1:9000",
			style:    true,
			host:     "127.0.0.1:9000",
			path:     "/corpus/",
		},
		{
			name:     "an endpoint that already has a path, which is what a gateway looks like",
			endpoint: "https://gateway.example.com/storage/",
			style:    true,
			key:      "a.md",
			host:     "gateway.example.com",
			path:     "/storage/corpus/a.md",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, err := NewClient(Config{Endpoint: c.endpoint, Bucket: "corpus", PathStyle: c.style})
			if err != nil {
				t.Fatal(err)
			}
			host, path := client.target(c.key)
			if host != c.host {
				t.Errorf("host is %q, want %q", host, c.host)
			}
			if path != c.path {
				t.Errorf("path is %q, want %q", path, c.path)
			}
		})
	}
}

// A link in a result row has to survive being pasted somewhere, which means the
// characters that are legal in a key and not in a URL have to be encoded.
func TestALinkToAnObjectIsEscaped(t *testing.T) {
	client, err := NewClient(Config{Endpoint: "https://s3.eu-west-1.amazonaws.com", Bucket: "corpus"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"notes/q1 report.pdf": "https://corpus.s3.eu-west-1.amazonaws.com/notes/q1%20report.pdf",
		"a+b/c.md":            "https://corpus.s3.eu-west-1.amazonaws.com/a%2Bb/c.md",
		"~tilde-and_dot.md":   "https://corpus.s3.eu-west-1.amazonaws.com/~tilde-and_dot.md",
	} {
		if got := client.url(key); got != want {
			t.Errorf("%q links to\n%s\nwant\n%s", key, got, want)
		}
	}
}

// The version is what tells a rewritten object from an unchanged one, and a
// version that is empty makes every object look stale on every sweep.
func TestTheVersionOfAnObject(t *testing.T) {
	when := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		o    object
		want string
	}{
		{
			name: "the entity tag, with the quotes the store puts round it taken off",
			o:    object{ETag: `"9b2cf5"`, LastModified: when},
			want: "9b2cf5",
		},
		{
			name: "the modification time, for a store that sends no entity tag",
			o:    object{LastModified: when},
			want: "2026-03-02T12:00:00Z",
		},
		{
			name: "nothing at all, which is a store that described nothing",
			o:    object{},
			want: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.version(); got != c.want {
				t.Errorf("version is %q, want %q", got, c.want)
			}
		})
	}
}

// The reading of a refusal decides whether a document is deleted from the
// index, so it is deliberately narrow. Anything wider empties the index the
// first time a setting is typoed or the store has a bad afternoon.
func TestWhatCountsAsAnObjectThatIsNoLongerThere(t *testing.T) {
	for _, c := range []struct {
		name string
		err  apiError
		want bool
	}{
		{
			name: "the store said the key is not there",
			err:  apiError{Status: 404, Code: "NoSuchKey", Key: "a.md"},
			want: true,
		},
		{
			name: "four hundred and four with no code, from a service that sends none",
			err:  apiError{Status: 404, Key: "a.md"},
			want: true,
		},
		{
			name: "the bucket is not there, which is a setting rather than a deletion",
			err:  apiError{Status: 404, Code: "NoSuchBucket", Key: "a.md"},
		},
		{
			name: "a listing that came back empty handed names no key",
			err:  apiError{Status: 404},
		},
		{
			name: "the credentials are wrong",
			err:  apiError{Status: 403, Code: "AccessDenied", Key: "a.md"},
		},
		{
			name: "the store is having a bad afternoon",
			err:  apiError{Status: 500, Code: "InternalError", Key: "a.md"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.gone(); got != c.want {
				t.Errorf("gone is %v, want %v, from %v", got, c.want, c.err.Error())
			}
		})
	}
}
