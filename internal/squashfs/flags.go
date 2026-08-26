package squashfs

import "strings"

type Flags uint16

const (
	FlagUncompressedInodes Flags = 1 << iota
	FlagUncompressedData
	FlagCheck
	FlagUncompressedFragments
	FlagNoFragments
	FlagAlwaysFragments
	FlagDuplicates
	FlagExportable
	FlagUncompressedXattrs
	FlagNoXattrs
	FlagCompressorOptions
	FlagUncompressedIDs
)

func (f Flags) String() string {
	var opt []string

	if f&FlagUncompressedInodes != 0 {
		opt = append(opt, "FlagUncompressedInodes")
	}
	if f&FlagUncompressedData != 0 {
		opt = append(opt, "FlagUncompressedData")
	}
	if f&FlagCheck != 0 {
		opt = append(opt, "FlagCheck")
	}
	if f&FlagUncompressedFragments != 0 {
		opt = append(opt, "FlagUncompressedFragments")
	}
	if f&FlagNoFragments != 0 {
		opt = append(opt, "FlagNoFragments")
	}
	if f&FlagAlwaysFragments != 0 {
		opt = append(opt, "FlagAlwaysFragments")
	}
	if f&FlagDuplicates != 0 {
		opt = append(opt, "FlagDuplicates")
	}
	if f&FlagExportable != 0 {
		opt = append(opt, "FlagExportable")
	}
	if f&FlagUncompressedXattrs != 0 {
		opt = append(opt, "FlagUncompressedXattrs")
	}
	if f&FlagNoXattrs != 0 {
		opt = append(opt, "FlagNoXattrs")
	}
	if f&FlagCompressorOptions != 0 {
		opt = append(opt, "FlagCompressorOptions")
	}
	if f&FlagUncompressedIDs != 0 {
		opt = append(opt, "FlagUncompressedIDs")
	}

	return strings.Join(opt, "|")
}

func (f Flags) Has(what Flags) bool {
	return f&what == what
}
