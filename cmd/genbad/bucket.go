package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tamnd/genba/connector/objectsource"
	"github.com/tamnd/genba/store"
)

// bucketOptions is what the -bucket flags add up to.
//
// They are the object storage half of the same idea the -corpus flags are: a
// server that is useful the first time it starts, pointed at something the
// person running it already has. A bucket is the thing most companies already
// have a lot of documents in, and it needs no agent installed anywhere and no
// vendor application to be approved before anybody can try it.
type bucketOptions struct {
	// Bucket is the bucket to read. Empty means do not ingest.
	Bucket string

	// Endpoint is the service's base URL, for example
	// https://s3.eu-west-1.amazonaws.com or http://127.0.0.1:9000.
	Endpoint string

	// Region is the region the bucket is in, which is part of what a signature
	// authenticates rather than only a piece of routing. A service with no
	// regions still expects to be signed with one.
	Region string

	// Prefix narrows the source to one part of the bucket, and narrows the
	// listing rather than filtering it afterwards, so scoping tightly inside a
	// very large bucket costs what the scope costs.
	Prefix string

	// Name is the source name the documents carry, and what a query filters on.
	Name string

	// ACL selects how permissions are decided: "tenant", "bucket" or "object".
	ACL string

	// Identity names the identity source the names in the access control lists
	// belong to, and is what the mapping writes its references under.
	Identity string

	// Domain is the mail domain that counts as this tenant, which is what a
	// grant written against an email address is checked against. Empty leaves
	// every such grant foreign, which is the safe reading.
	Domain string

	// PathStyle puts the bucket in the path rather than in the host name. It is
	// what MinIO, Ceph and anything else on a single host name need, and what
	// S3 itself does not.
	PathStyle bool

	// Refresh is how often to list the bucket again. Zero syncs once at startup.
	Refresh time.Duration

	// Reconcile is how often to sweep the index against the bucket. Zero sweeps
	// after every sync.
	Reconcile time.Duration

	// Access, Secret and Session are the credentials, and they are read from the
	// environment rather than taken from a flag.
	//
	// A secret in argv is readable by every process on the machine for as long
	// as the server runs, and it ends up in the shell history of whoever started
	// it. There is no flag for these on purpose, and the names are the ones
	// every other tool in this space already uses, so a machine that can already
	// reach the bucket needs nothing new set.
	Access  string
	Secret  string
	Session string
}

// The values -bucket-acl takes. The tenant one is shared with -corpus-acl,
// where it means the same thing.
const (
	aclBucket = "bucket"
	aclObject = "object"
)

// credentials fills in the keys from the environment.
func (o *bucketOptions) credentials(getenv func(string) string) {
	o.Access = getenv("AWS_ACCESS_KEY_ID")
	o.Secret = getenv("AWS_SECRET_ACCESS_KEY")
	o.Session = getenv("AWS_SESSION_TOKEN")
}

func (o bucketOptions) validate() error {
	if o.Bucket == "" {
		return nil
	}
	if o.Endpoint == "" {
		// There is no default worth guessing. The endpoint decides which company
		// the request goes to, and a wrong guess here is a bucket name and a set
		// of credentials sent somewhere nobody meant to send them.
		return errors.New("a bucket needs -bucket-endpoint, for example https://s3.eu-west-1.amazonaws.com")
	}
	if o.Name == "" {
		return errors.New("bucket source name is empty")
	}
	switch o.ACL {
	case aclTenant:
	case aclBucket, aclObject:
		if o.Identity == "" {
			// Every reference the mapping writes carries this name, and without
			// one they would all be compared against the bare account name.
			return fmt.Errorf("bucket acl %q needs an identity source", o.ACL)
		}
	default:
		return fmt.Errorf("unknown bucket acl %q, want %q, %q or %q", o.ACL, aclTenant, aclBucket, aclObject)
	}
	// Half a set of credentials signs nothing and is answered with a refusal
	// that says the signature did not match, which sends whoever reads it
	// looking at the clock and the region rather than at the one variable they
	// forgot to export.
	if (o.Access == "") != (o.Secret == "") {
		return errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY have to be set together, or neither for a public bucket")
	}
	if o.Refresh < 0 {
		return errors.New("bucket refresh is negative")
	}
	if o.Reconcile < 0 {
		return errors.New("bucket reconcile interval is negative")
	}
	return nil
}

