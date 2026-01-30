// Package artifact provides types and utilities for job artifacts.
// The actual type definitions are in the types subpackage.
package artifact

import (
	"orchestrator/internal/artifact/types"
)

// Re-export types for convenience
type (
	Artifact  = types.Artifact
	Result    = types.Result
	Download  = types.Download
	Upload    = types.Upload
	Write     = types.Write
	Read      = types.Read
	Archive   = types.Archive
	Unarchive = types.Unarchive
	List      = types.List
)

// Re-export constants and constructors
const JobDependency = types.JobDependency

var (
	NewDownload = types.NewDownload
	NewUpload   = types.NewUpload
)
