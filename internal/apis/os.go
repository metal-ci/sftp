package apis

import (
	"os"
	"time"
)

type OS struct {
}

func NewOS() *OS {
	return &OS{}
}

func (*OS) Chtimes(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}

func (*OS) Chmod(name string, mode os.FileMode) error {
	return os.Chmod(name, mode)
}

func (*OS) Chown(name string, uid, gid int) error {
	return os.Chown(name, uid, gid)
}

func (*OS) Mkdir(name string, perm os.FileMode) error {
	return os.Mkdir(name, perm)
}

func (*OS) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

func (*OS) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	return os.OpenFile(name, flag, perm)
}

func (*OS) ReadDir(name string) ([]os.DirEntry, error) {
	return os.ReadDir(name)
}

func (*OS) Readlink(name string) (string, error) {
	return os.Readlink(name)
}

func (*OS) Remove(name string) error {
	return os.Remove(name)
}

func (*OS) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (*OS) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (*OS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

func (*OS) Truncate(name string, size int64) error {
	return os.Truncate(name, size)
}

func (*OS) Open(name string) (File, error) {
	return os.Open(name)
}

func (*OS) RemoveAll(name string) error {
	return os.RemoveAll(name)
}

func (*OS) Create(name string) (File, error) {
	return os.Create(name)
}

func (*OS) Getwd() (string, error) {
	return os.Getwd()
}

func (*OS) TempDir() string {
	return os.TempDir()
}

func (*OS) Link(oldname string, newname string) error {
	return os.Link(oldname, newname)
}
