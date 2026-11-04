package artifact

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"orchestrator/internal/config"
	"strconv"
	"strings"
	"time"
)

// s3Scheme is the URL scheme that names an object by bucket and key rather than
// by address; it is pre-signed into a plain https URL before it is fetched.
const s3Scheme = "s3"

// isS3URL reports whether rawURL uses the s3:// scheme.
func isS3URL(rawURL string) bool {
	return strings.HasPrefix(strings.ToLower(rawURL), s3Scheme+"://")
}

// s3Now is overridable in tests; production uses the wall clock.
var s3Now = time.Now

// Presign returns a URL that performs method on the object an
// s3://[profile@]bucket/key URL names, signed with AWS Signature Version 4 in
// the query string and valid for ttl. The body is not signed, so an upload
// sends the file as is. Only the services hold the profiles; a pod asks for
// a URL through PresignHandler.
func Presign(profiles config.S3Profiles, method, rawURL string, ttl time.Duration) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid s3 URL: %w", err)
	}
	profile := u.User.Username()
	creds := profiles[profile]
	if !creds.Enabled() {
		return "", fmt.Errorf("s3 URL %q: S3 profile %q is not configured", rawURL, cmp.Or(profile, "default"))
	}
	bucket := u.Host
	key := strings.TrimPrefix(u.Path, "/")
	if bucket == "" || key == "" {
		return "", fmt.Errorf("s3 URL must be s3://bucket/key, got %q", rawURL)
	}

	scheme, host, basePath, err := s3EndpointHost(creds, bucket)
	if err != nil {
		return "", err
	}

	// Path-style puts the bucket in the path; virtual-hosted puts it in the host.
	// A configured endpoint may carry a base path (e.g. http://gw:9000/s3) that
	// prefixes — and must be signed as part of — the request URI.
	var canonicalURI string
	if creds.Endpoint != "" || creds.ForcePathStyle {
		canonicalURI = basePath + "/" + uriEncodePath(bucket) + "/" + uriEncodePath(key)
	} else {
		canonicalURI = basePath + "/" + uriEncodePath(key)
	}

	return scheme + "://" + host + canonicalURI + "?" + presignQuery(creds, method, host, canonicalURI, ttl, s3Now().UTC()), nil
}

// presignQuery is the SigV4 query string, signature last, that authorizes
// method on host and canonicalURI from now for ttl.
func presignQuery(creds config.S3Credentials, method, host, canonicalURI string, ttl time.Duration, now time.Time) string {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + creds.Region + "/s3/aws4_request"

	// The canonical query is every X-Amz parameter but the signature, sorted by
	// name. A session token (STS/temporary credentials) is signed with them.
	query := []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=" + uriEncode(creds.AccessKeyID+"/"+scope),
		"X-Amz-Date=" + amzDate,
		"X-Amz-Expires=" + strconv.Itoa(int(ttl/time.Second)),
	}
	if creds.SessionToken != "" {
		query = append(query, "X-Amz-Security-Token="+uriEncode(creds.SessionToken))
	}
	query = append(query, "X-Amz-SignedHeaders=host")
	canonicalQuery := strings.Join(query, "&")

	canonicalRequest := method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" +
		"host:" + host + "\n\n" + "host\n" + "UNSIGNED-PAYLOAD"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))
	signature := hex.EncodeToString(hmacSHA256(s3SigningKey(creds.SecretAccessKey, dateStamp, creds.Region), stringToSign))

	return canonicalQuery + "&X-Amz-Signature=" + signature
}

// s3EndpointHost resolves the scheme, host, and base path to connect to. With a
// configured endpoint (MinIO/custom), that endpoint is used, including any path
// prefix it carries (e.g. http://gw:9000/s3 → basePath "/s3"); otherwise the AWS
// regional host is derived, virtual-hosted unless path style is forced. basePath
// is "" for AWS and endpoints with no path, and never has a trailing slash.
func s3EndpointHost(creds config.S3Credentials, bucket string) (scheme, host, basePath string, err error) {
	if creds.Endpoint != "" {
		ep := creds.Endpoint
		if !strings.Contains(ep, "://") {
			ep = "https://" + ep
		}
		u, err := url.Parse(ep)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid S3 endpoint %q: %w", creds.Endpoint, err)
		}
		// Encode the operator-supplied prefix (slashes preserved) so it signs and
		// sends verbatim; strip the trailing slash so we don't double up on "/".
		return u.Scheme, u.Host, uriEncodePath(strings.TrimSuffix(u.Path, "/")), nil
	}
	if creds.ForcePathStyle {
		return "https", "s3." + creds.Region + ".amazonaws.com", "", nil
	}
	return "https", bucket + ".s3." + creds.Region + ".amazonaws.com", "", nil
}

func s3SigningKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uriEncodePath encodes a path per RFC 3986 as SigV4 requires, preserving the
// unreserved set and slashes between segments.
func uriEncodePath(p string) string {
	return strings.ReplaceAll(uriEncode(p), "%2F", "/")
}

// uriEncode encodes everything outside the RFC 3986 unreserved set, slashes
// included, as SigV4 requires of query values.
func uriEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
