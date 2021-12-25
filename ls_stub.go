// +build windows android

package sftp

import "io/fs"

func lsLinksUIDGID(fi fs.FileInfo) (numLinks uint64, uid, gid string) {
	return 1, "0", "0"
}
