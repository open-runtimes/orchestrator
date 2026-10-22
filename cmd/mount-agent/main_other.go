//go:build !linux

// mount-agent performs kernel mounts on a Kubernetes node and only builds a
// working binary for Linux; elsewhere it exists so `go build ./...` succeeds.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mount-agent only runs on linux")
	os.Exit(1)
}
