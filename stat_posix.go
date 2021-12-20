//go:build !plan9
// +build !plan9

package sftp

import (
	"io/fs"
	"syscall"
)

const EBADF = syscall.EBADF

func wrapPathError(filepath string, err error) error {
	if errno, ok := err.(syscall.Errno); ok {
		return &fs.PathError{Path: filepath, Err: errno}
	}
	return err
}

// translateErrno translates a syscall error number to a SFTP error code.
func translateErrno(errno syscall.Errno) uint32 {
	switch errno {
	case 0:
		return sshFxOk
	case syscall.ENOENT:
		return sshFxNoSuchFile
	case syscall.EACCES, syscall.EPERM:
		return sshFxPermissionDenied
	}

	return sshFxFailure
}

func translateSyscallError(err error) (uint32, bool) {
	switch e := err.(type) {
	case syscall.Errno:
		return translateErrno(e), true
	case *fs.PathError:
		debug("statusFromError,pathError: error is %T %#v", e.Err, e.Err)
		if errno, ok := e.Err.(syscall.Errno); ok {
			return translateErrno(errno), true
		}
	}
	return 0, false
}

// isRegular returns true if the mode describes a regular file.
func isRegular(mode uint32) bool {
	return mode&S_IFMT == syscall.S_IFREG
}

// toFileMode converts sftp filemode bits to the fs.FileMode specification
func toFileMode(mode uint32) fs.FileMode {
	var fm = fs.FileMode(mode & 0777)

	switch mode & S_IFMT {
	case syscall.S_IFBLK:
		fm |= fs.ModeDevice
	case syscall.S_IFCHR:
		fm |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFDIR:
		fm |= fs.ModeDir
	case syscall.S_IFIFO:
		fm |= fs.ModeNamedPipe
	case syscall.S_IFLNK:
		fm |= fs.ModeSymlink
	case syscall.S_IFREG:
		// nothing to do
	case syscall.S_IFSOCK:
		fm |= fs.ModeSocket
	}

	if mode&syscall.S_ISUID != 0 {
		fm |= fs.ModeSetuid
	}
	if mode&syscall.S_ISGID != 0 {
		fm |= fs.ModeSetgid
	}
	if mode&syscall.S_ISVTX != 0 {
		fm |= fs.ModeSticky
	}

	return fm
}

// fromFileMode converts from the fs.FileMode specification to sftp filemode bits
func fromFileMode(mode fs.FileMode) uint32 {
	ret := uint32(mode & fs.ModePerm)

	switch mode & fs.ModeType {
	case fs.ModeDevice | fs.ModeCharDevice:
		ret |= syscall.S_IFCHR
	case fs.ModeDevice:
		ret |= syscall.S_IFBLK
	case fs.ModeDir:
		ret |= syscall.S_IFDIR
	case fs.ModeNamedPipe:
		ret |= syscall.S_IFIFO
	case fs.ModeSymlink:
		ret |= syscall.S_IFLNK
	case 0:
		ret |= syscall.S_IFREG
	case fs.ModeSocket:
		ret |= syscall.S_IFSOCK
	}

	if mode&fs.ModeSetuid != 0 {
		ret |= syscall.S_ISUID
	}
	if mode&fs.ModeSetgid != 0 {
		ret |= syscall.S_ISGID
	}
	if mode&fs.ModeSticky != 0 {
		ret |= syscall.S_ISVTX
	}

	return ret
}

const (
	s_ISUID = syscall.S_ISUID
	s_ISGID = syscall.S_ISGID
	s_ISVTX = syscall.S_ISVTX
)
