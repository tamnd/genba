package objectsource

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tamnd/genba/connector"
)

// DefaultRequestTimeout bounds one request to the store.
//
// It covers reading the body as well as making the call, which is why it is
// generous. A sixteen megabyte report over a slow link is a normal thing for
// this connector to be doing and is not a hung request.
const DefaultRequestTimeout = 2 * time.Minute

// DefaultPageSize is how many objects one listing asks for. A thousand is the
// most S3 will return and the most every service that copies it will return.
const DefaultPageSize = 1000

// Config says where a bucket is and how to prove the right to read it.
type Config struct {
	// Endpoint is the service's base URL, for example
	// https://s3.eu-west-1.amazonaws.com or http://127.0.0.1:9000.
	Endpoint string

	// Region is the region the bucket is in, which is part of what a signature
	// authenticates. Empty means us-east-1, which is what services that have no
	// regions at all expect to be signed with.
	Region string

	// Bucket is the bucket to read.
	Bucket string

	// AccessKeyID and SecretAccessKey are the credentials. Both empty sends
	// unsigned requests, which is what a public bucket wants and what nothing
	// else does.
	AccessKeyID     string
	SecretAccessKey string

	// SessionToken is set when the credentials are temporary.
	SessionToken string

	// PathStyle puts the bucket in the path rather than in the host name.
	//
	// It is off by default because that is what S3 itself wants, and it has to
	// be turned on for almost everything else: MinIO, Ceph and a development
	// service on localhost have one host name and no way to give a bucket its
	// own.
	PathStyle bool
}

// Client talks to one bucket.
//
// It is safe for concurrent use and is meant to be shared. A [Source] and the
// [Policy] that answers for it should be built on the same client, so that what
// the permission lookups cost shows up in the same counters as what the reads
// cost.
type Client struct {
	cfg      Config
	endpoint *url.URL
	http     *http.Client

	lists    atomic.Int64
	metadata atomic.Int64
	fetches  atomic.Int64
	bytes    atomic.Int64
}

// ClientOption configures a client.
type ClientOption func(*Client)

// WithHTTPClient replaces the HTTP client, which is where a deployment puts its
// own transport, proxy or connection pool.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// NewClient returns a client for the bucket described by cfg.
func NewClient(cfg Config, opts ...ClientOption) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("objectsource: empty endpoint")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("objectsource: empty bucket")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("objectsource: endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("objectsource: endpoint %q is not http or https", cfg.Endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("objectsource: endpoint %q names no host", cfg.Endpoint)
	}

	c := &Client{
		cfg:      cfg,
		endpoint: u,
		http:     &http.Client{Timeout: DefaultRequestTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Bucket returns the bucket this client reads.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// Counters returns what this client has spent on the store.
//
// Everything built on it contributes, so a source and its permission policy
// report one set of numbers rather than two that have to be added up by hand.
func (c *Client) Counters() connector.Counters {
	return connector.Counters{
		Lists:    c.lists.Load(),
		Metadata: c.metadata.Load(),
		Fetches:  c.fetches.Load(),
		Bytes:    c.bytes.Load(),
	}
}

// object is one thing in the bucket, as the store described it.
type object struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
}

// version is the string that says which revision of an object this is.
//
// The entity tag is the right answer where there is one, because for an
// ordinary upload it is a hash of the content and two objects with the same tag
// are the same bytes. Where there is none the modification time has to do, and
// a second of resolution is enough to notice a document was rewritten.
func (o object) version() string {
	if tag := strings.Trim(o.ETag, `"`); tag != "" {
		return tag
	}
	if o.LastModified.IsZero() {
		return ""
	}
	return o.LastModified.UTC().Format(time.RFC3339)
}

// page is one response to a listing.
type page struct {
	objects []object
	next    string
	more    bool

	// at is the store's own clock, read out of the response. It is what the
	// cursor is compared against, because comparing a time the store wrote
	// against a time this process read off its own clock is how a sync that
	// looks correct on a developer's laptop loses documents in production.
	at time.Time
}

// listBucketResult is the ListObjectsV2 response.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []object `xml:"Contents"`
}

// list asks for one page of objects under a prefix.
//
// after is a key to resume past and is only honoured on the first page of a
// listing, which is the API's rule rather than this function's: once there is a
// continuation token, the token is the whole of where to carry on from.
func (c *Client) list(ctx context.Context, prefix, after, token string, limit int) (page, error) {
	q := url.Values{"list-type": {"2"}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if token != "" {
		q.Set("continuation-token", token)
	} else if after != "" {
		q.Set("start-after", after)
	}
	if limit > 0 {
		q.Set("max-keys", strconv.Itoa(limit))
	}

	c.lists.Add(1)
	resp, err := c.do(ctx, http.MethodGet, "", q)
	if err != nil {
		return page{}, err
	}
	defer drain(resp)

	var out listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return page{}, fmt.Errorf("objectsource: listing %s: %w", c.cfg.Bucket, err)
	}
	return page{
		objects: out.Contents,
		next:    out.NextContinuationToken,
		more:    out.IsTruncated,
		at:      serverTime(resp),
	}, nil
}

// get reads one object, refusing one over the limit.
//
// It reads a byte past the limit on purpose, because an object exactly at the
// limit is fine and an object over it has to be told apart from one that
// happens to end there. Trusting the size the listing reported would be the
// alternative, and a listing is a description rather than the thing.
func (c *Client) get(ctx context.Context, key string, limit int64) ([]byte, object, error) {
	c.fetches.Add(1)
	resp, err := c.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, object{}, err
	}
	defer drain(resp)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	c.bytes.Add(int64(len(raw)))
	if err != nil {
		return nil, object{}, fmt.Errorf("objectsource: reading %s: %w", key, err)
	}
	if int64(len(raw)) > limit {
		return nil, object{}, fmt.Errorf("objectsource: %s is over the limit of %d bytes", key, limit)
	}

	o := object{
		Key:          key,
		ETag:         resp.Header.Get("ETag"),
		Size:         int64(len(raw)),
		StorageClass: resp.Header.Get("X-Amz-Storage-Class"),
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		o.LastModified = t
	}
	return raw, o, nil
}

