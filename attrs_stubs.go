// +build plan9 windows android

package sftp

import "io/fs"

func fileStatFromInfoOs(fi fs.FileInfo, flags *uint32, fileStat *FileStat) {
	// todo
}
