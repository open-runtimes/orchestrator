package artifact

import "testing"

func TestValidate_Download(t *testing.T) {
	tests := []struct {
		name    string
		art     *Download
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Download{ID: "dl1", In: "https://example.com/file", Out: "input.txt"},
			wantErr: false,
		},
		{
			name:    "missing id",
			art:     &Download{In: "https://example.com/file", Out: "input.txt"},
			wantErr: true,
		},
		{
			name:    "missing in (url)",
			art:     &Download{ID: "dl1", Out: "input.txt"},
			wantErr: true,
		},
		{
			name:    "missing out (path)",
			art:     &Download{ID: "dl1", In: "https://example.com/file"},
			wantErr: true,
		},
		{
			name:    "invalid url scheme",
			art:     &Download{ID: "dl1", In: "ftp://example.com/file", Out: "input.txt"},
			wantErr: true,
		},
		{
			name:    "path traversal",
			art:     &Download{ID: "dl1", In: "https://example.com/file", Out: "../etc/passwd"},
			wantErr: true,
		},
		{
			name:    "absolute path",
			art:     &Download{ID: "dl1", In: "https://example.com/file", Out: "/etc/passwd"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_Upload(t *testing.T) {
	tests := []struct {
		name    string
		art     *Upload
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Upload{ID: "ul1", In: "output.txt", Out: "https://example.com/upload"},
			wantErr: false,
		},
		{
			name:    "missing in (path)",
			art:     &Upload{ID: "ul1", Out: "https://example.com/upload"},
			wantErr: true,
		},
		{
			name:    "missing out (url)",
			art:     &Upload{ID: "ul1", In: "output.txt"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_Write(t *testing.T) {
	tests := []struct {
		name    string
		art     *Write
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Write{ID: "w1", In: "{}", Out: "config.json"},
			wantErr: false,
		},
		{
			name:    "missing in (content)",
			art:     &Write{ID: "w1", Out: "config.json"},
			wantErr: true,
		},
		{
			name:    "missing out (path)",
			art:     &Write{ID: "w1", In: "{}"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_Read(t *testing.T) {
	tests := []struct {
		name    string
		art     *Read
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Read{ID: "r1", In: "result.json"},
			wantErr: false,
		},
		{
			name:    "missing in (path)",
			art:     &Read{ID: "r1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_Archive(t *testing.T) {
	tests := []struct {
		name    string
		art     *Archive
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Archive{ID: "a1", In: "src", Out: "src.tar.gz", Format: "tar.gz"},
			wantErr: false,
		},
		{
			name:    "missing out (dest)",
			art:     &Archive{ID: "a1", In: "src", Format: "tar.gz"},
			wantErr: true,
		},
		{
			name:    "missing in (path)",
			art:     &Archive{ID: "a1", Out: "src.tar.gz", Format: "tar.gz"},
			wantErr: true,
		},
		{
			name:    "invalid format",
			art:     &Archive{ID: "a1", In: "src", Out: "src.zip", Format: "zip"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_Unarchive(t *testing.T) {
	tests := []struct {
		name    string
		art     *Unarchive
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &Unarchive{ID: "ua1", In: "src.tar.gz", Out: "src"},
			wantErr: false,
		},
		{
			name:    "valid with subdir",
			art:     &Unarchive{ID: "ua1", In: "repo.tar.gz", Out: "code", Subdir: "functions/node"},
			wantErr: false,
		},
		{
			name:    "missing out (dest)",
			art:     &Unarchive{ID: "ua1", In: "src.tar.gz"},
			wantErr: true,
		},
		{
			name:    "missing in (path)",
			art:     &Unarchive{ID: "ua1", Out: "src"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_List(t *testing.T) {
	tests := []struct {
		name    string
		art     *List
		wantErr bool
	}{
		{
			name:    "valid",
			art:     &List{ID: "l1", In: "src"},
			wantErr: false,
		},
		{
			name:    "missing in (path)",
			art:     &List{ID: "l1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(0, tt.art)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		path    string
		wantErr bool
	}{
		{"file.txt", false},
		{"subdir/file.txt", false},
		{"a/b/c/file.txt", false},
		{"", false},
		{"/absolute/path", true},
		{"../parent", true},
		{"foo/../bar", true},
		{"foo/../../bar", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := validatePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://example.com/file", false},
		{"http://example.com/file", false},
		{"https://example.com:8080/path", false},
		{"", false},
		{"ftp://example.com/file", true},
		{"file:///etc/passwd", true},
		{"not-a-url", true},
		{"://missing-scheme", true},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := validateURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}
