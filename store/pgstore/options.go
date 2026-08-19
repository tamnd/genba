package pgstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Options are the pool, timeout and retry settings.
//
// They come out of the DSN rather than out of a second configuration block,
// because the whole point of the storage layer is that a deployment picks a
// driver with one string and nothing else in the process knows which one it
// got. A driver that needed six more environment variables would be a driver
// that leaked into config, cmd and the documentation for both.
//
// Two of these are not read here at all. connect_timeout and statement_timeout
// are spelled the way libpq and the server spell them, so pgx and Postgres
// handle them directly, and the values below are only the defaults applied when
// the DSN is silent about them.
type Options struct {
	// MaxConns is the size of the pool. The default is deliberately small: a
	// search server's concurrency lives in the number of requests in flight,
	// and a pool larger than the database has cores turns a queue that is
	// visible here into contention that is only visible there.
	MaxConns int32

	// MinConns is how many connections are kept open when nothing is
	// happening, so that the first request after an idle period does not pay
	// for a TLS handshake and an authentication round trip.
	MinConns int32

	// MaxConnLifetime retires a connection regardless of health. It is what
	// lets a rolling restart of a connection pooler or a failover drain
	// through without every server having to be restarted.
	MaxConnLifetime time.Duration

	// MaxConnIdleTime closes a connection nobody has used.
	MaxConnIdleTime time.Duration

	// ConnectTimeout bounds one attempt to open a connection. It is the
	// default for the DSN's connect_timeout.
	ConnectTimeout time.Duration

	// StatementTimeout is the server side limit on a single statement, and it
	// is the default for the DSN's statement_timeout.
	//
	// It is set at all because the failure it prevents is the bad one. A query
	// that hangs holds a connection out of a pool that has a handful of them,
	// so one pathological statement becomes an outage of everything else. Five
	// seconds is far beyond the ten millisecond budget the API gate holds this
	// to, which is the point: it is a backstop, not a policy.
	StatementTimeout time.Duration

	// Attempts is how many times an operation is tried before its error is
	// returned. One means no retry.
	//
	// It exists because the errors it covers are not failures of the query. A
	// failover, a pooler restart and a serialisation conflict are all things
	// the same statement will succeed at a moment later, and a search that
	// returns a five hundred because a standby was promoted is a worse product
	// than one that waits fifty milliseconds.
	Attempts int

	// Backoff is the wait before the second attempt, and it doubles for each
	// one after that.
	Backoff time.Duration
}

// DefaultOptions are what a DSN that says nothing gets.
func DefaultOptions() Options {
	return Options{
		MaxConns:         16,
		MinConns:         2,
		MaxConnLifetime:  time.Hour,
		MaxConnIdleTime:  30 * time.Minute,
		ConnectTimeout:   10 * time.Second,
		StatementTimeout: 5 * time.Second,
		Attempts:         3,
		Backoff:          25 * time.Millisecond,
	}
}

// Validate reports the first setting that would make a pool misbehave later.
func (o Options) Validate() error {
	switch {
	case o.MaxConns < 1:
		return fmt.Errorf("pgstore: pool_max_conns is %d, and a pool needs at least one connection", o.MaxConns)
	case o.MinConns < 0:
		return fmt.Errorf("pgstore: pool_min_conns is %d", o.MinConns)
	case o.MinConns > o.MaxConns:
		return fmt.Errorf("pgstore: pool_min_conns is %d and pool_max_conns is %d", o.MinConns, o.MaxConns)
	case o.Attempts < 1:
		return fmt.Errorf("pgstore: genba_attempts is %d, and an operation is tried at least once", o.Attempts)
	case o.Backoff < 0:
		return errors.New("pgstore: genba_backoff is negative")
	}
	return nil
}

// settings are the DSN keys this driver reads and removes before pgx sees the
// string. Everything else in a DSN is pgx's or the server's and is passed
// through untouched.
var settings = []string{
	"pool_max_conns",
	"pool_min_conns",
	"pool_max_conn_lifetime",
	"pool_max_conn_idle_time",
	"genba_attempts",
	"genba_backoff",
}

