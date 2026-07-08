package artifact

import "orchestrator/internal/apperrors"

// Built-in TypeDef variables. Pass these to NewRegistry to build a Registry
// containing only the types you need — useful in tests.
var (
	DownloadDef = TypeDef{
		Type: "download",
		New:  func() Artifact { return &Download{} },
		Validate: TypedValidator(func(field string, a *Download) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (url) is required")
			}
			if err := validateURL(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (url): "+err.Error())
			}
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (path) is required")
			}
			if err := validatePath(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (path): "+err.Error())
			}
			return nil
		}),
	}

	UploadDef = TypeDef{
		Type: "upload",
		New:  func() Artifact { return &Upload{} },
		Validate: TypedValidator(func(field string, a *Upload) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (url) is required")
			}
			if err := validateURL(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (url): "+err.Error())
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Upload) string { return a.In }),
	}

	WriteDef = TypeDef{
		Type: "write",
		New:  func() Artifact { return &Write{} },
		Validate: TypedValidator(func(field string, a *Write) error {
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (path) is required")
			}
			if err := validatePath(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (path): "+err.Error())
			}
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (content) is required")
			}
			return nil
		}),
	}

	ReadDef = TypeDef{
		Type: "read",
		New:  func() Artifact { return &Read{} },
		Validate: TypedValidator(func(field string, a *Read) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Read) string { return a.In }),
	}

	ArchiveDef = TypeDef{
		Type: "archive",
		New:  func() Artifact { return &Archive{} },
		Validate: TypedValidator(func(field string, a *Archive) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (dest) is required")
			}
			if err := validatePath(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (dest): "+err.Error())
			}
			switch a.Format {
			case "tar":
				switch a.Compression {
				case "", "none", "zstd":
					if a.Level != 0 {
						return apperrors.Validation(field+".level", "level is only valid with gzip compression")
					}
				case "gzip":
					if a.Level < 0 || a.Level > 9 {
						return apperrors.Validation(field+".level", "level must be between 0 and 9 (0 means default)")
					}
				default:
					return apperrors.Validation(field+".compression", "compression must be \"gzip\", \"zstd\", or \"none\" for tar")
				}
			case "squashfs":
				if _, err := squashfsCompression(a.Compression); err != nil {
					return apperrors.Validation(field+".compression", err.Error())
				}
				if a.Level != 0 {
					return apperrors.Validation(field+".level", "level is not supported for squashfs")
				}
			default:
				return apperrors.Validation(field+".format", "format must be \"tar\" or \"squashfs\"")
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Archive) string { return a.In }),
	}

	UnarchiveDef = TypeDef{
		Type: "unarchive",
		New:  func() Artifact { return &Unarchive{} },
		Validate: TypedValidator(func(field string, a *Unarchive) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (dest) is required")
			}
			if err := validatePath(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (dest): "+err.Error())
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Unarchive) string { return a.In }),
	}

	MountDef = TypeDef{
		Type: "mount",
		New:  func() Artifact { return &Mount{} },
		Validate: TypedValidator(func(field string, a *Mount) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (image path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			if a.Out == "" {
				return apperrors.Validation(field+".out", "out (mount dir) is required")
			}
			if err := validatePath(a.Out); err != nil {
				return apperrors.Validation(field+".out", "invalid out (path): "+err.Error())
			}
			if a.Size < 0 {
				return apperrors.Validation(field+".size", "size must not be negative")
			}
			if a.Size > 0 && !a.Writable {
				return apperrors.Validation(field+".size", "size only applies to writable mounts")
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Mount) string { return a.In }),
	}

	StatDef = TypeDef{
		Type: "stat",
		New:  func() Artifact { return &Stat{} },
		Validate: TypedValidator(func(field string, a *Stat) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Stat) string { return a.In }),
	}

	ListDef = TypeDef{
		Type: "list",
		New:  func() Artifact { return &List{} },
		Validate: TypedValidator(func(field string, a *List) error {
			if a.In == "" {
				return apperrors.Validation(field+".in", "in (path) is required")
			}
			if err := validatePath(a.In); err != nil {
				return apperrors.Validation(field+".in", "invalid in (path): "+err.Error())
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *List) string { return a.In }),
	}
)
