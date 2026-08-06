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
