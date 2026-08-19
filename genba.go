package genba

import "errors"

// Version is the build version. Releases overwrite it through the linker.
var Version = "dev"

// Commit is the git revision the binary was built from.
var Commit = "none"

// Date is the build timestamp in RFC 3339.
var Date = "unknown"

// TenantID names one deployment's data. Single tenant deployments still carry
// one so that the multi tenant control plane and the single tenant server share
// exactly one code path.
type TenantID string

// SubjectID is the stable internal identifier of a person or a service.
type SubjectID string

// DocID is the identifier of an indexed document, unique within a tenant.
type DocID string

// Errors returned across package boundaries. Callers match with errors.Is
// rather than comparing strings.
var (
	// ErrNotFound is returned when a named object does not exist. It is also
	// returned when the caller may not see the object, so that a caller cannot
	// use the difference between "missing" and "forbidden" to prove that
	// something exists.
	ErrNotFound = errors.New("genba: not found")

	// ErrNoPrincipal is returned when a call that reads content was made
	// without an authenticated subject.
	ErrNoPrincipal = errors.New("genba: no principal")

	// ErrUnsupported is returned by a storage driver or a connector for a
	// capability it does not implement.
	ErrUnsupported = errors.New("genba: unsupported")

	// ErrClosed is returned after the receiver has been closed.
	ErrClosed = errors.New("genba: closed")
)
