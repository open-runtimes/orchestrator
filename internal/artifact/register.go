package artifact

import "orchestrator/internal/apperrors"

func init() {
	Register(TypeDef{
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
	})

	Register(TypeDef{
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
	})

	Register(TypeDef{
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
	})

	Register(TypeDef{
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
	})

	Register(TypeDef{
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
			if a.Format != "tar.gz" {
				return apperrors.Validation(field+".format", "format must be \"tar.gz\"")
			}
			return nil
		}),
		SourcePath: TypedSourcePath(func(a *Archive) string { return a.In }),
	})

	Register(TypeDef{
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
	})

	Register(TypeDef{
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
	})
}
