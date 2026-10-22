package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
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

// redactTransferError strips the query string from a transfer error before it
// is wrapped into a failure message. A download/upload URL is presigned — the
// query carries the credential — and net/http puts the whole URL in a
// *url.Error, which would otherwise travel into the callback, logs and job
// status. The wrapped cause is preserved, so classification (isClientError and
// friends) still works.
func redactTransferError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := *ue
	if u, parseErr := url.Parse(ue.URL); parseErr == nil {
		u.RawQuery = ""
		u.Fragment = ""
		redacted.URL = u.Redacted()
	} else {
		redacted.URL = "[redacted]"
	}
	return &redacted
}
