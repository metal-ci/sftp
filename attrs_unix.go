// +build darwin dragonfly freebsd !android,linux netbsd openbsd solaris aix js

package sftp

import (
	"io/fs"
	"syscall"
)

func fileStatFromInfoOs(fi fs.FileInfo, flags *uint32, fileStat *FileStat) {
	if statt, ok := fi.Sys().(*syscall.Stat_t); ok {
		*flags |= sshFileXferAttrUIDGID
		fileStat.UID = statt.Uid
		fileStat.GID = statt.Gid
	}
}
