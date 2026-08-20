//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// account is the name the operating system policy will resolve this process to,
// which is the name a request has to arrive under for any of it to match.
func account(t *testing.T) string {
	t.Helper()
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Skipf("this machine cannot name the account it is running as: %v", err)
	}
	return u.Username
}

// TestTheFileSystemDecidesWhatAQueryReturns is the operating system policy
// through the front door. The tree is the file server, the mode bits are the
// access control system, and a query gets back what the person asking can open
// with a shell on the same machine.
func TestTheFileSystemDecidesWhatAQueryReturns(t *testing.T) {
	root := corpusTree(t)
	me := account(t)

	// Nobody but the owner, which under this policy is a document with one
	// name on it rather than a list.
	if err := os.Chmod(filepath.Join(root, "guides", "legacy.md"), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-corpus", root,
			"-corpus-name", "handbook",
			"-corpus-acl", aclOS,
			// The identity source has to be the one the front door issues
			// identifiers under, or the names on the files and the names on the
			// request are two different vocabularies that happen to look alike.
			"-corpus-identity", "github",
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	if got := searchAs(t, addr, me, "legacy"); got.Total == 0 {
		t.Error("the owner of a file cannot find it")
	}
	if got := searchAs(t, addr, "mallory", "legacy"); got.Total != 0 {
		t.Errorf("somebody with no account on this machine was shown %v", got.Hits)
	}
	if got := searchAs(t, addr, "mallory", "deploying"); got.Total != 0 {
		t.Errorf("a file readable by one host's accounts was shown to a stranger: %v", got.Hits)
	}
}

func TestTheOperatingSystemPolicyIsBuiltFromTheFlags(t *testing.T) {
	root := corpusTree(t)
	opts := corpusOptions{Dir: root, Name: "handbook", ACL: aclOS, Identity: "unix"}
	if err := opts.validate(); err != nil {
		t.Fatalf("a usable set was rejected: %v", err)
	}

	policy, err := policyFor(opts)
	if err != nil {
		t.Fatalf("building the policy: %v", err)
	}
	perm, err := policy.Permissions(t.Context(), "README.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if perm.Owner.Source != "unix" {
		t.Errorf("the owner is written under %q, want the identity source the flag named", perm.Owner.Source)
	}

	// And the domain flag reaches the policy, which for a world readable file
	// is the difference between a list of two names and the whole tenant.
	opts.Domain = "acme.example"
	told, err := policyFor(opts)
	if err != nil {
		t.Fatalf("building the policy: %v", err)
	}
	widened, err := told.Permissions(t.Context(), "README.md")
	if err != nil {
		t.Fatalf("permissions: %v", err)
	}
	if widened.Mode == perm.Mode {
		t.Errorf("naming the domain left a world readable file at %v", perm.Mode)
	}
}
