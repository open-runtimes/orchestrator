package artifact

import (
	"context"
	"io"
	"net/http"
	"orchestrator/internal/config"
)

// buildRequest returns a ready-to-send request for a transfer, hiding the
// scheme from callers: an s3:// URL yields a SigV4-signed request (the body is
// hashed for the signature), any other scheme a plain HTTP request. For uploads
// body is the source file and size its length; downloads pass a nil body.
// Callers layer their own headers, timeout, and retry policy on top.
func buildRequest(ctx context.Context, method, rawURL string, body io.ReadSeeker, size int64, creds config.S3Credentials) (*http.Request, error) {
	if isS3URL(rawURL) {
		return newSignedS3Request(ctx, method, rawURL, body, size, creds)
	}

	reqBody := io.Reader(http.NoBody)
	if body != nil {
		reqBody = body
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.ContentLength = size
	}
	return req, nil
}