// bucketPolicyFor builds the permission policy named by the flags.
//
// The three are in order of what they cost and of how exact they are. The
// tenant policy asks nothing and is right for a bucket of published
// documentation. The bucket policy reads one access control list per sync and
// is right for the common case where a bucket is one team's and the objects in
// it were all written by one process. The object policy reads one per object
// per sync, which on a bucket of any size is the most expensive thing this
// binary can be asked to do, and is right only when the objects really do
// differ.
func bucketPolicyFor(c *objectsource.Client, o bucketOptions) (objectsource.Policy, error) {
	var domains []string
	if o.Domain != "" {
		domains = append(domains, o.Domain)
	}
	switch o.ACL {
	case aclTenant:
		return objectsource.PublicToTenant(o.Name), nil
	case aclBucket:
		p, err := objectsource.NewBucketPolicy(c, o.Name, o.Identity, domains...)
		if err != nil {
			return nil, err
		}
		return p, nil
	case aclObject:
		p, err := objectsource.NewObjectPolicy(c, o.Name, o.Identity, domains...)
		if err != nil {
			return nil, err
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unknown bucket acl %q", o.ACL)
	}
}

// ingestBucket syncs the configured bucket into the store.
//
// It runs on the same schedule the directory does, for the same reason: the
// first sync finishes before the listener opens, and later ones are incremental
// against the cursor the last one saved.
func ingestBucket(ctx context.Context, st store.Store, cfg bucketOptions, tenant string, log *slog.Logger) (func(), error) {
	if cfg.Bucket == "" {
		return func() {}, nil
	}
	if tenant == "" {
		return nil, errors.New("ingesting a bucket needs -tenant")
	}

	client, err := objectsource.NewClient(objectsource.Config{
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		Bucket:          cfg.Bucket,
		AccessKeyID:     cfg.Access,
		SecretAccessKey: cfg.Secret,
		SessionToken:    cfg.Session,
		PathStyle:       cfg.PathStyle,
	})
	if err != nil {
		return nil, err
	}

	// The policy is built on the same client as the source, so that what the
	// permission lookups cost and what the reads cost land in one set of
	// counters rather than two that have to be added up by hand.
	policy, err := bucketPolicyFor(client, cfg)
	if err != nil {
		return nil, err
	}

	src, err := objectsource.New(client, cfg.Name, policy,
		objectsource.WithPrefix(cfg.Prefix),
		objectsource.WithSkipped(func(key string, reason error) {
			// An object nobody could read is not an error and does not stop the
			// sync, and an index quietly missing all of them looks exactly like
			// an index that is complete. This is the only place that difference
			// is visible.
			log.Warn("object passed over", "bucket", cfg.Bucket, "key", key, "reason", reason)
		}),
	)
	if err != nil {
		return nil, err
	}

	return runFeed(ctx, st, feed{
		Kind:      "bucket",
		Source:    src,
		Tenant:    tenant,
		Refresh:   cfg.Refresh,
		Reconcile: cfg.Reconcile,
		Fields:    []any{"bucket", cfg.Bucket, "prefix", cfg.Prefix, "source", cfg.Name},
		Report:    func() []any { return requesting(client) },
		Policy:    policy,
		Release:   func() { _ = src.Close() },
	}, log)
}

// requesting is what the bucket cost, for the sync log line.
//
// These are the numbers a bill is made of, and they are cumulative rather than
// per sync so that the shape over time is what an operator reads. A listing
// count that climbs by one per sync is a healthy incremental run, and a fetch
// count that climbs with it on a bucket nobody is writing to means the cursor
// is not doing its job.
func requesting(c *objectsource.Client) []any {
	n := c.Counters()
	return []any{
		"lists", n.Lists,
		"metadata", n.Metadata,
		"fetches", n.Fetches,
		"fetched_mb", megabytes(n.Bytes),
	}
}
