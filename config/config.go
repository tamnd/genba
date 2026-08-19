// Package config holds the runtime configuration and the rules for loading it.
//
// The order is defaults, then a file, then the environment, then flags, and
// each layer only overrides what it actually sets. That order is the one
// operators expect: a file describes an install, the environment describes a
// deployment of it, and a flag is what somebody types once while debugging.
//
// [Config.Validate] is the only place a bad configuration is rejected, and it
// is called by [Load]. Nothing downstream re checks a field, because a config
// that reached a server is a config that passed.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Store names the storage driver to run on.
type Store string

// The storage drivers that can be selected at runtime. Which of them are
// actually compiled in depends on build tags, and [Config.Validate] does not
// know about that: the server reports an unavailable driver, because that is a
// build question rather than a configuration question.
const (
	StoreMemory   Store = "memory"
	StoreSQLite   Store = "sqlite"
	StorePostgres Store = "postgres"
	StoreKura     Store = "kura"
)

// Config is everything a server needs to start.
type Config struct {
	// Addr is the listen address of the HTTP server.
	Addr string

	// Store selects the storage driver.
	Store Store

	// DSN is the driver's data source. It is a path for sqlite and kura and a
	// connection string for postgres. It is unused by the memory driver.
	DSN string

	// Tenant is the tenant a single tenant deployment serves. Multi tenant
	// deployments leave it empty and resolve the tenant per request.
	Tenant string

	// ReadTimeout and WriteTimeout bound a request. They exist so that one slow
	// connector or one enormous export cannot hold a connection open forever.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// ShutdownGrace is how long a shutdown waits for in flight requests.
	ShutdownGrace time.Duration

	// LogLevel is one of debug, info, warn or error.
	LogLevel string

	// Cache turns the query caches on. Running without them is supported and
	// correct: it costs latency on a repeated query and it costs nothing else,
	// which is the property that makes a cache safe to have in the first place.
	Cache bool

	// CacheResultExpiry is the backstop on how long a ranked ordering may be
	// reused. A write to the tenant drops its orderings whatever this says, so
	// this is what bounds a driver that cannot report its writes.
	//
	// Zero turns result caching off while leaving the layers that are bounded by
	// a write alone, which is what a deployment that cannot tolerate a stale
	// ordering asks for.
	CacheResultExpiry time.Duration
}

// Default returns the configuration a server starts with when nothing is set.
// It is a working single node install that keeps nothing on disk, which is the
// right default for somebody trying the binary for the first time.
func Default() Config {
	return Config{
		Addr:          "127.0.0.1:8080",
		Store:         StoreMemory,
		ReadTimeout:   30 * time.Second,
		WriteTimeout:  60 * time.Second,
		ShutdownGrace: 15 * time.Second,
		LogLevel:      "info",
		Cache:         true,
		// The same thirty seconds as index.DefaultResultExpiry. It is written
		// again rather than imported because configuration sits below everything
		// and importing the query layer to read one constant would invert that.
		CacheResultExpiry: 30 * time.Second,
	}
}

// Load returns the default configuration with the environment applied on top.
//
// Every variable is named GENBA_ plus the field in upper snake case. A variable
// that is not set leaves the default alone, and a variable that is set to an
// unparseable value is an error rather than a silent fallback.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Default()

	str(getenv, "GENBA_ADDR", &c.Addr)
	str(getenv, "GENBA_DSN", &c.DSN)
	str(getenv, "GENBA_TENANT", &c.Tenant)
	str(getenv, "GENBA_LOG_LEVEL", &c.LogLevel)
	if v := getenv("GENBA_STORE"); v != "" {
		c.Store = Store(strings.ToLower(v))
	}

	err := errors.Join(
		dur(getenv, "GENBA_READ_TIMEOUT", &c.ReadTimeout),
		dur(getenv, "GENBA_WRITE_TIMEOUT", &c.WriteTimeout),
		dur(getenv, "GENBA_SHUTDOWN_GRACE", &c.ShutdownGrace),
		dur(getenv, "GENBA_CACHE_RESULT_EXPIRY", &c.CacheResultExpiry),
		boolean(getenv, "GENBA_CACHE", &c.Cache),
	)
	if err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate reports the first thing about the configuration that would make a
// server misbehave later.
func (c Config) Validate() error {
	errs := make([]error, 0, 4)
	if c.Addr == "" {
		errs = append(errs, errors.New("config: addr is empty"))
	}
	switch c.Store {
	case StoreMemory, StoreSQLite, StorePostgres, StoreKura:
	default:
		errs = append(errs, fmt.Errorf("config: unknown store %q", c.Store))
	}
	if c.Store != StoreMemory && c.DSN == "" {
		errs = append(errs, fmt.Errorf("config: store %q needs a dsn", c.Store))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("config: unknown log level %q", c.LogLevel))
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"read timeout", c.ReadTimeout},
		{"write timeout", c.WriteTimeout},
		{"shutdown grace", c.ShutdownGrace},
		{"cache result expiry", c.CacheResultExpiry},
	} {
		if d.value < 0 {
			errs = append(errs, fmt.Errorf("config: %s is negative", d.name))
		}
	}
	return errors.Join(errs...)
}

// Redacted returns the configuration with secrets removed, which is what gets
// logged at startup. The DSN is the only field that routinely carries a
// password, and an operator still needs to see that one was set.
func (c Config) Redacted() Config {
	if c.DSN != "" {
		c.DSN = "[set]"
	}
	return c
}

func str(getenv func(string) string, key string, dst *string) {
	if v := getenv(key); v != "" {
		*dst = v
	}
}

// boolean reads a flag. The accepted spellings are the ones people type in a
// unit file, rather than only the two Go parses.
func boolean(getenv func(string) string, key string, dst *bool) error {
	v := strings.ToLower(strings.TrimSpace(getenv(key)))
	switch v {
	case "":
		return nil
	case "1", "t", "true", "yes", "on":
		*dst = true
	case "0", "f", "false", "no", "off":
		*dst = false
	default:
		return fmt.Errorf("config: %s = %q is not a yes or a no", key, v)
	}
	return nil
}

func dur(getenv func(string) string, key string, dst *time.Duration) error {
	v := getenv(key)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// A bare number is a common mistake and a plausible intent, so say what
		// the value would have to look like instead of just rejecting it.
		if _, numErr := strconv.Atoi(v); numErr == nil {
			return fmt.Errorf("config: %s = %q needs a unit, for example %ss", key, v, v)
		}
		return fmt.Errorf("config: %s: %w", key, err)
	}
	*dst = d
	return nil
}
