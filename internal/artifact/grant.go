package artifact

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"orchestrator/internal/config"
	"strings"
	"time"
)

const (
	// EnvS3Grants carries a pod's Grants as JSON, and EnvPresignEndpoint the
	// base URL of the service that redeems them.
	EnvS3Grants        = "S3_GRANTS"
	EnvPresignEndpoint = "PRESIGN_ENDPOINT"

	// PresignPath is where a service redeems a grant for a pre-signed URL.
	PresignPath = "/internal/presign"

	// presignTTL bounds a pre-signed URL. A pod asks for one right before each
	// transfer, so it only has to outlive the start of that transfer.
	presignTTL = 15 * time.Minute
)

// Grants maps a transfer ("GET s3://bucket/key") to the grant that has it
// pre-signed. It is all a pod holds for S3: a grant names one object and one
// method, so a leaked one reaches nothing else, and it carries no key. Grants
// are stateless: rotating the profile's secret key revokes every one of them.
type Grants map[string]string

// GrantsFor returns a grant for every s3:// transfer the artifacts can make:
// a download reads, an upload writes, and a synced mount does both. It is nil
// when there are none.
func GrantsFor(artifacts []Artifact, profiles config.S3Profiles) Grants {
	var g Grants
	add := func(method, rawURL string) {
		if !isS3URL(rawURL) {
			return
		}
		if mac, ok := grant(profiles, method, rawURL); ok {
			if g == nil {
				g = Grants{}
			}
			g[transfer(method, rawURL)] = mac
		}
	}
	for _, a := range artifacts {
		switch a := a.(type) {
		case *Download:
			add(http.MethodGet, a.In)
		case *Upload:
			add(http.MethodPut, a.Out)
		case *Mount:
			add(http.MethodGet, a.Sync)
			add(http.MethodPut, a.Sync)
		}
	}
	return g
}

// JSON is the grants as the EnvS3Grants value; "" when there are none.
func (g Grants) JSON() string {
	if len(g) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(g)
	return string(encoded)
}

// ParseGrants reads an EnvS3Grants value. A malformed one is logged and
// dropped: transfers then fail with "no grant", naming the URL.
func ParseGrants(raw string) Grants {
	if raw == "" {
		return nil
	}
	var g Grants
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		slog.Error("Ignoring malformed "+EnvS3Grants, "error", err)
		return nil
	}
	return g
}

func transfer(method, rawURL string) string { return method + " " + rawURL }

// grant is HMAC-SHA256 of the transfer under the secret key of the profile the
// URL names, so whoever holds a plane's S3 keys can mint and redeem grants
// with no other key to distribute. False when that profile is not configured.
func grant(profiles config.S3Profiles, method, rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	creds := profiles[u.User.Username()]
	if !creds.Enabled() {
		return "", false
	}
	return hex.EncodeToString(hmacSHA256([]byte(creds.SecretAccessKey), "orchestrator-s3-grant\n"+transfer(method, rawURL))), true
}

// Signer turns an s3:// URL into one a plain request can be sent to.
type Signer interface {
	Sign(ctx context.Context, method, rawURL string) (string, error)
}

// GrantSigner redeems a pod's grants at the service that minted them.
type GrantSigner struct {
	Endpoint string // the service's base URL (PRESIGN_ENDPOINT)
	Grants   Grants
	Client   *http.Client // nil = a 30s-timeout client
}

type presignRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Grant  string `json:"grant"`
}

type presignResponse struct {
	URL string `json:"url"`
}

// Sign implements Signer.
func (s *GrantSigner) Sign(ctx context.Context, method, rawURL string) (string, error) {
	mac, ok := s.Grants[transfer(method, rawURL)]
	if !ok {
		return "", fmt.Errorf("no S3 grant for %s %s", method, rawURL)
	}
	if s.Endpoint == "" {
		return "", fmt.Errorf("cannot sign %s: no PRESIGN_ENDPOINT to redeem the grant at", rawURL)
	}
	body, _ := json.Marshal(presignRequest{Method: method, URL: rawURL, Grant: mac})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.Endpoint, "/")+PresignPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("presign %s: status %d", rawURL, resp.StatusCode)
	}
	var out presignResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.URL == "" {
		return "", fmt.Errorf("presign %s: malformed response", rawURL)
	}
	return out.URL, nil
}

// PresignHandler redeems grants: POST {method, url, grant} answers {url}, a
// pre-signed URL for exactly that transfer. The grant is the only credential,
// so the route sits outside the API key, like the artifact reports.
func PresignHandler(profiles config.S3Profiles) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req presignRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(w, "malformed presign request", http.StatusBadRequest)
			return
		}
		if (req.Method != http.MethodGet && req.Method != http.MethodPut) || !isS3URL(req.URL) {
			http.Error(w, "presign takes GET or PUT of an s3:// URL", http.StatusBadRequest)
			return
		}
		want, ok := grant(profiles, req.Method, req.URL)
		if !ok || !hmac.Equal([]byte(req.Grant), []byte(want)) {
			http.Error(w, "invalid grant", http.StatusForbidden)
			return
		}
		signed, err := Presign(profiles, req.Method, req.URL, presignTTL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(presignResponse{URL: signed})
	})
}
