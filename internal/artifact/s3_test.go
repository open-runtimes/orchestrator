package artifact

import (
	"net/http"
	"net/url"
	"orchestrator/internal/config"
	"strings"
	"testing"
	"time"
)

func fixedClock(t *testing.T) {
	t.Helper()
	prev := s3Now
	s3Now = func() time.Time { return time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC) }
	t.Cleanup(func() { s3Now = prev })
}

var testCreds = config.S3Credentials{
	Region:          "us-east-1",
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "secret",
}

// The query-string signing example from the AWS SigV4 documentation
// ("Authenticating Requests: Using Query Parameters"), signature included.
func TestPresignQuery_AWSExample(t *testing.T) {
	creds := config.S3Credentials{
		Region:          "us-east-1",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	got := presignQuery(creds, http.MethodGet, "examplebucket.s3.amazonaws.com", "/test.txt",
		24*time.Hour, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Errorf("presignQuery =\n%s\nwant\n%s", got, want)
	}
}

func TestPresign_Addressing(t *testing.T) {
	fixedClock(t)
	withEndpoint := func(ep string) config.S3Credentials { c := testCreds; c.Endpoint = ep; return c }
	pathStyle := testCreds
	pathStyle.ForcePathStyle = true

	for _, tc := range []struct {
		name  string
		creds config.S3Credentials
		url   string
		want  string // scheme://host/path, before the query
	}{
		{"virtual-hosted", testCreds, "s3://mybucket/path/to/obj.txt", "https://mybucket.s3.us-east-1.amazonaws.com/path/to/obj.txt"},
		{"forced path style", pathStyle, "s3://mybucket/obj", "https://s3.us-east-1.amazonaws.com/mybucket/obj"},
		{"endpoint", withEndpoint("http://minio:9000"), "s3://mybucket/obj", "http://minio:9000/mybucket/obj"},
		{"endpoint prefix", withEndpoint("http://gw:9000/s3/"), "s3://b/k", "http://gw:9000/s3/b/k"},
		{"encoded key", testCreds, "s3://b/a b+c.txt", "https://b.s3.us-east-1.amazonaws.com/a%20b%2Bc.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signed, err := Presign(config.S3Profiles{"": tc.creds}, http.MethodGet, tc.url, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			base, query, _ := strings.Cut(signed, "?")
			if base != tc.want {
				t.Errorf("URL = %q, want %q", base, tc.want)
			}
			q, err := url.ParseQuery(query)
			if err != nil {
				t.Fatal(err)
			}
			if q.Get("X-Amz-Credential") != "AKIDEXAMPLE/20230102/us-east-1/s3/aws4_request" || q.Get("X-Amz-Expires") != "60" || len(q.Get("X-Amz-Signature")) != 64 {
				t.Errorf("query = %v", q)
			}
		})
	}
}

func TestPresign_SessionTokenIsSigned(t *testing.T) {
	fixedClock(t)
	creds := testCreds
	creds.SessionToken = "tok/en+="
	plain, _ := Presign(config.S3Profiles{"": testCreds}, http.MethodGet, "s3://b/k", time.Minute)
	signed, err := Presign(config.S3Profiles{"": creds}, http.MethodGet, "s3://b/k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	if u.Query().Get("X-Amz-Security-Token") != "tok/en+=" {
		t.Errorf("security token not carried: %s", signed)
	}
	if signature(t, signed) == signature(t, plain) {
		t.Error("the session token must be part of the signature")
	}
}

func TestPresign_Profiles(t *testing.T) {
	fixedClock(t)
	archive := testCreds
	archive.AccessKeyID = "AKIDARCHIVE"
	profiles := config.S3Profiles{"": testCreds, "archive": archive}

	signed, err := Presign(profiles, http.MethodPut, "s3://archive@b/k", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(signed); !strings.HasPrefix(u.Query().Get("X-Amz-Credential"), "AKIDARCHIVE/") {
		t.Errorf("signed with the wrong profile: %s", signed)
	}
	if _, err := Presign(profiles, http.MethodGet, "s3://missing@b/k", time.Minute); err == nil {
		t.Error("an unconfigured profile must not sign")
	}
	if _, err := Presign(config.S3Profiles{}, http.MethodGet, "s3://b/k", time.Minute); err == nil {
		t.Error("no credentials must not sign")
	}
	if _, err := Presign(profiles, http.MethodGet, "s3://b/", time.Minute); err == nil {
		t.Error("a URL without a key must not sign")
	}
}

func TestUriEncodePath(t *testing.T) {
	for in, want := range map[string]string{
		"a/b/c.txt":   "a/b/c.txt",
		"a b/c":       "a%20b/c",
		"x+y=z":       "x%2By%3Dz",
		"unreserved~": "unreserved~",
	} {
		if got := uriEncodePath(in); got != want {
			t.Errorf("uriEncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func signature(t *testing.T, signed string) string {
	t.Helper()
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("X-Amz-Signature")
}
