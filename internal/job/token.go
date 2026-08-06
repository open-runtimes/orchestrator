package job

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// ArtifactToken derives the per-job bearer token for the internal artifact
// endpoint: hex(HMAC-SHA256(key, jobID)). Deriving it from the API key keeps
// the scheme stateless — every service replica can issue and verify tokens
// without persisting anything, and tokens remain valid across restarts.
func ArtifactToken(key, jobID string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(jobID))
	return hex.EncodeToString(mac.Sum(nil))
}
