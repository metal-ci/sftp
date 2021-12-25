package apis

import (
	"io/fs"
	"os"
	"time"

	"github.com/avfs/avfs"
	"github.com/avfs/avfs/vfs/osfs"
)

type AVFS struct {
	fs avfs.VFS
}

func NewAVFS() *AVFS {
	return &AVFS{
		fs: osfs.New(),
	}
}

func (api *AVFS) Chtimes(name string, atime, mtime time.Time) error {
	return api.fs.Chtimes(name, atime, mtime)
}

func (api *AVFS) Chmod(name string, mode os.FileMode) error {
	return api.fs.Chmod(name, mode)
}

func (api *AVFS) Chown(name string, uid, gid int) error {
	return api.fs.Chown(name, uid, gid)
}

func (api *AVFS) Mkdir(name string, perm os.FileMode) error {
	return api.fs.Mkdir(name, perm)
}

func (api *AVFS) Lstat(name string) (fs.FileInfo, error) {
	return api.fs.Lstat(name)
}

func (api *AVFS) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	return api.fs.OpenFile(name, flag, perm)
}

func (api *AVFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return api.fs.ReadDir(name)
}

func (api *AVFS) Readlink(name string) (string, error) {
	return api.fs.Readlink(name)
}

func (api *AVFS) Remove(name string) error {
	return api.fs.Remove(name)
}

func (api *AVFS) Rename(oldpath, newpath string) error {
	return api.fs.Rename(oldpath, newpath)
}

func (api *AVFS) Stat(name string) (fs.FileInfo, error) {
	return api.fs.Stat(name)
}

func (api *AVFS) Symlink(oldname, newname string) error {
	return api.fs.Symlink(oldname, newname)
}

func (api *AVFS) Truncate(name string, size int64) error {
	return api.fs.Truncate(name, size)
}

func (api *AVFS) Open(name string) (File, error) {
	return api.fs.Open(name)
}

func (api *AVFS) RemoveAll(path string) error {
	return api.fs.RemoveAll(path)
}

func (api *AVFS) Create(name string) (File, error) {
	return api.fs.Create(name)
}

func (api *AVFS) Getwd() (string, error) {
	return api.fs.Getwd()
}

func (api *AVFS) TempDir() string {
	return api.fs.TempDir()
}

func (api *AVFS) Link(oldname string, newname string) error {
	return api.fs.Link(oldname, newname)
}
