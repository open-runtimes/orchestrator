package artifact

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchestrator/internal/config"
	"strings"
	"time"
)

// s3Scheme is the URL scheme that routes a download/upload through the SigV4
// signer instead of a plain HTTP request.
const s3Scheme = "s3"

// emptyPayloadHash is SHA256(""), used for request bodies with no content (GET).
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// isS3URL reports whether rawURL uses the s3:// scheme.
func isS3URL(rawURL string) bool {
	return strings.HasPrefix(strings.ToLower(rawURL), s3Scheme+"://")
}

// s3Now is overridable in tests; production uses the wall clock.
var s3Now = time.Now

// newSignedS3Request builds an http.Request for an s3://bucket/key URL, signed
// with AWS Signature Version 4. For uploads, body is the source (an *os.File)
// and size its length; the body is hashed for the signature and rewound before
// it is sent. Downloads pass a nil body.
func newSignedS3Request(ctx context.Context, method, rawURL string, body io.ReadSeeker, size int64, creds config.S3Credentials) (*http.Request, error) {
	if !creds.Enabled() {
		return nil, fmt.Errorf("s3 URL %q requires S3 credentials, but none are configured", rawURL)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 URL: %w", err)
	}
	bucket := u.Host
	key := strings.TrimPrefix(u.Path, "/")
	if bucket == "" || key == "" {
		return nil, fmt.Errorf("s3 URL must be s3://bucket/key, got %q", rawURL)
	}

	scheme, host, basePath, err := s3EndpointHost(creds, bucket)
	if err != nil {
		return nil, err
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
	targetURL := scheme + "://" + host + canonicalURI

	payloadHash, err := s3PayloadHash(body)
	if err != nil {
		return nil, err
	}

	reqBody := io.Reader(http.NoBody)
	if body != nil {
		reqBody = body
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, reqBody)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.ContentLength = size
	}

	now := s3Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Host = host
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Canonical request: signed headers are host + the x-amz headers, in sorted
	// order. A session token (STS/temporary credentials) adds x-amz-security-token,
	// which sorts after x-amz-date and must be signed too or AWS returns 403.
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
		signedHeaders += ";x-amz-security-token"
		canonicalHeaders += "x-amz-security-token:" + creds.SessionToken + "\n"
	}
	canonicalRequest := method + "\n" + canonicalURI + "\n" + "" + "\n" +
		canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash

	scope := dateStamp + "/" + creds.Region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	signingKey := s3SigningKey(creds.SecretAccessKey, dateStamp, creds.Region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, scope, signedHeaders, signature))

	return req, nil
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

// s3PayloadHash returns the hex SHA256 that SigV4 signs for the body, rewinding
// the reader afterwards so it can be sent. A nil body (GET) hashes as empty.
func s3PayloadHash(body io.ReadSeeker) (string, error) {
	if body == nil {
		return emptyPayloadHash, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return "", fmt.Errorf("failed to hash upload body: %w", err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("failed to rewind upload body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uriEncodePath encodes a path per RFC 3986 as SigV4 requires, preserving the
// unreserved set and slashes between segments.
func uriEncodePath(p string) string {
	var b strings.Builder
	for _, c := range []byte(p) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
