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
