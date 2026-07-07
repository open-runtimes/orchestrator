package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadS3Credentials(t *testing.T) {
	t.Setenv(EnvS3Endpoint, "http://minio:9000")
	t.Setenv(EnvS3Region, "eu-west-1")
	t.Setenv(EnvS3AccessKeyID, "AKID")
	t.Setenv(EnvS3SecretAccessKey, "SECRET")
	t.Setenv(EnvS3ForcePathStyle, "true")

	c := LoadS3Credentials()
	if c.Endpoint != "http://minio:9000" || c.Region != "eu-west-1" || c.AccessKeyID != "AKID" || c.SecretAccessKey != "SECRET" || !c.ForcePathStyle {
		t.Fatalf("unexpected credentials: %+v", c)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestLoadS3Credentials_Defaults(t *testing.T) {
	// No S3 env set: region defaults, credentials disabled.
	c := LoadS3Credentials()
	if c.Region != defaultS3Region {
		t.Errorf("Region = %q, want %q", c.Region, defaultS3Region)
	}
	if c.Enabled() {
		t.Error("Enabled() = true with no keys, want false")
	}
	if c.ToEnv() != nil {
		t.Errorf("ToEnv() = %v, want nil when disabled", c.ToEnv())
	}
}

func TestLoadS3Credentials_SecretFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("  filesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvS3AccessKeyID, "AKID")
	t.Setenv(EnvS3SecretFile, path)

	c := LoadS3Credentials()
	if c.SecretAccessKey != "filesecret" {
		t.Errorf("SecretAccessKey = %q, want %q (trimmed from file)", c.SecretAccessKey, "filesecret")
	}
}

func TestLoadS3Credentials_SessionToken(t *testing.T) {
	t.Setenv(EnvS3AccessKeyID, "AKID")
	t.Setenv(EnvS3SecretAccessKey, "SECRET")
	t.Setenv(EnvS3SessionToken, "SESSION")

	c := LoadS3Credentials()
	if c.SessionToken != "SESSION" {
		t.Errorf("SessionToken = %q, want SESSION", c.SessionToken)
	}
	// Forwarded to the sidecar so STS creds keep working there.
	var forwarded string
	for _, kv := range c.ToEnv() {
		if kv[0] == EnvS3SessionToken {
			forwarded = kv[1]
		}
	}
	if forwarded != "SESSION" {
		t.Errorf("ToEnv did not forward the session token, got %q", forwarded)
	}
}

func TestS3Credentials_ToEnv(t *testing.T) {
	c := S3Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", Region: "us-east-1"}
	env := c.ToEnv()
	// Endpoint and ForcePathStyle omitted when unset.
	want := map[string]string{EnvS3AccessKeyID: "AKID", EnvS3SecretAccessKey: "SECRET", EnvS3Region: "us-east-1"}
	if len(env) != len(want) {
		t.Fatalf("ToEnv() = %v, want %d entries", env, len(want))
	}
	for _, kv := range env {
		if want[kv[0]] != kv[1] {
			t.Errorf("ToEnv() entry %q = %q, want %q", kv[0], kv[1], want[kv[0]])
		}
	}
}
