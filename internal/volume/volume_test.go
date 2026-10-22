package volume

import "testing"

func TestVolume_Validate(t *testing.T) {
	tests := []struct {
		name    string
		vol     Volume
		wantErr bool
	}{
		{"valid", Volume{Source: "my-pvc", Path: "/data"}, false},
		{"valid with subpath", Volume{Source: "my-pvc", Path: "/data", SubPath: "sub/dir", ReadOnly: true}, false},
		{"missing source", Volume{Path: "/data"}, true},
		{"missing path", Volume{Source: "my-pvc"}, true},
		{"relative path", Volume{Source: "my-pvc", Path: "data"}, true},
		{"absolute subpath", Volume{Source: "my-pvc", Path: "/data", SubPath: "/etc"}, true},
		{"subpath traversal", Volume{Source: "my-pvc", Path: "/data", SubPath: "../escape"}, true},
		{"subpath traversal mid", Volume{Source: "my-pvc", Path: "/data", SubPath: "a/../../b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.vol.Validate("volumes[0]")
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVolume_Validate_AllowedVolumes(t *testing.T) {
	vol := func(source string) Volume { return Volume{Source: source, Path: "/data"} }

	t.Setenv(EnvAllowedVolumes, "")
	if err := vol("anything").Validate("volumes[0]"); err != nil {
		t.Errorf("no allowlist: Validate() = %v, want any source accepted", err)
	}

	t.Setenv(EnvAllowedVolumes, " appwrite-builds , cache ")
	for _, source := range []string{"appwrite-builds", "cache"} {
		if err := vol(source).Validate("volumes[0]"); err != nil {
			t.Errorf("Validate(%q) = %v, want allowed", source, err)
		}
	}
	for _, source := range []string{"appwrite-mariadb", "job-other-workspace", "appwrite-builds2", "Cache"} {
		if err := vol(source).Validate("volumes[0]"); err == nil {
			t.Errorf("Validate(%q) = nil, want it refused", source)
		}
	}
}
