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

// A sidecar that loads what ToEnv forwards sees exactly the configured
// profiles, session token and path style included, and none without keys.
func TestS3Profiles_RoundTrip(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://minio:9000")
	t.Setenv("S3_ACCESS_KEY_ID", "AKID")
	t.Setenv("S3_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("S3_SESSION_TOKEN", "SESSION")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")
	t.Setenv("S3_PROFILES", "archive, backup")
	t.Setenv("S3_ARCHIVE_ACCESS_KEY_ID", "AKIDARCHIVE")
	t.Setenv("S3_ARCHIVE_SECRET_ACCESS_KEY", "SECRETARCHIVE")
	t.Setenv("S3_ARCHIVE_REGION", "eu-west-1")

	p := LoadS3Profiles()
	if p["backup"].Enabled() {
		t.Fatal("a listed profile without keys must stay disabled")
	}
	forwarded := p.ToEnv()
	for _, key := range []string{"S3_ENDPOINT", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_SESSION_TOKEN", "S3_FORCE_PATH_STYLE", "S3_PROFILES", "S3_ARCHIVE_ACCESS_KEY_ID", "S3_ARCHIVE_SECRET_ACCESS_KEY", "S3_ARCHIVE_REGION"} {
		os.Unsetenv(key)
	}
	for _, kv := range forwarded {
		t.Setenv(kv[0], kv[1])
	}
	got := LoadS3Profiles()
	if os.Getenv("S3_PROFILES") != "archive" {
		t.Errorf("forwarded S3_PROFILES = %q, want only the configured archive", os.Getenv("S3_PROFILES"))
	}
	for _, name := range []string{"", "archive"} {
		if got[name] != p[name] {
			t.Errorf("profile %q round-trip = %+v, want %+v", name, got[name], p[name])
		}
	}
}

// Names the chart would refuse are refused here too, so a URL, an env var
// prefix and a values key can never name a profile differently.
func TestLoadS3Profiles_InvalidNames(t *testing.T) {
	t.Setenv("S3_PROFILES", "Archive,my-store,ok")
	for _, prefix := range []string{"S3_ARCHIVE_", "S3_MY-STORE_", "S3_OK_"} {
		t.Setenv(prefix+"ACCESS_KEY_ID", "AKID")
		t.Setenv(prefix+"SECRET_ACCESS_KEY", "SECRET")
	}
	p := LoadS3Profiles()
	if _, ok := p["Archive"]; ok {
		t.Error("an upper-case name was loaded")
	}
	if _, ok := p["my-store"]; ok {
		t.Error("a name that is not an env var prefix was loaded")
	}
	if !p["ok"].Enabled() {
		t.Error("a valid profile was dropped")
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
