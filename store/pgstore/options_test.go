package pgstore

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestDefaultsSurviveADSNThatSaysNothing is the case every deployment starts
// from. A driver whose defaults were zero would open a pool of no connections
// and retry nothing.
func TestDefaultsSurviveADSNThatSaysNothing(t *testing.T) {
	got, clean, err := ParseOptions("postgres://genba@db.example.com:5432/genba?sslmode=verify-full")
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	if got != DefaultOptions() {
		t.Fatalf("a DSN with no settings produced %+v, want the defaults", got)
	}
	if !strings.Contains(clean, "sslmode=verify-full") {
		t.Fatalf("the settings pass stripped something that was not ours: %q", clean)
	}
}

func TestSettingsComeOutOfTheDSN(t *testing.T) {
	const dsn = "postgres://genba@db.example.com:5432/genba?sslmode=require" +
		"&pool_max_conns=32&pool_min_conns=4" +
		"&pool_max_conn_lifetime=2h&pool_max_conn_idle_time=5m" +
		"&genba_attempts=5&genba_backoff=100ms"

	got, clean, err := ParseOptions(dsn)
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	want := Options{
		MaxConns: 32, MinConns: 4,
		MaxConnLifetime: 2 * time.Hour, MaxConnIdleTime: 5 * time.Minute,
		ConnectTimeout: DefaultOptions().ConnectTimeout, StatementTimeout: DefaultOptions().StatementTimeout,
		Attempts: 5, Backoff: 100 * time.Millisecond,
	}
	if got != want {
		t.Fatalf("ParseOptions returned\n got %+v\nwant %+v", got, want)
	}
	// The settings are this driver's and the server has never heard of them, so
	// leaving them in the string would be a connection error at startup.
	for _, key := range settings {
		if strings.Contains(clean, key) {
			t.Fatalf("%s was left in the connection string: %q", key, clean)
		}
	}
	if !strings.Contains(clean, "sslmode=require") {
		t.Fatalf("the settings pass stripped something that was not ours: %q", clean)
	}
}

// TestKeywordDSNsWork covers the shape a shop with an existing Postgres already
// has written down somewhere. Refusing it would mean asking them to rewrite a
// working connection string to try this driver.
func TestKeywordDSNsWork(t *testing.T) {
	got, clean, err := ParseOptions("host=db.example.com user=genba dbname=genba pool_max_conns=8 genba_attempts=1")
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	if got.MaxConns != 8 || got.Attempts != 1 {
		t.Fatalf("ParseOptions returned %+v", got)
	}
	if clean != "host=db.example.com user=genba dbname=genba" {
		t.Fatalf("the remaining connection string is %q", clean)
	}
}

func TestBadSettingsAreRefusedBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"a pool of nothing", "postgres://db/genba?pool_max_conns=0", "at least one connection"},
		{"a minimum above the maximum", "postgres://db/genba?pool_max_conns=2&pool_min_conns=4", "pool_min_conns"},
		{"no attempts at all", "postgres://db/genba?genba_attempts=0", "tried at least once"},
		{"a duration with no unit", "postgres://db/genba?genba_backoff=100", "needs a unit"},
		{"a count that is not a number", "postgres://db/genba?pool_max_conns=lots", "is not a number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParseOptions(c.dsn)
			if err == nil {
				t.Fatalf("ParseOptions accepted %q", c.dsn)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error says %q, and an operator reading it needs to see %q", err, c.want)
			}
		})
	}
}

// TestRetryableErrors pins the list down in both directions. Retrying a
// constraint violation turns one clear error into three of the same error and a
// slower answer, and not retrying a failover turns a promotion into an outage.
func TestRetryableErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nothing went wrong", nil, false},
		{"the caller gave up", context.Canceled, false},
		{"the caller ran out of time", context.DeadlineExceeded, false},
		{"a serialisation conflict", &pgconn.PgError{Code: "40001"}, true},
		{"a deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"the administrator restarted the cluster", &pgconn.PgError{Code: "57P01"}, true},
		{"a connection that broke", &pgconn.PgError{Code: "08006"}, true},
		{"a unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"a syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"permission denied", &pgconn.PgError{Code: "42501"}, false},
		{"a network error", &net.DNSError{IsTemporary: true}, true},
		{"something wrapped", errors.Join(errors.New("put: "), &pgconn.PgError{Code: "40001"}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryable(c.err); got != c.want {
				t.Fatalf("retryable(%v) is %v, want %v", c.err, got, c.want)
			}
		})
	}
}
