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
	"syscall"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/config"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
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

	var corpus corpusOptions
	fs.StringVar(&corpus.Dir, "corpus", "", "directory to index at startup")
	fs.StringVar(&corpus.Name, "corpus-name", "files", "source name the indexed directory carries")
	fs.StringVar(&corpus.ACL, "corpus-acl", aclTenant, "who may read the corpus: tenant or owners")
	fs.DurationVar(&corpus.Refresh, "corpus-refresh", 0, "how often to reindex the directory, zero for once")

	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "genbad runs the genba server.\n\nUsage:\n  genbad [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nEvery flag also has an environment variable, named GENBA_ plus the flag in upper case.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintf(stdout, "genbad %s (%s, built %s)\n", genba.Version, genba.Commit, genba.Date)
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := corpus.validate(); err != nil {
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

	// The first sync runs here, before the listener opens, so that the server is
	// useful the moment it says it is up.
	waitForCorpus, err := ingestCorpus(ctx, st, corpus, cfg.Tenant, log)
	if err != nil {
		return err
	}
	defer waitForCorpus()

	opts := []api.Option{api.WithLogger(log)}
	if h := web.Handler(); h != nil {
		opts = append(opts, api.WithAssets(h))
	}

	// The searcher subscribes the cache to the store's writes, so it is closed
	// before the store is, and it holds nothing else.
	searcher := index.New(st, searchOptions(cfg)...)
	defer func() { _ = searcher.Close() }()

	srv := api.New(st, searcher, api.HeaderAuth{Tenant: cfg.Tenant}, opts...)

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
// file, which covers a laptop and a single node. The drivers that need a server
// to talk to are not in this build yet, and this is where that is reported
// rather than at the first query.
func openStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	switch cfg.Store {
	case config.StoreMemory:
		return memstore.New(), nil
	case config.StoreSQLite:
		return sqlitestore.Open(ctx, cfg.DSN)
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
