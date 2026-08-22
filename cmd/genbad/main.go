// Command genbad runs the server.
//
// It is one binary with the interface, the API and the storage driver in it, so
// a first install is a download and a run. The same binary is what a large
// deployment runs, with a different storage driver and more copies of it behind
// a load balancer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/config"
	"github.com/tamnd/genba/connector/limit"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
	"github.com/tamnd/genba/store/pgstore"
	"github.com/tamnd/genba/store/sqlitestore"
	"github.com/tamnd/genba/web"
)

func main() {
	os.Exit(cli())
}

// cli is main with a return value, so that the signal handler is unregistered on
// every path out. os.Exit does not run deferred calls, which is why the work is
// not done in main itself.
func cli() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "genbad:", err)
		return 1
	}
	return 0
}

// run holds the whole program so that it can be called from a test with its own
// context, arguments, environment and output. Nothing in here reaches for a
// global, which is what makes that possible.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("genbad", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", cfg.MetricsAddr, "listen address for the metrics endpoint, empty to serve no metrics")
	fs.StringVar((*string)(&cfg.Store), "store", string(cfg.Store), "storage driver: memory, sqlite, postgres or kura")
	fs.StringVar(&cfg.DSN, "dsn", cfg.DSN, "storage data source")
	fs.StringVar(&cfg.Tenant, "tenant", cfg.Tenant, "tenant served by a single tenant deployment")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug, info, warn or error")
	// A string rather than a repeated flag, because it is a list of a handful of
	// names that is written once in a unit file. Empty means nobody is an
	// administrator, which is the right default for a deployment that has not
	// said who operates it.
	admins := fs.String("admins", strings.Join(cfg.Admins, ","), "subjects that hold the administrator role, comma separated")

	var corpus corpusOptions
	fs.StringVar(&corpus.Dir, "corpus", "", "directory to index at startup")
	fs.StringVar(&corpus.Name, "corpus-name", "files", "source name the indexed directory carries")
	fs.StringVar(&corpus.ACL, "corpus-acl", aclTenant, "who may read the corpus: tenant, owners or os")
	fs.StringVar(&corpus.Identity, "corpus-identity", "unix", "identity source the account names in the tree belong to, for -corpus-acl os")
	fs.StringVar(&corpus.Domain, "corpus-domain", "", "domain the accounts on this host belong to, for -corpus-acl os, empty for none")
	fs.DurationVar(&corpus.Refresh, "corpus-refresh", 0, "how often to reindex the directory, zero for once")
	fs.BoolVar(&corpus.Watch, "corpus-watch", false, "ask the operating system what changed instead of walking the tree, needs -corpus-refresh")
	fs.DurationVar(&corpus.Reconcile, "corpus-reconcile", 0, "how often to sweep the index against the directory, zero for after every sync")

	var bucket bucketOptions
	fs.StringVar(&bucket.Bucket, "bucket", "", "S3 compatible bucket to index at startup")
	fs.StringVar(&bucket.Endpoint, "bucket-endpoint", "", "base URL of the object storage service, for example https://s3.eu-west-1.amazonaws.com")
	fs.StringVar(&bucket.Region, "bucket-region", "us-east-1", "region the bucket is in, which is part of what a signature authenticates")
	fs.StringVar(&bucket.Prefix, "bucket-prefix", "", "read only the keys under this prefix, empty for the whole bucket")
	fs.StringVar(&bucket.Name, "bucket-name", "objects", "source name the indexed objects carry")
	fs.StringVar(&bucket.ACL, "bucket-acl", aclTenant, "who may read the objects: tenant, bucket or object")
	fs.StringVar(&bucket.Identity, "bucket-identity", "", "identity source the names in the access control lists belong to, for -bucket-acl bucket or object")
	fs.StringVar(&bucket.Domain, "bucket-domain", "", "mail domain that counts as this tenant in a grant written against an address, empty for none")
	fs.BoolVar(&bucket.PathStyle, "bucket-path-style", false, "put the bucket in the path rather than in the host name, which MinIO and Ceph need")
	fs.DurationVar(&bucket.Refresh, "bucket-refresh", 0, "how often to list the bucket again, zero for once")
	fs.DurationVar(&bucket.Reconcile, "bucket-reconcile", 0, "how often to sweep the index against the bucket, zero for after every sync")
	fs.Float64Var(&bucket.Rate, "bucket-rate", limit.DefaultRate, "requests per second the crawl keeps itself under, there is no unlimited")
	fs.IntVar(&bucket.Burst, "bucket-burst", limit.DefaultBurst, "how many requests may go out back to back before the rate binds")
	fs.IntVar(&bucket.Retries, "bucket-retries", limit.DefaultMaxRetries, "how many times to try a refused request again, negative for never")

	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "genbad runs the genba server.\n\nUsage:\n  genbad [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nThe server flags also have environment variables, named GENBA_ plus the flag in upper case.\n")
		fmt.Fprintf(stderr, "The corpus and bucket flags do not, because what to index is typed once rather than deployed.\n")
		fmt.Fprintf(stderr, "Object storage credentials are the other way round and are read only from AWS_ACCESS_KEY_ID,\n")
		fmt.Fprintf(stderr, "AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN, because a secret in argv is readable by every\n")
		fmt.Fprintf(stderr, "process on the machine.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintf(stdout, "genbad %s (%s, built %s)\n", genba.Version, genba.Commit, genba.Date)
		return nil
	}
	cfg.Admins = subjects(*admins)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := corpus.validate(); err != nil {
		return err
	}
	bucket.credentials(getenv)
	if err := bucket.validate(); err != nil {
		return err
	}

	log := newLogger(stderr, cfg.LogLevel)

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing the store", "error", err)
		}
	}()

	// Shared by the feeds, which write to it, and the server, which reads it on
	// every stats request and every readiness check. It is built before either,
	// because the question it answers is whether this process came up with an
	// empty index and there is exactly one moment at which that can be asked.
	track := newIndexing(ctx, st)

	// The same arrangement for what the connectors are doing, which the
	// administration screen reads and nothing else does.
	ops := newOperations()

	opts := []api.Option{
		api.WithLogger(log),
		api.WithDriver(string(cfg.Store)),
		api.WithIndexing(track.State),
		api.WithOperations(ops.State),
	}
	if h := web.Handler(); h != nil {
		opts = append(opts, api.WithAssets(h))
	}

	// The searcher subscribes the cache to the store's writes, so it is closed
	// before the store is, and it holds nothing else.
	searcher := index.New(st, searchOptions(cfg)...)
	defer func() { _ = searcher.Close() }()

	// Built before anything is indexed rather than after, because the server
	// listens for the store's writes and the first sync is a write like any
	// other. Building it afterwards left it reporting that nothing had been
	// indexed since it came up while sitting on a corpus it had just loaded.
	srv := api.New(st, searcher, api.HeaderAuth{Tenant: cfg.Tenant, Admins: cfg.Admins}, opts...)

	// Each source starts syncing here and keeps going behind the listener. What
	// is built here and not in the background is everything that can be wrong
	// with a flag, so a bad corpus directory or a bucket with no credentials is
	// still an error the process exits on rather than a warning it logs a minute
	// later. A server pointed at both indexes both, and the two are separate
	// feeds with separate cursors rather than one merged crawl, because a bucket
	// that is refusing requests should not stop a directory being reindexed.
	waitForCorpus, err := ingestCorpus(ctx, st, corpus, cfg.Tenant, track, ops, log)
	if err != nil {
		return err
	}
	defer waitForCorpus()

	waitForBucket, err := ingestBucket(ctx, st, bucket, cfg.Tenant, track, ops, log)
	if err != nil {
		return err
	}
	defer waitForBucket()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	// The metrics endpoint gets its own listener, on its own address, and is off
	// unless one is configured. What it publishes is not secret and is not
	// public either, and the deployment that gets this right binds it somewhere
	// the outside cannot reach rather than mounting it on the API and relying on
	// a proxy rule to hide it again.
	servers := []*http.Server{httpSrv}
	if cfg.MetricsAddr != "" {
		servers = append(servers, &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           srv.Metrics(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
		})
	}

	log.Info("starting",
		"version", genba.Version,
		"addr", cfg.Addr,
		"metrics", cfg.MetricsAddr,
		"store", cfg.Store,
		"interface", web.Enabled(),
		"cache", cfg.Cache,
	)

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
				return
			}
			errc <- nil
		}()
	}

	// One listener falling over takes the other down with it. A process that
	// went on serving the API after the metrics address failed to bind is half
	// of what was asked for and looks entirely healthy from the outside.
	var first error
	running := len(servers)
	select {
	case first = <-errc:
		running--
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", cfg.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			// A shutdown that runs out of grace is worth reporting but is not a
			// failure of the process: the requests it was waiting on were
			// already over budget.
			log.Warn("shutdown did not finish in time", "addr", s.Addr, "error", err)
		}
	}

	// Every listener reports before this returns, so nothing is still serving a
	// request out of the store when the deferred close runs.
	for range running {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openStore builds the storage driver named in the configuration.
//
// The memory driver keeps nothing across a restart and the sqlite driver is one
// file, which covers a laptop and a single node. Postgres is the option for a
// shop that already runs one and would rather operate a database it knows than
// a new one. The drivers that need an engine this build was not compiled with
// are reported here rather than at the first query.
func openStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	switch cfg.Store {
	case config.StoreMemory:
		return memstore.New(), nil
	case config.StoreSQLite:
		return sqlitestore.Open(ctx, cfg.DSN)
	case config.StorePostgres:
		return pgstore.Open(ctx, cfg.DSN)
	default:
		return nil, fmt.Errorf("store %q is not available in this build", cfg.Store)
	}
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

// searchOptions builds the searcher from the configuration.
//
// A deployment that turns the cache off gets a searcher with no cache at all
// rather than a cache that holds nothing, so that the option is visible in a
// stack trace and in the stats response instead of being a set of layers
// reporting zero.
func searchOptions(cfg config.Config) []index.Option {
	if !cfg.Cache {
		return nil
	}
	return []index.Option{
		index.WithCache(index.NewCache(index.WithResultExpiry(cfg.CacheResultExpiry))),
	}
}

// subjects splits a comma separated list of names, dropping the empties.
//
// The empties matter. A flag left at its default is an empty string, and a
// naive split of that is a list holding one name that is nothing at all, which
// would make an unauthenticated request an administrator on any deployment
// where the subject is also empty.
func subjects(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
