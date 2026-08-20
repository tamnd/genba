package objectsource

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The worked examples from the signature version 4 documentation, which are the
// only way to know an implementation of this is right rather than merely
// consistent with itself. A signer that agrees with nobody produces a request
// every service refuses, and the error it gets back says the signature did not
// match and nothing whatever about why.
//
// The credentials are the ones in the specification and have never been real.
const (
	exampleKeyID  = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	exampleHost   = "examplebucket.s3.amazonaws.com"
)

func TestTheSignatureMatchesTheWorkedExamples(t *testing.T) {
	when := time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC)
	cfg := Config{
		Region:          "us-east-1",
		AccessKeyID:     exampleKeyID,
		SecretAccessKey: exampleSecret,
	}

	for _, c := range []struct {
		name    string
		url     string
		headers map[string]string
		signed  string
		want    string
	}{
		{
			name:    "reading an object with a range",
			url:     "https://" + exampleHost + "/test.txt",
			headers: map[string]string{"Range": "bytes=0-9"},
			signed:  "host;range;x-amz-content-sha256;x-amz-date",
			want:    "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "listing a bucket, which is where the query is canonicalised",
			url:    "https://" + exampleHost + "/?max-keys=2&prefix=J",
			signed: "host;x-amz-content-sha256;x-amz-date",
			want:   "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
		{
			name:   "a subresource, which is a parameter with no value at all",
			url:    "https://" + exampleHost + "/?lifecycle",
			signed: "host;x-amz-content-sha256;x-amz-date",
			want:   "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}

			sign(req, emptyPayload, cfg, when)

			auth := req.Header.Get("Authorization")
			if got, want := field(auth, "Signature="), c.want; got != want {
				t.Errorf("signature is\n%s\nwant\n%s\nfrom %s", got, want, auth)
			}
			if got := field(auth, "SignedHeaders="); got != c.signed {
				t.Errorf("signed headers are %q, want %q", got, c.signed)
			}
			if want := "AWS4-HMAC-SHA256 Credential=" + exampleKeyID + "/20130524/us-east-1/s3/aws4_request"; !strings.HasPrefix(auth, want) {
				t.Errorf("the credential is %q, want it to start with %q", auth, want)
			}
		})
	}
}

// field pulls one comma separated part out of an Authorization header.
func field(auth, name string) string {
	for part := range strings.SplitSeq(auth, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), name); ok {
			return v
		}
	}
	return ""
}
