package objectsource_test

import (
	"cmp"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/genba/connector/objectsource"
)

// A bucket in memory, speaking enough of the protocol to be worth testing
// against.
//
// It is here rather than in a container because what these tests are about is
// the connector: paging, the cursor, what gets fetched and what does not, and
// what an access control list turns into. A real service would answer all of
// those the same way and would also make the suite need Docker, a network and
// several seconds. The parts a fake cannot check, which are whether a real
// service accepts these signatures and returns this XML, are checked by
// sign_test.go against the published examples and by the shape of the types
// against the documented responses.

const theBucket = "corpus"

// The grantee URIs the protocol defines, spelled out here rather than reached
// for inside the package, so that a test breaks if somebody changes one.
const (
	uriAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	uriAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
)

// stored is one object the fake holds.
type stored struct {
	body     string
	modified time.Time
	acl      string
}

// fakeStore is an S3 compatible service with one bucket in it.
type fakeStore struct {
	server *httptest.Server

	mu      sync.Mutex
	objects map[string]stored
	acl     string
	clock   time.Time

	// calls is every request line the service was sent, which is how a test
	// says something was done with one listing rather than with a thousand.
	calls []string
}

// newStore starts a fake service and stops it when the test ends.
func newStore(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{
		objects: make(map[string]stored),
		clock:   time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// client is a client pointed at the fake, with credentials, so that every
// request in these tests is signed the way a real one would be.
func (f *fakeStore) client(t *testing.T) *objectsource.Client {
	t.Helper()
	c, err := objectsource.NewClient(objectsource.Config{
		Endpoint:        f.server.URL,
		Region:          "eu-west-1",
		Bucket:          theBucket,
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		PathStyle:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// put writes an object, at the service's current time.
func (f *fakeStore) put(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = stored{body: body, modified: f.clock}
}

// putAt writes an object with a modification time of its own.
func (f *fakeStore) putAt(key, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = stored{body: body, modified: at}
}

// remove deletes an object, which is the event a listing can never report.
func (f *fakeStore) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

// setACL replaces the bucket's access control list.
func (f *fakeStore) setACL(list string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acl = list
}

// setObjectACL gives one object a list of its own.
func (f *fakeStore) setObjectACL(key, list string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[key]
	o.acl = list
	f.objects[key] = o
}

// tick moves the service's clock, which is the clock the connector's cursor is
// written against.
func (f *fakeStore) tick(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

// now is the service's current time.
func (f *fakeStore) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// requests returns every request line the service has been sent since the last
// call, and forgets them.
func (f *fakeStore) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func (f *fakeStore) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())

	// Every request has to be signed. A connector that quietly stopped signing
	// would pass every other test here and fail against every real service.
	auth := r.Header.Get("Authorization")
	switch {
	case !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="):
		refuse(w, http.StatusForbidden, "AccessDenied", "the request is not signed")
		return
	case r.Header.Get("X-Amz-Date") == "", r.Header.Get("X-Amz-Content-Sha256") == "":
		refuse(w, http.StatusForbidden, "AccessDenied", "the signature is missing a header it covers")
		return
	}

	// The service's own clock, which the connector reads and holds its cursor
	// behind. Set explicitly so that a test can move it.
	w.Header().Set("Date", f.clock.UTC().Format(http.TimeFormat))

	prefix := "/" + theBucket
	if !strings.HasPrefix(r.URL.Path, prefix) {
		refuse(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")

	if _, ok := r.URL.Query()["acl"]; ok {
		f.serveACL(w, key)
		return
	}
	if key == "" {
		f.serveList(w, r)
		return
	}
	f.serveObject(w, key)
}

func (f *fakeStore) serveACL(w http.ResponseWriter, key string) {
	list := f.acl
	if key != "" {
		o, ok := f.objects[key]
		if !ok {
			refuse(w, http.StatusNotFound, "NoSuchKey", "no such key")
			return
		}
		list = cmp.Or(o.acl, f.acl)
	}
	if list == "" {
		refuse(w, http.StatusNotFound, "NoSuchBucketPolicy", "this bucket has no access control list")
		return
	}
	writeXML(w, list)
}

func (f *fakeStore) serveObject(w http.ResponseWriter, key string) {
	o, ok := f.objects[key]
	if !ok {
		refuse(w, http.StatusNotFound, "NoSuchKey", "no such key")
		return
	}
	w.Header().Set("ETag", `"`+etagOf(key, o.body)+`"`)
	w.Header().Set("Last-Modified", o.modified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
	_, _ = w.Write([]byte(o.body))
}

func (f *fakeStore) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		refuse(w, http.StatusBadRequest, "InvalidArgument", "only version two listings are served")
		return
	}

	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	// The two ways a listing is narrowed, applied the way the API applies them:
	// the prefix, then wherever the caller said to carry on from. A
	// continuation token and a start after point are the same thing here, which
	// is exactly what the real service's opaque token amounts to.
	after := cmp.Or(q.Get("continuation-token"), q.Get("start-after"))
	limit := 1000
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil && n > 0 {
		limit = n
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	var (
		sent      int
		last      string
		truncated bool
	)
	for _, k := range keys {
		if !strings.HasPrefix(k, q.Get("prefix")) || k <= after {
			continue
		}
		if sent == limit {
			truncated = true
			break
		}
		o := f.objects[k]
		b.WriteString("<Contents><Key>" + k + "</Key>")
		b.WriteString("<LastModified>" + o.modified.UTC().Format("2006-01-02T15:04:05.000Z") + "</LastModified>")
		b.WriteString(`<ETag>&quot;` + etagOf(k, o.body) + `&quot;</ETag>`)
		b.WriteString("<Size>" + strconv.Itoa(len(o.body)) + "</Size>")
		b.WriteString("<StorageClass>STANDARD</StorageClass></Contents>")
		sent, last = sent+1, k
	}
	if truncated {
		b.WriteString("<IsTruncated>true</IsTruncated>")
		b.WriteString("<NextContinuationToken>" + last + "</NextContinuationToken>")
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	b.WriteString("</ListBucketResult>")
	writeXML(w, b.String())
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(body))
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<Error><Code>" + code + "</Code><Message>" + message + "</Message></Error>"))
}

// etagOf is a stand in for the content hash a real service returns. It only has
// to change when the bytes do, which is the whole of what a version is for.
func etagOf(key, body string) string {
	return strconv.Itoa(len(body)) + "-" + strconv.Itoa(len(key))
}

// The access control lists the tests hand the fake.

// ownedBy is a list where one person owns the object and can read it, which is
// what a bucket written by one account and read by nobody else looks like.
func ownedBy(email string) string {
	return `<AccessControlPolicy>` +
		`<Owner><ID>abc123</ID><DisplayName>` + email + `</DisplayName><EmailAddress>` + email + `</EmailAddress></Owner>` +
		`<AccessControlList>` + userGrant(email, "FULL_CONTROL") + `</AccessControlList>` +
		`</AccessControlPolicy>`
}

// listOf builds a list with an owner and whatever statements are given.
func listOf(owner string, grants ...string) string {
	return `<AccessControlPolicy>` +
		`<Owner><ID>abc123</ID><DisplayName>` + owner + `</DisplayName><EmailAddress>` + owner + `</EmailAddress></Owner>` +
		`<AccessControlList>` + strings.Join(grants, "") + `</AccessControlList>` +
		`</AccessControlPolicy>`
}

func userGrant(email, permission string) string {
	return `<Grant><Grantee><ID>` + email + `</ID><DisplayName>` + email + `</DisplayName>` +
		`<EmailAddress>` + email + `</EmailAddress></Grantee>` +
		`<Permission>` + permission + `</Permission></Grant>`
}

func groupGrant(uri, permission string) string {
	return `<Grant><Grantee><URI>` + uri + `</URI></Grantee>` +
		`<Permission>` + permission + `</Permission></Grant>`
}
