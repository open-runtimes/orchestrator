//go:build !linux

package mount

import "errors"

// Beneath stands in on non-Linux platforms (local development on macOS) so the
// packages that use it build; mounting only happens on Linux.
type Beneath struct {
	Dir string
}

var errUnsupported = errors.New("artifact mounting is only supported on linux")

func (Beneath) Mount(source, target string, opts Opts) error { return errUnsupported }

func (Beneath) Unmount(target string) error { return errUnsupported }

func (Beneath) IsMounted(target string) (bool, error) { return false, errUnsupported }
