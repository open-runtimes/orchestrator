package artifact

import (
	"fmt"
	"orchestrator/internal/apperrors"
)

// The artifact types are a closed set: the wire "type" discriminator, the
// concrete Go type, its validation rules, and the workspace file it reads all
// live in the three functions below. Adding a type means adding a case to
// each.

// newArtifact returns the zero value for a wire type, or nil for an unknown one.
func newArtifact(typ string) Artifact {
	switch typ {
	case "download":
		return &Download{}
	case "clone":
		return &Clone{}
	case "upload":
		return &Upload{}
	case "write":
		return &Write{}
	case "read":
		return &Read{}
	case "archive":
		return &Archive{}
	case "unarchive":
		return &Unarchive{}
	case "mount":
		return &Mount{}
	case "list":
		return &List{}
	case "stat":
		return &Stat{}
	}
	return nil
}

// SourceFile returns the workspace file an artifact reads before it can run,
// or "" for types that read nothing. The sidecar waits for it to appear.
func SourceFile(a Artifact) string {
	switch a := a.(type) {
	case *Upload:
		return a.In
	case *Read:
		return a.In
	case *Archive:
		return a.In
	case *Unarchive:
		return a.In
	case *Mount:
		return a.In
	case *Stat:
		return a.In
	case *List:
		return a.In
	}
	return ""
}

// Validate checks field-level constraints for the artifact at index i of a
// request's artifact list.
func Validate(i int, a Artifact) error {
	field := fmt.Sprintf("artifacts[%d]", i)
	if a.ArtifactID() == "" {
		return apperrors.Validation(field+".id", fmt.Sprintf("artifact[%d]: id is required", i))
	}
	switch a := a.(type) {
	case *Download:
		return firstErr(requiredURL(field, "in", "url", a.In), requiredPath(field, "out", "path", a.Out))
	case *Clone:
		if a.In == "" {
			return apperrors.Validation(field+".in", "in (repository url) is required")
		}
		if err := validateGitURL(a.In); err != nil {
			return apperrors.Validation(field+".in", "invalid in (repository url): "+err.Error())
		}
		if err := requiredPath(field, "out", "path", a.Out); err != nil {
			return err
		}
		if err := validatePath(a.Subdir); err != nil {
			return apperrors.Validation(field+".subdir", "invalid subdir: "+err.Error())
		}
		return nil
	case *Upload:
		return firstErr(requiredPath(field, "in", "path", a.In), requiredURL(field, "out", "url", a.Out))
	case *Write:
		if err := requiredPath(field, "out", "path", a.Out); err != nil {
			return err
		}
		if a.In == "" {
			return apperrors.Validation(field+".in", "in (content) is required")
		}
		return nil
	case *Read:
		if err := requiredPath(field, "in", "path", a.In); err != nil {
			return err
		}
		switch a.Format {
		case "", "text", "json":
			return nil
		}
		return apperrors.Validation(field+".format", "format must be \"text\" or \"json\"")
	case *Archive:
		if err := firstErr(requiredPath(field, "in", "path", a.In), requiredPath(field, "out", "dest", a.Out)); err != nil {
			return err
		}
		return validateArchiveFormat(field, a)
	case *Unarchive:
		if err := firstErr(requiredPath(field, "in", "path", a.In), requiredPath(field, "out", "dest", a.Out)); err != nil {
			return err
		}
		switch a.SymlinkPolicy {
		case "", SymlinkPolicyPreserve, SymlinkPolicyContained:
			return nil
		}
		return apperrors.Validation(field+".symlinkPolicy", "symlinkPolicy must be \"preserve\" or \"contained\"")
	case *Mount:
		if err := firstErr(requiredPath(field, "in", "image path", a.In), requiredPath(field, "out", "mount dir", a.Out)); err != nil {
			return err
		}
		return validateMountOptions(field, a)
	case *Stat:
		return requiredPath(field, "in", "path", a.In)
	case *List:
		return requiredPath(field, "in", "path", a.In)
	}
	return apperrors.Validation(field+".type", fmt.Sprintf("unknown artifact type %q", a.ArtifactType()))
}

