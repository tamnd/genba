package objectsource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Signature version 4 is the one thing every S3 compatible service agrees on.
//
// It is written out here rather than taken from a vendor SDK because the whole
// of what this connector needs is one function over an *http.Request, and the
// SDK that provides it arrives with a credential chain, a retry policy, a
// middleware stack and several hundred other services attached. The algorithm
// has not changed since 2012 and sign_test.go pins it against the worked
// examples in the specification, so owning it is a known quantity in a way that
// owning the dependency is not.

const (
	// signingAlgorithm names the scheme in the Authorization header.
	signingAlgorithm = "AWS4-HMAC-SHA256"

	// signingService is what the credential scope calls this API. Every S3
	// compatible service uses the same word, whatever it calls itself.
	signingService = "s3"

	// emptyPayload is the SHA-256 of no bytes at all, which is what every
	// request this connector makes carries, because it only ever reads.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// sign authenticates a request in place.
//
// It rewrites the query string as well as adding headers. The signature covers
// a canonical form of the query, and the only way to be certain the signed form
// is the sent form is to send the signed one, rather than to hope that whoever
// built the URL encoded it the same way.
func sign(r *http.Request, hashedPayload string, cfg Config, now time.Time) {
	stamp := now.UTC().Format("20060102")
	timestamp := now.UTC().Format("20060102T150405Z")

	r.Header.Set("X-Amz-Date", timestamp)
	r.Header.Set("X-Amz-Content-Sha256", hashedPayload)
	if cfg.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", cfg.SessionToken)
	}
	r.URL.RawQuery = canonicalQuery(r.URL.Query())

	names, headers := canonicalHeaders(r)
	canonical := strings.Join([]string{
		r.Method,
		canonicalURI(r),
		r.URL.RawQuery,
		headers,
		names,
		hashedPayload,
	}, "\n")

	scope := stamp + "/" + cfg.Region + "/" + signingService + "/aws4_request"
	toSign := strings.Join([]string{
		signingAlgorithm,
		timestamp,
		scope,
		hashHex([]byte(canonical)),
	}, "\n")

	key := chain([]byte("AWS4"+cfg.SecretAccessKey), stamp, cfg.Region, signingService, "aws4_request")
	signature := hex.EncodeToString(mac(key, toSign))

	r.Header.Set("Authorization", signingAlgorithm+
		" Credential="+cfg.AccessKeyID+"/"+scope+
		", SignedHeaders="+names+
		", Signature="+signature)
}

// canonicalURI is the path as it will appear on the wire.
//
// It reads the escaped path rather than escaping the path again, because the
// two have to be the same string and the request has already been built. The
// encoding that gets them to agree is in [escape], and the client sets it as
// the URL's raw path so that this and the transport read the same bytes.
func canonicalURI(r *http.Request) string {
	if p := r.URL.EscapedPath(); p != "" {
		return p
	}
	return "/"
}

// canonicalQuery sorts and encodes a query the way the signature expects.
//
// Parameters are ordered by name and then by value, and everything outside the
// unreserved set is percent encoded, including the slash. That last part is why
// url.Values.Encode is not used: it writes a space as a plus, which the
// signature reads as a literal plus.
func canonicalQuery(q url.Values) string {
	parts := make([]string, 0, len(q))
	for _, k := range slices.Sorted(maps.Keys(q)) {
		values := slices.Clone(q[k])
		slices.Sort(values)
		for _, v := range values {
			parts = append(parts, escape(k, false)+"="+escape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalHeaders returns the signed header names and the block of headers
// they refer to.
//
// Every header on the request is signed, plus the host, which is the point of
// the exercise: a proxy that rewrote the bucket out of the host would produce a
// request that no longer verifies.
func canonicalHeaders(r *http.Request) (names, block string) {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	values := make(map[string]string, len(r.Header)+1)
	values["host"] = host
	for k, v := range r.Header {
		name := strings.ToLower(k)
		// The header being built is not part of what it signs.
		if name == "authorization" {
			continue
		}
		values[name] = strings.Join(collapse(v), ",")
	}

	keys := slices.Sorted(maps.Keys(values))
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(values[k])
		b.WriteByte('\n')
	}
	return strings.Join(keys, ";"), b.String()
}

// collapse trims each header value and squeezes runs of spaces inside it down
// to one, which is what the signature is calculated over.
func collapse(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strings.Join(strings.Fields(v), " "))
	}
	return out
}

// hexDigits is upper case because percent escapes in a canonical request are.
const hexDigits = "0123456789ABCDEF"

// escape is the percent encoding the signature is defined in terms of, which is
// RFC 3986 and not the one net/url writes.
//
// A path keeps its slashes and a query parameter does not, which is the only
// difference between the two uses.
func escape(s string, keepSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		}
	}
	return b.String()
}

// chain derives the signing key, which is a hash of the secret narrowed one
// step at a time to a day, a region and a service.
//
// The narrowing is the reason a signature that leaks is worth so much less than
// the key: it only authenticates that day, in that region, for that service.
func chain(key []byte, parts ...string) []byte {
	for _, p := range parts {
		key = mac(key, p)
	}
	return key
}

func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
