package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadS3Profiles(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://minio:9000")
	t.Setenv("S3_REGION", "eu-west-1")
	t.Setenv("S3_ACCESS_KEY_ID", "AKID")
	t.Setenv("S3_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")

	c := LoadS3Profiles()[""]
	if c.Endpoint != "http://minio:9000" || c.Region != "eu-west-1" || c.AccessKeyID != "AKID" || c.SecretAccessKey != "SECRET" || !c.ForcePathStyle {
		t.Fatalf("unexpected credentials: %+v", c)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestLoadS3Profiles_Defaults(t *testing.T) {
	// No S3 env set: region defaults, credentials disabled.
	p := LoadS3Profiles()
	if c := p[""]; c.Region != defaultS3Region || c.Enabled() {
		t.Errorf("default profile = %+v, want disabled with region %q", c, defaultS3Region)
	}
	if env := p.ToEnv(); env != nil {
		t.Errorf("ToEnv() = %v, want nil when disabled", env)
	}
}

func TestLoadS3Profiles_Named(t *testing.T) {
	t.Setenv("S3_PROFILES", "archive, backup")
	t.Setenv("S3_ARCHIVE_ENDPOINT", "http://minio:9000")
	t.Setenv("S3_ARCHIVE_ACCESS_KEY_ID", "AKIDARCHIVE")
	t.Setenv("S3_ARCHIVE_SECRET_ACCESS_KEY", "SECRETARCHIVE")

	p := LoadS3Profiles()
	if c := p["archive"]; c.Endpoint != "http://minio:9000" || c.AccessKeyID != "AKIDARCHIVE" || c.SecretAccessKey != "SECRETARCHIVE" || c.Region != defaultS3Region {
		t.Fatalf("archive profile = %+v", c)
	}
	if p[""].Enabled() || p["backup"].Enabled() {
		t.Error("profiles without keys should be disabled")
	}

	// Forwarded to the sidecar: only configured profiles, and a sidecar that
	// loads the forwarded env sees the same archive profile.
	for _, kv := range p.ToEnv() {
		t.Setenv(kv[0], kv[1])
	}
	if got := os.Getenv("S3_PROFILES"); got != "archive" {
		t.Errorf("forwarded S3_PROFILES = %q, want archive", got)
	}
	if got := LoadS3Profiles()["archive"]; got != p["archive"] {
		t.Errorf("round-trip = %+v, want %+v", got, p["archive"])
	}
}

func TestLoadS3Profiles_SecretFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("  filesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("S3_ACCESS_KEY_ID", "AKID")
	t.Setenv("S3_SECRET_ACCESS_KEY_FILE", path)

	if c := LoadS3Profiles()[""]; c.SecretAccessKey != "filesecret" {
		t.Errorf("SecretAccessKey = %q, want %q (trimmed from file)", c.SecretAccessKey, "filesecret")
	}
}

func TestLoadS3Profiles_SessionToken(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY_ID", "AKID")
	t.Setenv("S3_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("S3_SESSION_TOKEN", "SESSION")

	p := LoadS3Profiles()
	if p[""].SessionToken != "SESSION" {
		t.Errorf("SessionToken = %q, want SESSION", p[""].SessionToken)
	}
	// Forwarded to the sidecar so STS creds keep working there.
	var forwarded string
	for _, kv := range p.ToEnv() {
		if kv[0] == "S3_SESSION_TOKEN" {
			forwarded = kv[1]
		}
	}
	if forwarded != "SESSION" {
		t.Errorf("ToEnv did not forward the session token, got %q", forwarded)
	}
}

func TestS3Profiles_ToEnv(t *testing.T) {
	p := S3Profiles{"": {AccessKeyID: "AKID", SecretAccessKey: "SECRET", Region: "us-east-1"}}
	env := p.ToEnv()
	// Endpoint, ForcePathStyle and S3_PROFILES omitted when unset.
	want := map[string]string{"S3_ACCESS_KEY_ID": "AKID", "S3_SECRET_ACCESS_KEY": "SECRET", "S3_REGION": "us-east-1"}
	if len(env) != len(want) {
		t.Fatalf("ToEnv() = %v, want %d entries", env, len(want))
	}
	for _, kv := range env {
		if want[kv[0]] != kv[1] {
			t.Errorf("ToEnv() entry %q = %q, want %q", kv[0], kv[1], want[kv[0]])
		}
	}
}
