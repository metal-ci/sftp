package sftp

import (
	"io/fs"
)

// ensure that attrs implemenst os.FileInfo
var _ fs.FileInfo = new(fileInfo)
