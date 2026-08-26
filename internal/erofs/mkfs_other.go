//go:build !linux && !darwin

package erofs

import (
	"io/fs"

	"orchestrator/internal/erofs/builder"
)

func entryFromSys(info fs.FileInfo) *builder.Entry {
	if be, ok := info.Sys().(*builder.Entry); ok {
		return be
	}
	return nil
}
