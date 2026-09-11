package main

import (
	"cmp"
	"fmt"
	"orchestrator/internal/config"
)

const releaseImageRepository = "ghcr.io/open-runtimes/orchestrator"

// releaseVersion is set by the release build. latest keeps ordinary local
// builds useful when no version is injected.
//
//nolint:gochecknoglobals // Go linker injection requires a package variable.
var releaseVersion = "latest"

func configuredSidecarImage(env, name string) string {
	return config.GetEnv(env, releaseSidecarImage(name))
}

func releaseSidecarImage(name string) string {
	// A release build may inject an empty version; local builds keep latest.
	version := cmp.Or(releaseVersion, "latest")
	return fmt.Sprintf("%s/%s:%s", releaseImageRepository, name, version)
}
