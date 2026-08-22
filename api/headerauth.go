package api

import (
	"net/http"
	"slices"
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

	// Admins are the subjects that hold [acl.RoleAdmin] whatever the request
	// says, named by whoever started the process.
	//
	// It is here rather than only in the roles header because of what the two
	// are for. The header is how a proxy that has already authenticated
	// somebody passes their roles down, and it is only as trustworthy as the
	// proxy stripping it. This is how a deployment with no proxy in front of it
	// still has an administrator, which is every first install and every
	// laptop, and it is a list somebody typed rather than a default: empty
	// means nobody is an administrator and the screens that need one are
	// refused. That is the right way round. A deployment that has not decided
	// who operates it has not decided, and guessing everybody is the answer
	// that cannot be taken back.
	Admins []string
}

// Header names read by [HeaderAuth].
const (
	HeaderTenant       = "X-Genba-Tenant"
	HeaderSubject      = "X-Genba-Subject"
	HeaderGroups       = "X-Genba-Groups"
	HeaderGroupVersion = "X-Genba-Group-Version"
	HeaderIdentities   = "X-Genba-Identities"
	HeaderRoles        = "X-Genba-Roles"
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
		Groups:  acl.GroupSet{Version: version, Members: splitCSV(r.Header.Get(HeaderGroups))},
		Roles:   splitCSV(r.Header.Get(HeaderRoles)),
	}
	// The configured list wins over the header in the only direction that
	// matters: it can add the role and it never takes it away, so a proxy that
	// hands down roles and an operator who named themselves at startup do not
	// have to agree.
	if slices.Contains(h.Admins, subject) && !p.HasRole(acl.RoleAdmin) {
		p.Roles = append(p.Roles, acl.RoleAdmin)
	}
	for _, raw := range splitCSV(r.Header.Get(HeaderIdentities)) {
		source, value, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		p.Identities = append(p.Identities, acl.Identity{Source: source, Value: value})
	}
	return p, nil
}

// splitCSV reads a comma separated header value, dropping empty entries.
func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
