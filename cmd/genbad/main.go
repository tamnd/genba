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

	st, err := openStore(cfg)
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
	srv := api.New(st, index.New(st), api.HeaderAuth{Tenant: cfg.Tenant}, opts...)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	log.Info("starting",
		"version", genba.Version,
		"addr", cfg.Addr,
		"store", cfg.Store,
		"interface", web.Enabled(),
	)

	errc := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", cfg.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		// A shutdown that runs out of grace is worth reporting but is not a
		// failure of the process: the requests it was waiting on were already
		// over budget.
		log.Warn("shutdown did not finish in time", "error", err)
	}
	return <-errc
}

// openStore builds the storage driver named in the configuration.
//
// Drivers other than the in memory one arrive with their own build tags, and
// this is where an unavailable one is reported. The message says what to do
// about it, because a person hitting this has usually downloaded a build that
// does not carry the driver they configured.
func openStore(cfg config.Config) (store.Store, error) {
	switch cfg.Store {
	case config.StoreMemory:
		return memstore.New(), nil
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