// ParseOptions reads the settings out of a DSN and returns the DSN without
// them.
//
// It is exported and takes a string rather than being folded into Open so that
// a deployment can be checked without a database to connect to, which is what
// the test for it does and what a startup check would do.
func ParseOptions(dsn string) (Options, string, error) {
	values, clean, err := split(dsn)
	if err != nil {
		return Options{}, "", err
	}

	o := DefaultOptions()
	var errs []error
	num := func(key string, dst *int32) {
		v, ok := values[key]
		if !ok {
			return
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("pgstore: %s = %q is not a number", key, v))
			return
		}
		*dst = int32(n)
	}
	dur := func(key string, dst *time.Duration) {
		v, ok := values[key]
		if !ok {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			// A bare number is a common mistake and a plausible intent, so say
			// what the value would have to look like rather than just rejecting
			// it. config does the same for the same reason.
			if _, numErr := strconv.Atoi(v); numErr == nil {
				errs = append(errs, fmt.Errorf("pgstore: %s = %q needs a unit, for example %ss", key, v, v))
				return
			}
			errs = append(errs, fmt.Errorf("pgstore: %s: %w", key, err))
			return
		}
		*dst = d
	}

	num("pool_max_conns", &o.MaxConns)
	num("pool_min_conns", &o.MinConns)
	dur("pool_max_conn_lifetime", &o.MaxConnLifetime)
	dur("pool_max_conn_idle_time", &o.MaxConnIdleTime)
	dur("genba_backoff", &o.Backoff)
	if v, ok := values["genba_attempts"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("pgstore: genba_attempts = %q is not a number", v))
		} else {
			o.Attempts = n
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Options{}, "", err
	}
	if err := o.Validate(); err != nil {
		return Options{}, "", err
	}
	return o, clean, nil
}

// split pulls this driver's settings out of a DSN in either of the two shapes
// people write one in, and hands back the rest.
//
// The URL shape is what the documentation uses and what an operator pastes out
// of a secret store. The keyword shape is what libpq has always accepted and
// what a shop with an existing Postgres install already has in a file
// somewhere, so refusing it would mean asking them to rewrite a working
// connection string to try this driver.
func split(dsn string) (values map[string]string, rest string, err error) {
	values = map[string]string{}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, "", fmt.Errorf("pgstore: %w", err)
		}
		q := u.Query()
		for _, key := range settings {
			if q.Has(key) {
				values[key] = q.Get(key)
				q.Del(key)
			}
		}
		u.RawQuery = q.Encode()
		return values, u.String(), nil
	}

	// The keyword shape, which is space separated key=value. A value may be
	// quoted, and none of this driver's are, so a field that does not split on
	// the first equals sign is somebody else's and is passed through whole.
	var kept []string
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if ok && contains(settings, key) {
			values[key] = value
			continue
		}
		kept = append(kept, field)
	}
	return values, strings.Join(kept, " "), nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// retryable reports whether an error is one that the same statement would
// plausibly succeed at a moment later.
//
// The list is short on purpose. A constraint violation, a syntax error or a
// permission denial is a bug or a misconfiguration, and retrying it three times
// turns one clear error into three of the same error and a slower response.
// What is here is a database that moved: a failover, a pooler that recycled the
// connection, an administrator restarting the cluster, and the two isolation
// errors that mean another transaction won a race.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller's decision, and a retry
	// would ignore it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pge *pgconn.PgError
	if errors.As(err, &pge) {
		switch pge.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"57P01", // admin_shutdown
			"57P02", // crash_shutdown
			"57P03", // cannot_connect_now
			"57P05", // idle_session_timeout
			"58030": // io_error
			return true
		}
		// 08 is the connection exception class, all of which mean the
		// connection rather than the statement.
		return strings.HasPrefix(pge.Code, "08")
	}
	if pgconn.SafeToRetry(err) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
