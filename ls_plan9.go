// +build plan9

package sftp

import (
	"io/fs"
	"syscall"
)

func lsLinksUIDGID(fi fs.FileInfo) (numLinks uint64, uid, gid string) {
	numLinks = 1
	uid, gid = "0", "0"

	switch sys := fi.Sys().(type) {
	case *syscall.Dir:
		uid = sys.Uid
		gid = sys.Gid
	}

	return numLinks, uid, gid
}
