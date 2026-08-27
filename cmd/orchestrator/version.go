package main

import (
	"fmt"
	"orchestrator/internal/config"
)

const releaseImageRepository = "ghcr.io/open-runtimes/orchestrator"

// releaseVersion is set by the release build. latest keeps ordinary local
// builds useful when no version is injected.
var releaseVersion = "latest"

func configuredSidecarImage(env, name string) string {
	return config.GetEnv(env, releaseSidecarImage(name))
}

func releaseSidecarImage(name string) string {
	version := releaseVersion
	if version == "" {
		version = "latest"
	}
	return fmt.Sprintf("%s/%s:%s", releaseImageRepository, name, version)
}