// accessControl is the response to an acl request, on a bucket or on an object.
//
// The grantee's xsi:type attribute is not read. What kind of grantee it is can
// be told from which of the fields is filled in, and a document is not worth
// quarantining over an XML namespace declaration a service left out.
type accessControl struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	Owner   grantee  `xml:"Owner"`
	Grants  []struct {
		Grantee    grantee `xml:"Grantee"`
		Permission string  `xml:"Permission"`
	} `xml:"AccessControlList>Grant"`
}

// grantee is whoever a statement is about.
type grantee struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
	URI         string `xml:"URI"`
	Email       string `xml:"EmailAddress"`
}

// acl reads the access control list of one object, or of the bucket when the
// key is empty.
func (c *Client) acl(ctx context.Context, key string) (accessControl, time.Time, error) {
	c.metadata.Add(1)
	resp, err := c.do(ctx, http.MethodGet, key, url.Values{"acl": {""}})
	if err != nil {
		return accessControl{}, time.Time{}, err
	}
	defer drain(resp)

	var out accessControl
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return accessControl{}, time.Time{}, fmt.Errorf("objectsource: reading the access control list of %s/%s: %w",
			c.cfg.Bucket, key, err)
	}
	return out, serverTime(resp), nil
}

// do makes one signed request and reports a status outside the two hundreds as
// an error.
func (c *Client) do(ctx context.Context, method, key string, query url.Values) (*http.Response, error) {
	u := *c.endpoint
	host, p := c.target(key)
	u.Host = host
	u.Path = p
	// The raw path is set as well as the path so that the transport writes the
	// same encoding the signature was calculated over. Without it a key with a
	// plus or a space in it signs one way and is sent another, and the store
	// refuses a request that is perfectly well formed.
	u.RawPath = escape(p, true)
	u.RawQuery = canonicalQuery(query)

	req, err := http.NewRequestWithContext(ctx, method, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("objectsource: %w", err)
	}
	if c.cfg.AccessKeyID != "" {
		sign(req, emptyPayload, c.cfg, time.Now())
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("objectsource: %s %s: %w", method, u.Redacted(), err)
	}
	if resp.StatusCode/100 != 2 {
		defer drain(resp)
		return nil, readError(resp, key)
	}
	return resp, nil
}

// target is the host and path one key is read from.
//
// Which of the two the bucket goes in is the difference between S3 and almost
// everything that speaks its protocol, and it is a property of the deployment
// rather than of the request, so it is a setting rather than a guess.
func (c *Client) target(key string) (host, path string) {
	base := strings.TrimSuffix(c.endpoint.Path, "/")
	if c.cfg.PathStyle {
		return c.endpoint.Host, base + "/" + c.cfg.Bucket + "/" + key
	}
	return c.cfg.Bucket + "." + c.endpoint.Host, base + "/" + key
}

// url is where a key can be read from, for a result row to link to.
func (c *Client) url(key string) string {
	u := *c.endpoint
	host, p := c.target(key)
	u.Host = host
	u.Path = p
	u.RawPath = escape(p, true)
	u.RawQuery = ""
	return u.String()
}

// apiError is what the store said when it refused.
type apiError struct {
	Status  int
	Code    string
	Message string
	Key     string
}

func (e *apiError) Error() string {
	where := e.Key
	if where == "" {
		where = "the bucket"
	}
	if e.Code == "" {
		return "objectsource: " + where + ": " + strconv.Itoa(e.Status) + " " + http.StatusText(e.Status)
	}
	return "objectsource: " + where + ": " + e.Code + ": " + e.Message
}

// gone reports whether this error means the object is not there any more,
// rather than that something else went wrong.
//
// The distinction decides whether a document is deleted from the index, so the
// reading is deliberately narrow. A bucket that has been renamed also answers
// with four hundred and four, and treating that as an object that no longer
// exists would empty the whole index the first time somebody typoed a setting.
func (e *apiError) gone() bool {
	if e.Code != "" {
		return e.Code == "NoSuchKey"
	}
	return e.Status == http.StatusNotFound && e.Key != ""
}

// errorBody is the XML every one of these services returns with a refusal.
type errorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// maxErrorBody bounds what is read out of a refusal, because the response to a
// request that went to the wrong host is an HTML page of unknown size.
const maxErrorBody = 8 << 10

func readError(resp *http.Response, key string) error {
	var body errorBody
	// A body that does not parse is not itself a failure. The status is the
	// part that matters and the code is what makes the log line useful, so a
	// service that answered with something else still produces an error naming
	// what happened.
	_ = xml.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)
	return &apiError{
		Status:  resp.StatusCode,
		Code:    body.Code,
		Message: body.Message,
		Key:     key,
	}
}

// gone turns a refusal that means the object is not there into
// [connector.ErrGone], and leaves everything else alone.
func gone(err error) error {
	var api *apiError
	if errors.As(err, &api) && api.gone() {
		return connector.ErrGone
	}
	return err
}

// serverTime is the store's own clock, from the response.
//
// A service that sends no Date header leaves this zero, and the caller falls
// back to not knowing rather than to this machine's clock, which is the thing
// the header exists to avoid trusting.
func serverTime(resp *http.Response) time.Time {
	t, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return t
}

// drain closes a response body after reading what is left of it, so the
// connection goes back to the pool instead of being thrown away.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}
