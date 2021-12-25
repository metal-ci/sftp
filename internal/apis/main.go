package apis

import (
	"io/fs"
	"os"
	"time"
)

type File interface {
	Chdir() error
	Chmod(mode fs.FileMode) error
	Chown(uid, gid int) error
	Close() error
	Fd() uintptr
	Name() string
	Read(b []byte) (n int, err error)
	ReadAt(b []byte, off int64) (n int, err error)
	Seek(offset int64, whence int) (ret int64, err error)
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	Write(b []byte) (n int, err error)
	WriteAt(b []byte, off int64) (n int, err error)
	WriteString(s string) (n int, err error)
	ReadDir(n int) ([]fs.DirEntry, error)
	Readdirnames(n int) (names []string, err error)
}

type Fs interface {
	Chtimes(name string, atime, mtime time.Time) error
	Chmod(name string, mode os.FileMode) error
	Chown(name string, uid, gid int) error
	Mkdir(name string, perm os.FileMode) error
	Lstat(name string) (os.FileInfo, error)
	OpenFile(name string, flag int, perm os.FileMode) (File, error)
	ReadDir(path string) ([]os.DirEntry, error)
	Readlink(name string) (string, error)
	Remove(name string) error
	Rename(oldpath, newpath string) error
	Stat(name string) (os.FileInfo, error)
	Symlink(oldname, newname string) error
	Truncate(name string, size int64) error
	Open(name string) (File, error)
	RemoveAll(path string) error
	Create(name string) (File, error)
	TempDir() string
	Link(oldname string, newname string) error
}
