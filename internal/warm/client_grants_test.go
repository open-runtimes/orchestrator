package warm

import (
	"context"
	"orchestrator/internal/artifact"
	"orchestrator/internal/claim"
	"orchestrator/internal/config"
	"orchestrator/internal/workload"
	"testing"
)

type capturePoster struct{ got *workload.ClaimRequest }

func (p *capturePoster) Post(_ context.Context, _ claim.Unit, req *workload.ClaimRequest) error {
	p.got = req
	return nil
}

// Grants are minted as the claim leaves, from the claimant's keys, so the
// claim stored on a Revision or handed in by a caller never holds them.
func TestHTTPClient_ClaimCarriesGrants(t *testing.T) {
	poster := &capturePoster{}
	c := &httpClient{poster: poster, s3: config.S3Profiles{"": {AccessKeyID: "AKID", SecretAccessKey: "secret"}}}
	req := &workload.ClaimRequest{ClaimID: "c", Artifacts: artifact.Set{
		&artifact.Download{ID: "img", In: "s3://bucket/base.erofs", Out: "base.erofs"},
	}}
	if err := c.Claim(t.Context(), "10.0.0.1", "tok", req); err != nil {
		t.Fatal(err)
	}
	if poster.got.S3Grants["GET s3://bucket/base.erofs"] == "" || len(poster.got.S3Grants) != 1 {
		t.Errorf("sent grants = %v", poster.got.S3Grants)
	}
	if req.S3Grants != nil {
		t.Error("the caller's claim was modified")
	}
}
