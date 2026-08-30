//go:build !linux

package sidecar

import "errors"

// unsupportedMounter stands in on non-Linux platforms (e.g. local dev on macOS)
// so the package builds; artifact mounting only runs in the Linux sidecar.
type unsupportedMounter struct{}

//nolint:iface // platform builds return different concrete Mounter types
func defaultMounter() Mounter { return unsupportedMounter{} }

func (unsupportedMounter) Mount(image, target string, opts MountOpts) error {
	return errors.New("artifact mounting is only supported on linux")
}

func (unsupportedMounter) Unmount(target string) error {
	return errors.New("artifact mounting is only supported on linux")
}
