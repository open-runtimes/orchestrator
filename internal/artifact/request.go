package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
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
