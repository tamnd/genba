package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/config"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestDefaultIsValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("the default configuration does not validate: %v", err)
	}
}

func TestLoadAppliesTheEnvironment(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"GENBA_ADDR":           ":9000",
		"GENBA_STORE":          "SQLite",
		"GENBA_DSN":            "/var/lib/genba/genba.db",
		"GENBA_READ_TIMEOUT":   "5s",
		"GENBA_SHUTDOWN_GRACE": "1m",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":9000" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.Store != config.StoreSQLite {
		t.Errorf("Store = %q, the name should be case insensitive", c.Store)
	}
	if c.ReadTimeout != 5*time.Second || c.ShutdownGrace != time.Minute {
		t.Errorf("timeouts = %v and %v", c.ReadTimeout, c.ShutdownGrace)
	}
	if c.WriteTimeout != config.Default().WriteTimeout {
		t.Errorf("an unset variable overwrote the default: WriteTimeout = %v", c.WriteTimeout)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unknown store", map[string]string{"GENBA_STORE": "cassandra"}, "unknown store"},
		{"store without a dsn", map[string]string{"GENBA_STORE": "postgres"}, "needs a dsn"},
		{"unknown log level", map[string]string{"GENBA_LOG_LEVEL": "trace"}, "unknown log level"},
		{"duration without a unit", map[string]string{"GENBA_READ_TIMEOUT": "30"}, "needs a unit"},
		{"unparseable duration", map[string]string{"GENBA_READ_TIMEOUT": "soon"}, "GENBA_READ_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(env(tt.env))
			if err == nil {
				t.Fatal("Load accepted the value")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestRedactedHidesTheDSN(t *testing.T) {
	c := config.Default()
	c.DSN = "postgres://genba:hunter2@db/genba"
	if got := c.Redacted().DSN; strings.Contains(got, "hunter2") {
		t.Fatalf("Redacted kept the password: %q", got)
	}
	if config.Default().Redacted().DSN != "" {
		t.Fatal("Redacted invented a dsn that was never set")
	}
}
