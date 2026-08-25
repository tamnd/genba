package main

import (
	"fmt"
	"log/slog"

	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/config"
)

// openAudit builds the log every content access is written to.
//
// There is no branch in here that returns nothing. A deployment says where its
// records are kept, and the answer when it has not said anything is the process
// log rather than silence, because a trail that can be switched off in a
// configuration file is a trail nobody can rely on having.
//
// A directory that cannot be written is a startup failure. Coming up anyway and
// falling back to the log would be the same server serving the same content
// with a different promise about it, and the difference would be one line in a
// log nobody reads until they need the records that were never written.
func openAudit(cfg config.Config, log *slog.Logger) (*audit.Log, error) {
	if cfg.AuditDir == "" {
		return audit.New(audit.Logging(log)), nil
	}
	f, err := audit.Open(cfg.AuditDir, audit.WithRetention(cfg.AuditRetention))
	if err != nil {
		return nil, fmt.Errorf("opening the audit trail: %w", err)
	}
	return audit.New(f, audit.WithLogger(log)), nil
}

// auditDestination is what the startup line says about where the trail goes,
// which is the one thing an operator wants to read back after a deployment.
func auditDestination(cfg config.Config) string {
	if cfg.AuditDir == "" {
		return "log"
	}
	return cfg.AuditDir
}
