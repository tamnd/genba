package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tamnd/genba/acl"
)

// HeaderAuth reads a principal straight out of request headers.
//
// It exists for local development, for the conformance tests of anything that
// sits above the API, and for deployments where a trusted proxy in front of the
// process has already done the authentication and passes the result down. In
// that last case the proxy must strip these headers from anything it receives
// from the outside, because this type trusts them completely.
//
// It is not a login system, and it must never be reachable from an untrusted
// network. [Server] will happily run without any authenticator configured only
// in the sense that it will reject every request.
type HeaderAuth struct {
	// Tenant is used when the request does not carry one. A single tenant
	// deployment sets it here and never sends the header.
	Tenant string
}

// Header names read by [HeaderAuth].
const (
	HeaderTenant       = "X-Genba-Tenant"
	HeaderSubject      = "X-Genba-Subject"
	HeaderGroups       = "X-Genba-Groups"
	HeaderGroupVersion = "X-Genba-Group-Version"
	HeaderIdentities   = "X-Genba-Identities"
)

// Authenticate builds a principal from the headers.
func (h HeaderAuth) Authenticate(r *http.Request) (*acl.Principal, error) {
	subject := strings.TrimSpace(r.Header.Get(HeaderSubject))
	if subject == "" {
		return nil, ErrUnauthenticated
	}
	tenant := strings.TrimSpace(r.Header.Get(HeaderTenant))
	if tenant == "" {
		tenant = h.Tenant
	}
	if tenant == "" {
		return nil, ErrUnauthenticated
	}

	version, err := strconv.ParseUint(orDefault(r.Header.Get(HeaderGroupVersion), "1"), 10, 64)
	if err != nil {
		return nil, ErrUnauthenticated
	}

	p := &acl.Principal{
		Tenant:  tenant,
		Subject: subject,
		Kind:    acl.KindUser,
		Groups:  acl.GroupSet{Version: version, Members: splitList(r.Header.Get(HeaderGroups))},
	}
	for _, raw := range splitList(r.Header.Get(HeaderIdentities)) {
		source, value, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		p.Identities = append(p.Identities, acl.Identity{Source: source, Value: value})
	}
	return p, nil
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
