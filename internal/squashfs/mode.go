package squashfs

import (
	"io/fs"
)

// squashfs internal modes are based on linux, so use these methods:
// based on: https://golang.org/src/os/stat_linux.go

const (
	sIFMT   = 0xf000
	sIFREG  = 0x8000
	sIFDIR  = 0x4000
	sIFBLK  = 0x6000
	sIFCHR  = 0x2000
	sIFIFO  = 0x1000
	sIFLNK  = 0xa000
	sIFSOCK = 0xc000

	sISVTX = 0x200
	sISGID = 0x400
	sISUID = 0x800
)

func unixToMode(mode uint32) fs.FileMode {
	res := fs.FileMode(mode & 0777)

	switch {
	case mode&sIFCHR == sIFCHR:
		res |= fs.ModeCharDevice
	case mode&sIFBLK == sIFBLK:
		res |= fs.ModeDevice
	case mode&sIFDIR == sIFDIR:
		res |= fs.ModeDir
	case mode&sIFIFO == sIFIFO:
		res |= fs.ModeNamedPipe
	case mode&sIFLNK == sIFLNK:
		res |= fs.ModeSymlink
	case mode&sIFSOCK == sIFSOCK:
		res |= fs.ModeSocket
	}

	// extra flags
	if mode&sISGID == sISGID {
		res |= fs.ModeSetgid
	}

	if mode&sISUID == sISUID {
		res |= fs.ModeSetuid
	}

	if mode&sISVTX == sISVTX {
		res |= fs.ModeSticky
	}

	return res
}

func modeToUnix(mode fs.FileMode) uint32 {
	res := uint32(mode.Perm())

	// type of file
	switch {
	case mode&fs.ModeCharDevice == fs.ModeCharDevice:
		res |= sIFCHR
	case mode&fs.ModeDevice == fs.ModeDevice:
		res |= sIFBLK
	case mode&fs.ModeDir == fs.ModeDir:
		res |= sIFDIR
	case mode&fs.ModeNamedPipe == fs.ModeNamedPipe:
		res |= sIFIFO
	case mode&fs.ModeSymlink == fs.ModeSymlink:
		res |= sIFLNK
	case mode&fs.ModeSocket == fs.ModeSocket:
		res |= sIFSOCK
	default:
		res |= sIFREG
	}

	// extra flags
	if mode&fs.ModeSetgid == fs.ModeSetgid {
		res |= sISGID
	}

	if mode&fs.ModeSetuid == fs.ModeSetuid {
		res |= sISUID
	}

	if mode&fs.ModeSticky == fs.ModeSticky {
		res |= sISVTX
	}

	return res
}
