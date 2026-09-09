package artifact

import (
	"archive/tar"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/klauspost/pgzip"
)

// ErrorCode is the stable value of a failed artifact callback's error field.
// Detailed errors remain in sidecar logs; they are not part of the wire enum.
type ErrorCode string

const (
	ArtifactWriteFailed           ErrorCode = "artifact_write_failed"
	ArchiveEmpty                  ErrorCode = "archive_empty"
	ArchiveUnknownFormat          ErrorCode = "archive_unknown_format"
	ArchiveCorrupt                ErrorCode = "archive_corrupt"
	ArchivePathInvalid            ErrorCode = "archive_path_invalid"
	ArchiveLayoutMismatch         ErrorCode = "archive_layout_mismatch"
	ArchiveExtractionFailed       ErrorCode = "archive_extraction_failed"
	ArchiveCreationFailed         ErrorCode = "archive_creation_failed"
	ArchiveCompressionUnsupported ErrorCode = "archive_compression_unsupported"
	ArtifactNotFound              ErrorCode = "artifact_not_found"
	ArtifactPermissionDenied      ErrorCode = "artifact_permission_denied"
	ArtifactTimeout               ErrorCode = "artifact_timeout"
	ArtifactCanceled              ErrorCode = "artifact_canceled"
	ArtifactReadFailed            ErrorCode = "artifact_read_failed"
	ArtifactStatFailed            ErrorCode = "artifact_stat_failed"
	ArtifactListFailed            ErrorCode = "artifact_list_failed"
	ArtifactJSONInvalid           ErrorCode = "artifact_json_invalid"
	DownloadFailed                ErrorCode = "download_failed"
	DownloadHTTPError             ErrorCode = "download_http_error"
	UploadFailed                  ErrorCode = "upload_failed"
	CloneFailed                   ErrorCode = "clone_failed"
	MountFailed                   ErrorCode = "mount_failed"
	ArtifactFailed                ErrorCode = "artifact_failed"
)

// Error lets a producer wrap a specific failure while retaining its cause with
// fmt.Errorf("%w: ...", code). Classification does not parse diagnostic text.
func (c ErrorCode) Error() string { return string(c) }

func (c ErrorCode) Valid() bool {
	switch c {
	case ArchiveEmpty, ArchiveUnknownFormat, ArchiveCorrupt, ArchivePathInvalid,
		ArchiveLayoutMismatch, ArchiveExtractionFailed, ArchiveCreationFailed,
		ArchiveCompressionUnsupported, ArtifactNotFound, ArtifactPermissionDenied,
		ArtifactTimeout, ArtifactCanceled, ArtifactReadFailed, ArtifactWriteFailed, ArtifactStatFailed,
		ArtifactListFailed, ArtifactJSONInvalid, DownloadFailed, DownloadHTTPError,
		UploadFailed, CloneFailed, MountFailed, ArtifactFailed:
		return true
	}
	return false
}

// FailureCode classifies the operation's actual error. Unclassified codec/tool
// errors get an operation-level fallback rather than blaming the archive.
func FailureCode(kind string, err error) ErrorCode {
	var code ErrorCode
	if errors.As(err, &code) && code.Valid() {
		return code
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ArtifactTimeout
	case errors.Is(err, context.Canceled):
		return ArtifactCanceled
	case errors.Is(err, fs.ErrNotExist):
		return ArtifactNotFound
	case errors.Is(err, fs.ErrPermission):
		return ArtifactPermissionDenied
	}
	if kind == "unarchive" || kind == "mount" {
		var corrupt flate.CorruptInputError
		if errors.As(err, &corrupt) || errors.Is(err, pgzip.ErrHeader) || errors.Is(err, pgzip.ErrChecksum) ||
			errors.Is(err, tar.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ArchiveCorrupt
		}
	}
	if kind == "read" {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return ArtifactJSONInvalid
		}
	}
	switch kind {
	case "unarchive":
		return ArchiveExtractionFailed
	case "archive":
		return ArchiveCreationFailed
	case "download":
		return DownloadFailed
	case "upload":
		return UploadFailed
	case "clone":
		return CloneFailed
	case "read":
		return ArtifactReadFailed
	case "write":
		return ArtifactWriteFailed
	case "stat":
		return ArtifactStatFailed
	case "list":
		return ArtifactListFailed
	case "mount":
		return MountFailed
	default:
		return ArtifactFailed
	}
}
