// +build aix darwin dragonfly freebsd !android,linux netbsd openbsd solaris js

package sftp

import (
	"io/fs"
	"syscall"
)

func lsLinksUIDGID(fi fs.FileInfo) (numLinks uint64, uid, gid string) {
	numLinks = 1
	uid, gid = "0", "0"

	switch sys := fi.Sys().(type) {
	case *syscall.Stat_t:
		numLinks = uint64(sys.Nlink)
		uid = lsFormatID(sys.Uid)
		gid = lsFormatID(sys.Gid)
	default:
	}

	return numLinks, uid, gid
}
