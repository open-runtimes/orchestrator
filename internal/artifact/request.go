package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// buildRequest returns a ready-to-send request for a transfer, hiding the
// scheme from callers: an s3:// URL is pre-signed by signer first, any other
// URL is requested as given. For uploads body is the source file and size its
// length; downloads pass a nil body. Callers layer their own headers, timeout,
// and retry policy on top.
func buildRequest(ctx context.Context, method, rawURL string, body io.Reader, size int64, signer Signer) (*http.Request, error) {
	if isS3URL(rawURL) {
		if signer == nil {
			return nil, errors.New("s3 URL " + rawURL + " needs a signer: no S3 grants were given")
		}
		signed, err := signer.Sign(ctx, method, rawURL)
		if err != nil {
			return nil, err
		}
		rawURL = signed
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