func validateArchiveFormat(field string, a *Archive) error {
	switch a.Format {
	case "tar":
		switch a.Compression {
		case "", "none", "zstd", "lz4", "lz4hc":
			if a.Level != 0 {
				return apperrors.Validation(field+".level", "level is only valid with gzip compression")
			}
		case "gzip":
			if a.Level < 0 || a.Level > 9 {
				return apperrors.Validation(field+".level", "level must be between 0 and 9 (0 means default)")
			}
		default:
			return apperrors.Validation(field+".compression", "compression must be \"gzip\", \"zstd\", \"lz4\", \"lz4hc\", or \"none\" for tar")
		}
		if a.BlockSize != 0 {
			return apperrors.Validation(field+".blockSize", "blockSize is only valid for squashfs")
		}
	case "squashfs":
		if _, err := squashfsCompression(a.Compression); err != nil {
			return apperrors.Validation(field+".compression", err.Error())
		}
		if a.Level != 0 {
			return apperrors.Validation(field+".level", "level is not supported for squashfs")
		}
		if a.BlockSize != 0 && !validSquashfsBlockSize(a.BlockSize) {
			return apperrors.Validation(field+".blockSize", "blockSize must be a power of 2 between 4096 and 1048576 (0 means 1 MiB default)")
		}
	case "erofs":
		if _, _, err := erofsCompression(a.Compression); err != nil {
			return apperrors.Validation(field+".compression", err.Error())
		}
		if a.Level != 0 {
			return apperrors.Validation(field+".level", "level is not supported for erofs")
		}
		if a.BlockSize != 0 {
			return apperrors.Validation(field+".blockSize", "blockSize is not supported for erofs")
		}
	default:
		return apperrors.Validation(field+".format", "format must be \"tar\", \"squashfs\", or \"erofs\"")
	}
	return nil
}

func validateMountOptions(field string, a *Mount) error {
	if a.Size < 0 {
		return apperrors.Validation(field+".size", "size must not be negative")
	}
	if a.Size > 0 && !a.Writable {
		return apperrors.Validation(field+".size", "size only applies to writable mounts")
	}
	if a.Sync != "" {
		if !a.Writable {
			return apperrors.Validation(field+".sync",
				"sync only applies to writable mounts: without an overlay there is no delta to sync")
		}
		if err := validateURL(a.Sync); err != nil {
			return apperrors.Validation(field+".sync", "invalid sync destination: "+err.Error())
		}
	}
	switch {
	case a.SyncIntervalSeconds < 0:
		return apperrors.Validation(field+".syncIntervalSeconds", "sync interval must not be negative")
	case a.SyncIntervalSeconds > 0 && a.Sync == "":
		return apperrors.Validation(field+".syncIntervalSeconds", "sync interval needs a sync destination")
	case a.SyncIntervalSeconds > 0 && a.SyncIntervalSeconds < MinSyncIntervalSeconds:
		return apperrors.Validation(field+".syncIntervalSeconds",
			fmt.Sprintf("sync interval must be at least %d seconds", MinSyncIntervalSeconds))
	}
	return nil
}

// requiredPath and requiredURL check a required field of the given kind; what
// names the kind in the message, e.g. "out (path) is required".
func requiredPath(field, name, what, value string) error {
	if value == "" {
		return apperrors.Validation(field+"."+name, name+" ("+what+") is required")
	}
	if err := validatePath(value); err != nil {
		return apperrors.Validation(field+"."+name, "invalid "+name+" ("+what+"): "+err.Error())
	}
	return nil
}

func requiredURL(field, name, what, value string) error {
	if value == "" {
		return apperrors.Validation(field+"."+name, name+" ("+what+") is required")
	}
	if err := validateURL(value); err != nil {
		return apperrors.Validation(field+"."+name, "invalid "+name+" ("+what+"): "+err.Error())
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
