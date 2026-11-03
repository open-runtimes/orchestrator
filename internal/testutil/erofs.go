package testutil

import (
	"io/fs"
	"orchestrator/internal/erofs"
	"os"
	"testing"
	"testing/fstest"
)

// WriteErofs writes an EROFS image at path holding hello.txt = "hello from the
// image" and dir/data, every entry owned by root, as a build's output is.
func WriteErofs(t *testing.T, path string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	w := erofs.Create(out)
	if err := w.CopyFrom(fstest.MapFS{
		"hello.txt": {Data: []byte("hello from the image"), Mode: 0o644},
		"dir":       {Mode: fs.ModeDir | 0o755},
		"dir/data":  {Data: []byte("data"), Mode: 0o644},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
