package artifact

import (
	"archive/tar"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"

	"github.com/klauspost/pgzip"
)

// CodeError is the stable code of a failed artifact callback's error object.
// It is the branchable half; the detail travels beside it as the message.
type CodeError string

const (
	ErrArtifactWriteFailed           CodeError = "artifact_write_failed"
	ErrArchiveEmpty                  CodeError = "archive_empty"
	ErrArchiveUnknownFormat          CodeError = "archive_unknown_format"
	ErrArchiveCorrupt                CodeError = "archive_corrupt"
	ErrArchivePathInvalid            CodeError = "archive_path_invalid"
	ErrArchiveLayoutMismatch         CodeError = "archive_layout_mismatch"
	ErrArchiveExtractionFailed       CodeError = "archive_extraction_failed"
	ErrArchiveCreationFailed         CodeError = "archive_creation_failed"
	ErrArchiveCompressionUnsupported CodeError = "archive_compression_unsupported"
	ErrArtifactNotFound              CodeError = "artifact_not_found"
	ErrArtifactPermissionDenied      CodeError = "artifact_permission_denied"
	ErrArtifactTimeout               CodeError = "artifact_timeout"
	ErrArtifactCanceled              CodeError = "artifact_canceled"
	ErrArtifactReadFailed            CodeError = "artifact_read_failed"
	ErrArtifactStatFailed            CodeError = "artifact_stat_failed"
	ErrArtifactListFailed            CodeError = "artifact_list_failed"
	ErrArtifactJSONInvalid           CodeError = "artifact_json_invalid"
	ErrDownloadFailed                CodeError = "download_failed"
	ErrDownloadHTTPError             CodeError = "download_http_error"
	ErrUploadFailed                  CodeError = "upload_failed"
	ErrCloneFailed                   CodeError = "clone_failed"
	ErrMountFailed                   CodeError = "mount_failed"
	ErrArtifactFailed                CodeError = "artifact_failed"
)

// Error lets a producer wrap a specific failure while retaining its cause with
// fmt.Errorf("%w: ...", code). Classification does not parse diagnostic text.
func (c CodeError) Error() string { return string(c) }

func (c CodeError) Valid() bool {
	switch c {
	case ErrArchiveEmpty, ErrArchiveUnknownFormat, ErrArchiveCorrupt, ErrArchivePathInvalid,
		ErrArchiveLayoutMismatch, ErrArchiveExtractionFailed, ErrArchiveCreationFailed,
		ErrArchiveCompressionUnsupported, ErrArtifactNotFound, ErrArtifactPermissionDenied,
		ErrArtifactTimeout, ErrArtifactCanceled, ErrArtifactReadFailed, ErrArtifactWriteFailed, ErrArtifactStatFailed,
		ErrArtifactListFailed, ErrArtifactJSONInvalid, ErrDownloadFailed, ErrDownloadHTTPError,
		ErrUploadFailed, ErrCloneFailed, ErrMountFailed, ErrArtifactFailed:
		return true
	}
	return false
}

// FailureMessage renders the human-readable half of a callback error. Producers
// wrap a code as fmt.Errorf("%w: detail", ErrX), so the code prefix is stripped:
// it travels in its own field, and repeating it reads badly in a message.
func FailureMessage(err error) string {
	msg := err.Error()
	var code CodeError
	if errors.As(err, &code) {
		msg = strings.TrimPrefix(msg, string(code)+": ")
	}
	return msg
}

// FailureCode classifies the operation's actual error. Unclassified codec/tool
// errors get an operation-level fallback rather than blaming the archive.
func FailureCode(kind string, err error) CodeError {
	var code CodeError
	if errors.As(err, &code) && code.Valid() {
		return code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrArtifactTimeout
	case errors.Is(err, context.Canceled):
		return ErrArtifactCanceled
	case errors.Is(err, fs.ErrNotExist):
		return ErrArtifactNotFound
	case errors.Is(err, fs.ErrPermission):
		return ErrArtifactPermissionDenied
	}
	if kind == "unarchive" || kind == "mount" {
		var corrupt flate.CorruptInputError
		if errors.As(err, &corrupt) || errors.Is(err, pgzip.ErrHeader) || errors.Is(err, pgzip.ErrChecksum) ||
			errors.Is(err, tar.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrArchiveCorrupt
		}
	}
	if kind == "read" {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return ErrArtifactJSONInvalid
		}
	}
	switch kind {
	case "unarchive":
		return ErrArchiveExtractionFailed
	case "archive":
		return ErrArchiveCreationFailed
	case "download":
		return ErrDownloadFailed
	case "upload":
		return ErrUploadFailed
	case "clone":
		return ErrCloneFailed
	case "read":
		return ErrArtifactReadFailed
	case "write":
		return ErrArtifactWriteFailed
	case "stat":
		return ErrArtifactStatFailed
	case "list":
		return ErrArtifactListFailed
	case "mount":
		return ErrMountFailed
	default:
		return ErrArtifactFailed
	}
}
