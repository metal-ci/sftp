package sftp

// sftp integration tests
// enable with -integration

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"testing/quick"
	"time"

	"github.com/pkg/sftp/internal/apis"

	"github.com/kr/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	READONLY                = true
	READWRITE               = false
	NODELAY   time.Duration = 0

	debuglevel = "ERROR" // set to "DEBUG" for debugging
)

type delayedWrite struct {
	t time.Time
	b []byte
}

// delayedWriter wraps a writer and artificially delays the write. This is
// meant to mimic connections with various latencies. Error's returned from the
// underlying writer will panic so this should only be used over reliable
// connections.
type delayedWriter struct {
	closed chan struct{}

	mu      sync.Mutex
	ch      chan delayedWrite
	closing chan struct{}
}

func newDelayedWriter(w io.WriteCloser, delay time.Duration) io.WriteCloser {
	dw := &delayedWriter{
		ch:      make(chan delayedWrite, 128),
		closed:  make(chan struct{}),
		closing: make(chan struct{}),
	}

	go func() {
		defer close(dw.closed)
		defer w.Close()

		for writeMsg := range dw.ch {
			time.Sleep(time.Until(writeMsg.t.Add(delay)))

			n, err := w.Write(writeMsg.b)
			if err != nil {
				panic("write error")
			}

			if n < len(writeMsg.b) {
				panic("showrt write")
			}
		}
	}()

	return dw
}

func (dw *delayedWriter) Write(b []byte) (int, error) {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	write := delayedWrite{
		t: time.Now(),
		b: append([]byte(nil), b...),
	}

	select {
	case <-dw.closing:
		return 0, errors.New("delayedWriter is closing")
	case dw.ch <- write:
	}

	return len(b), nil
}

func (dw *delayedWriter) Close() error {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	select {
	case <-dw.closing:
	default:
		close(dw.ch)
		close(dw.closing)
	}

	<-dw.closed
	return nil
}

// netPipe provides a pair of io.ReadWriteClosers connected to each other.
// The functions is identical to os.Pipe with the exception that netPipe
// provides the Read/Close guarantees that os.File derrived pipes do not.
func netPipe(t testing.TB) (io.ReadWriteCloser, io.ReadWriteCloser) {
	type result struct {
		net.Conn
		error
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	closeListener := make(chan struct{}, 1)
	closeListener <- struct{}{}

	ch := make(chan result, 1)
	go func() {
		conn, err := l.Accept()
		ch <- result{conn, err}

		if _, ok := <-closeListener; ok {
			err = l.Close()
			if err != nil {
				t.Error(err)
			}
			close(closeListener)
		}
	}()

	c1, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		if _, ok := <-closeListener; ok {
			l.Close()
			close(closeListener)
		}
		t.Fatal(err)
	}

	r := <-ch
	if r.error != nil {
		t.Fatal(err)
	}

	return c1, r.Conn
}

func testClientGoSvr(t testing.TB, readonly bool, delay time.Duration) (*Client, *exec.Cmd) {
	c1, c2 := netPipe(t)

	options := []ServerOption{WithDebug(os.Stderr)}
	if readonly {
		options = append(options, ReadOnly())
	}

	server, err := NewServer(c1, apis.NewAVFS(), options...)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()

	var wr io.WriteCloser = c2
	if delay > NODELAY {
		wr = newDelayedWriter(wr, delay)
	}

	client, err := NewClientPipe(c2, wr)
	if err != nil {
		t.Fatal(err)
	}

	// dummy command...
	return client, exec.Command("true")
}

// testClient returns a *Client connected to a locally running sftp-server
// the *exec.Cmd returned must be defer Wait'd.
func testClient(t testing.TB, readonly bool, delay time.Duration) (*Client, *exec.Cmd) {
	if !*testIntegration {
		t.Skip("skipping integration test")
	}

	if *testServerImpl {
		return testClientGoSvr(t, readonly, delay)
	}

	cmd := exec.Command(*testSftp, "-e", "-R", "-l", debuglevel) // log to stderr, read only
	if !readonly {
		cmd = exec.Command(*testSftp, "-e", "-l", debuglevel) // log to stderr
	}

	cmd.Stderr = os.Stdout

	pw, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	if delay > NODELAY {
		pw = newDelayedWriter(pw, delay)
	}

	pr, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Skipf("could not start sftp-server process: %v", err)
	}

	sftp, err := NewClientPipe(pr, pw)
	if err != nil {
		t.Fatal(err)
	}

	return sftp, cmd
}

func TestNewClient(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()

	if err := sftp.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientLstat(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-lstat")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer fsApi.Remove(f.Name())

	want, err := fsApi.Lstat(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := sftp.Lstat(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	if !sameFile(want, got) {
		t.Fatalf("Lstat(%q): want %#v, got %#v", f.Name(), want, got)
	}
}

func TestClientLstatIsNotExist(t *testing.T) {
	fsApi := apis.NewAVFS()
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-lstatisnotexist")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fsApi.Remove(f.Name())

}

func TestClientMkdir(t *testing.T) {
	fsApi := apis.NewAVFS()
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-mkdir")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(dir)

	sub := path.Join(dir, "mkdir1")
	if err := sftp.Mkdir(sub); err != nil {
		t.Fatal(err)
	}
	if _, err := fsApi.Lstat(sub); err != nil {
		t.Fatal(err)
	}
}
func TestClientMkdirAll(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-mkdirall")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(dir)

	sub := path.Join(dir, "mkdir1", "mkdir2", "mkdir3")
	if err := sftp.MkdirAll(sub); err != nil {
		t.Fatal(err)
	}
	info, err := fsApi.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("Expected mkdirall to create dir at: %s", sub)
	}
}

func TestClientOpen(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-open")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer fsApi.Remove(f.Name())

	got, err := sftp.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientOpenIsNotExist(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

}

func TestClientStatIsNotExist(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

}

const seekBytes = 128 * 1024

type seek struct {
	offset int64
}

func (s seek) Generate(r *rand.Rand, _ int) reflect.Value {
	s.offset = int64(r.Int31n(seekBytes))
	return reflect.ValueOf(s)
}

func (s seek) set(t *testing.T, r io.ReadSeeker) {
	if _, err := r.Seek(s.offset, io.SeekStart); err != nil {
		t.Fatalf("error while seeking with %+v: %v", s, err)
	}
}

func (s seek) current(t *testing.T, r io.ReadSeeker) {
	const mid = seekBytes / 2

	skip := s.offset / 2
	if s.offset > mid {
		skip = -skip
	}

	if _, err := r.Seek(mid, io.SeekStart); err != nil {
		t.Fatalf("error seeking to midpoint with %+v: %v", s, err)
	}
	if _, err := r.Seek(skip, io.SeekCurrent); err != nil {
		t.Fatalf("error seeking from %d with %+v: %v", mid, s, err)
	}
}

func (s seek) end(t *testing.T, r io.ReadSeeker) {
	if _, err := r.Seek(-s.offset, io.SeekEnd); err != nil {
		t.Fatalf("error seeking from end with %+v: %v", s, err)
	}
}

func TestClientSeek(t *testing.T) {
	fsApi := apis.NewAVFS()
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	fOS, err := ioutil.TempFile("", "sftptest-seek")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(fOS.Name())
	defer fOS.Close()

	fSFTP, err := sftp.Open(fOS.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer fSFTP.Close()

	writeN(t, fOS, seekBytes)

	if err := quick.CheckEqual(
		func(s seek) (string, int64) { s.set(t, fOS); return readHash(t, fOS) },
		func(s seek) (string, int64) { s.set(t, fSFTP); return readHash(t, fSFTP) },
		nil,
	); err != nil {
		t.Errorf("Seek: expected equal absolute seeks: %v", err)
	}

	if err := quick.CheckEqual(
		func(s seek) (string, int64) { s.current(t, fOS); return readHash(t, fOS) },
		func(s seek) (string, int64) { s.current(t, fSFTP); return readHash(t, fSFTP) },
		nil,
	); err != nil {
		t.Errorf("Seek: expected equal seeks from middle: %v", err)
	}

	if err := quick.CheckEqual(
		func(s seek) (string, int64) { s.end(t, fOS); return readHash(t, fOS) },
		func(s seek) (string, int64) { s.end(t, fSFTP); return readHash(t, fSFTP) },
		nil,
	); err != nil {
		t.Errorf("Seek: expected equal seeks from end: %v", err)
	}
}

func TestClientCreate(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-create")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer fsApi.Remove(f.Name())

	f2, err := sftp.Create(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
}

func TestClientAppend(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-append")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer fsApi.Remove(f.Name())

	f2, err := sftp.OpenFile(f.Name(), syscall.O_RDWR|syscall.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
}

func TestClientCreateFailed(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-createfailed")
	require.NoError(t, err)

	defer f.Close()
	defer fsApi.Remove(f.Name())

	f2, err := sftp.Create(f.Name())
	if err == nil {
		f2.Close()
	}
}

func TestClientFileName(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-filename")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())

	f2, err := sftp.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()

	if got, want := f2.Name(), f.Name(); got != want {
		t.Fatalf("Name: got %q want %q", want, got)
	}
}

func TestClientFileStat(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-filestat")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())

	want, err := fsApi.Lstat(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	f2, err := sftp.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()

	got, err := f2.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if !sameFile(want, got) {
		t.Fatalf("Lstat(%q): want %#v, got %#v", f.Name(), want, got)
	}
}

func TestClientStatLink(t *testing.T) {
	skipIfWindows(t) // Windows does not support links.
	fsApi := apis.NewAVFS()
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-statlink")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())

	realName := f.Name()
	linkName := f.Name() + ".softlink"

	// create a symlink that points at sftptest
	if err := fsApi.Symlink(realName, linkName); err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(linkName)

	// compare Lstat of links
	wantLstat, err := fsApi.Lstat(linkName)
	if err != nil {
		t.Fatal(err)
	}
	wantStat, err := fsApi.Stat(linkName)
	if err != nil {
		t.Fatal(err)
	}

	gotLstat, err := sftp.Lstat(linkName)
	if err != nil {
		t.Fatal(err)
	}
	gotStat, err := sftp.Stat(linkName)
	if err != nil {
		t.Fatal(err)
	}

	// check that stat is not lstat from os package
	if sameFile(wantLstat, wantStat) {
		t.Fatalf("Lstat / Stat(%q): both %#v %#v", f.Name(), wantLstat, wantStat)
	}

	// compare Lstat of links
	if !sameFile(wantLstat, gotLstat) {
		t.Fatalf("Lstat(%q): want %#v, got %#v", f.Name(), wantLstat, gotLstat)
	}

	// compare Stat of links
	if !sameFile(wantStat, gotStat) {
		t.Fatalf("Stat(%q): want %#v, got %#v", f.Name(), wantStat, gotStat)
	}

	// check that stat is not lstat
	if sameFile(gotLstat, gotStat) {
		t.Fatalf("Lstat / Stat(%q): both %#v %#v", f.Name(), gotLstat, gotStat)
	}
}

func TestClientRemove(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-remove")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	if err := sftp.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
}

func TestClientRemoveDir(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-removedir")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(dir)

	if err := sftp.Remove(dir); err != nil {
		t.Fatal(err)
	}
}

func TestClientRemoveFailed(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-removefailed")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())

	if err := sftp.Remove(f.Name()); err == nil {
		t.Fatalf("Remove(%v): want: permission denied, got %v", f.Name(), err)
	}
	if _, err := fsApi.Lstat(f.Name()); err != nil {
		t.Fatal(err)
	}
}

func TestClientRename(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-rename")
	require.NoError(t, err)
	defer fsApi.RemoveAll(dir)
	f, err := fsApi.Create(filepath.Join(dir, "old"))
	require.NoError(t, err)
	f.Close()

	f2 := filepath.Join(dir, "new")
	if err := sftp.Rename(f.Name(), f2); err != nil {
		t.Fatal(err)
	}
	if _, err := fsApi.Lstat(f2); err != nil {
		t.Fatal(err)
	}
}

func TestClientPosixRename(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-posixrename")
	require.NoError(t, err)
	defer fsApi.RemoveAll(dir)
	f, err := fsApi.Create(filepath.Join(dir, "old"))
	require.NoError(t, err)
	f.Close()

	f2 := filepath.Join(dir, "new")
	if err := sftp.PosixRename(f.Name(), f2); err != nil {
		t.Fatal(err)
	}

	if _, err := fsApi.Lstat(f2); err != nil {
		t.Fatal(err)
	}
}

func TestClientGetwd(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	lwd, err := fsApi.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rwd, err := sftp.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rwd) {
		t.Fatalf("Getwd: wanted absolute path, got %q", rwd)
	}
	if filepath.ToSlash(lwd) != filepath.ToSlash(rwd) {
		t.Fatalf("Getwd: want %q, got %q", lwd, rwd)
	}
}

func TestClientReadLink(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-readlink")
	require.NoError(t, err)
	defer fsApi.RemoveAll(dir)
	f, err := fsApi.Create(filepath.Join(dir, "file"))
	require.NoError(t, err)
	f.Close()

	f2 := filepath.Join(dir, "symlink")
	if err := fsApi.Symlink(f.Name(), f2); err != nil {
		t.Fatal(err)
	}
	if rl, err := sftp.ReadLink(f2); err != nil {
		t.Fatal(err)
	} else if rl != f.Name() {
		t.Fatalf("unexpected link target: %v, not %v", rl, f.Name())
	}
}

func TestClientLink(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-link")
	require.NoError(t, err)
	defer fsApi.RemoveAll(dir)

	f, err := fsApi.Create(filepath.Join(dir, "file"))
	require.NoError(t, err)
	data := []byte("linktest")
	_, err = f.Write(data)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	f2 := filepath.Join(dir, "link")
	if err := sftp.Link(f.Name(), f2); err != nil {
		t.Fatal(err)
	}
	if st2, err := sftp.Stat(f2); err != nil {
		t.Fatal(err)
	} else if int(st2.Size()) != len(data) {
		t.Fatalf("unexpected link size: %v, not %v", st2.Size(), len(data))
	}
}

func TestClientSymlink(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-symlink")
	require.NoError(t, err)
	defer fsApi.RemoveAll(dir)
	f, err := fsApi.Create(filepath.Join(dir, "file"))
	require.NoError(t, err)
	f.Close()

	f2 := filepath.Join(dir, "symlink")
	if err := sftp.Symlink(f.Name(), f2); err != nil {
		t.Fatal(err)
	}
	if rl, err := sftp.ReadLink(f2); err != nil {
		t.Fatal(err)
	} else if rl != f.Name() {
		t.Fatalf("unexpected link target: %v, not %v", rl, f.Name())
	}
}

func TestClientChmod(t *testing.T) {
	skipIfWindows(t) // No UNIX permissions.
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-chmod")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	if err := sftp.Chmod(f.Name(), 0531); err != nil {
		t.Fatal(err)
	}
	if stat, err := fsApi.Stat(f.Name()); err != nil {
		t.Fatal(err)
	} else if stat.Mode() != 0531 {
		t.Fatalf("invalid perm %o\n", stat.Mode())
	}

	sf, err := sftp.Open(f.Name())
	require.NoError(t, err)
	require.NoError(t, sf.Chmod(0500))
	sf.Close()

	stat, err := fsApi.Stat(f.Name())
	require.NoError(t, err)
	require.EqualValues(t, 0500, stat.Mode())
}

func TestClientChmodReadonly(t *testing.T) {
	skipIfWindows(t) // No UNIX permissions.
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-chmodreadonly")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	if err := sftp.Chmod(f.Name(), 0531); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientSetuid(t *testing.T) {
	skipIfWindows(t) // No UNIX permissions.
	fsApi := apis.NewAVFS()
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}

	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-setuid")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	const allPerm = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky |
		s_ISUID | s_ISGID | s_ISVTX

	for _, c := range []struct {
		goPerm    os.FileMode
		posixPerm uint32
	}{
		{os.ModeSetuid, s_ISUID},
		{os.ModeSetgid, s_ISGID},
		{os.ModeSticky, s_ISVTX},
		{os.ModeSetuid | os.ModeSticky, s_ISUID | s_ISVTX},
	} {
		goPerm := 0700 | c.goPerm
		posixPerm := 0700 | c.posixPerm

		err = sftp.Chmod(f.Name(), goPerm)
		require.NoError(t, err)

		info, err := sftp.Stat(f.Name())
		require.NoError(t, err)
		require.Equal(t, goPerm, info.Mode()&allPerm)

		err = sftp.Chmod(f.Name(), 0700) // Reset funny bits.
		require.NoError(t, err)

		// For historical reasons, we also support literal POSIX mode bits in
		// Chmod. Stat should still translate these to Go os.FileMode bits.
		err = sftp.Chmod(f.Name(), os.FileMode(posixPerm))
		require.NoError(t, err)

		info, err = sftp.Stat(f.Name())
		require.NoError(t, err)
		require.Equal(t, goPerm, info.Mode()&allPerm)
	}
}

func TestClientChown(t *testing.T) {
	skipIfWindows(t) // No UNIX permissions.
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	usr, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if usr.Uid != "0" {
		t.Log("must be root to run chown tests")
		t.Skip()
	}

	chownto, err := user.Lookup("daemon") // seems common-ish...
	if err != nil {
		t.Fatal(err)
	}
	toUID, err := strconv.Atoi(chownto.Uid)
	if err != nil {
		t.Fatal(err)
	}
	toGID, err := strconv.Atoi(chownto.Gid)
	if err != nil {
		t.Fatal(err)
	}

	f, err := ioutil.TempFile("", "sftptest-chown")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	before, err := exec.Command("ls", "-nl", f.Name()).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := sftp.Chown(f.Name(), toUID, toGID); err != nil {
		t.Fatal(err)
	}
	after, err := exec.Command("ls", "-nl", f.Name()).Output()
	if err != nil {
		t.Fatal(err)
	}

	spaceRegex := regexp.MustCompile(`\s+`)

	beforeWords := spaceRegex.Split(string(before), -1)
	if beforeWords[2] != "0" {
		t.Fatalf("bad previous user? should be root")
	}
	afterWords := spaceRegex.Split(string(after), -1)
	if afterWords[2] != chownto.Uid || afterWords[3] != chownto.Gid {
		t.Fatalf("bad chown: %#v", afterWords)
	}
	t.Logf("before: %v", string(before))
	t.Logf(" after: %v", string(after))
}

func TestClientChownReadonly(t *testing.T) {
	skipIfWindows(t) // No UNIX permissions.
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	usr, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if usr.Uid != "0" {
		t.Log("must be root to run chown tests")
		t.Skip()
	}

	chownto, err := user.Lookup("daemon") // seems common-ish...
	if err != nil {
		t.Fatal(err)
	}
	toUID, err := strconv.Atoi(chownto.Uid)
	if err != nil {
		t.Fatal(err)
	}
	toGID, err := strconv.Atoi(chownto.Gid)
	if err != nil {
		t.Fatal(err)
	}

	f, err := ioutil.TempFile("", "sftptest-chownreadonly")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	if err := sftp.Chown(f.Name(), toUID, toGID); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientChtimes(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-chtimes")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	atime := time.Date(2013, 2, 23, 13, 24, 35, 0, time.UTC)
	mtime := time.Date(1985, 6, 12, 6, 6, 6, 0, time.UTC)
	if err := sftp.Chtimes(f.Name(), atime, mtime); err != nil {
		t.Fatal(err)
	}
	if stat, err := fsApi.Stat(f.Name()); err != nil {
		t.Fatal(err)
	} else if stat.ModTime().Sub(mtime) != 0 {
		t.Fatalf("incorrect mtime: %v vs %v", stat.ModTime(), mtime)
	}
}

func TestClientChtimesReadonly(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-chtimesreadonly")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	f.Close()

	atime := time.Date(2013, 2, 23, 13, 24, 35, 0, time.UTC)
	mtime := time.Date(1985, 6, 12, 6, 6, 6, 0, time.UTC)
	if err := sftp.Chtimes(f.Name(), atime, mtime); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientTruncate(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-truncate")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	fname := f.Name()

	if n, err := f.Write([]byte("hello world")); n != 11 || err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := sftp.Truncate(fname, 5); err != nil {
		t.Fatal(err)
	}
	if stat, err := fsApi.Stat(fname); err != nil {
		t.Fatal(err)
	} else if stat.Size() != 5 {
		t.Fatalf("unexpected size: %d", stat.Size())
	}
}

func TestClientTruncateReadonly(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-truncreadonly")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.Remove(f.Name())
	fname := f.Name()

	if n, err := f.Write([]byte("hello world")); n != 11 || err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := sftp.Truncate(fname, 5); err == nil {
		t.Fatal("expected error")
	}
	if stat, err := fsApi.Stat(fname); err != nil {
		t.Fatal(err)
	} else if stat.Size() != 11 {
		t.Fatalf("unexpected size: %d", stat.Size())
	}
}

func sameFile(want, got os.FileInfo) bool {
	_, wantName := filepath.Split(want.Name())
	_, gotName := filepath.Split(got.Name())
	return wantName == gotName &&
		want.Size() == got.Size()
}

func TestClientReadSimple(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-readsimple")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	f, err := ioutil.TempFile(d, "read-test")
	if err != nil {
		t.Fatal(err)
	}
	fname := f.Name()
	f.Write([]byte("hello"))
	f.Close()

	f2, err := sftp.Open(fname)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	stuff := make([]byte, 32)
	n, err := f2.Read(stuff)
	if err != nil && err != io.EOF {
		t.Fatalf("err: %v", err)
	}
	if n != 5 {
		t.Fatalf("n should be 5, is %v", n)
	}
	if string(stuff[0:5]) != "hello" {
		t.Fatalf("invalid contents")
	}
}

func TestClientReadSequential(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	sftp.disableConcurrentReads = true
	d, err := ioutil.TempDir("", "sftptest-readsequential")
	require.NoError(t, err)

	defer fsApi.RemoveAll(d)

	f, err := ioutil.TempFile(d, "read-sequential-test")
	require.NoError(t, err)
	fname := f.Name()
	content := []byte("hello world")
	f.Write(content)
	f.Close()

	for _, maxPktSize := range []int{1, 2, 3, 4} {
		sftp.maxPacket = maxPktSize

		sftpFile, err := sftp.Open(fname)
		require.NoError(t, err)

		stuff := make([]byte, 32)
		n, err := sftpFile.Read(stuff)
		require.NoError(t, err)
		require.Equal(t, len(content), n)
		require.Equal(t, content, stuff[0:len(content)])

		err = sftpFile.Close()
		require.NoError(t, err)

		sftpFile, err = sftp.Open(fname)
		require.NoError(t, err)

		stuff = make([]byte, 5)
		n, err = sftpFile.Read(stuff)
		require.NoError(t, err)
		require.Equal(t, len(stuff), n)
		require.Equal(t, content[:len(stuff)], stuff)

		err = sftpFile.Close()
		require.NoError(t, err)

		// now read from a offset
		off := int64(3)
		sftpFile, err = sftp.Open(fname)
		require.NoError(t, err)

		stuff = make([]byte, 5)
		n, err = sftpFile.ReadAt(stuff, off)
		require.NoError(t, err)
		require.Equal(t, len(stuff), n)
		require.Equal(t, content[off:off+int64(len(stuff))], stuff)

		err = sftpFile.Close()
		require.NoError(t, err)
	}
}

func TestClientReadDir(t *testing.T) {
	sftp1, cmd1 := testClient(t, READONLY, NODELAY)
	sftp2, cmd2 := testClientGoSvr(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd1.Wait()
	defer cmd2.Wait()
	defer sftp1.Close()
	defer sftp2.Close()

	dir := fsApi.TempDir()

	d, err := fsApi.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	osfiles, err := d.ReadDir(4096)
	if err != nil {
		t.Fatal(err)
	}

	sftp1Files, err := sftp1.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sftp2Files, err := sftp2.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	osFilesByName := map[string]os.FileInfo{}
	for _, f := range osfiles {
		fi, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		osFilesByName[f.Name()] = fi
	}
	sftp1FilesByName := map[string]os.FileInfo{}
	for _, f := range sftp1Files {
		sftp1FilesByName[f.Name()] = f
	}
	sftp2FilesByName := map[string]os.FileInfo{}
	for _, f := range sftp2Files {
		sftp2FilesByName[f.Name()] = f
	}

	if len(osFilesByName) != len(sftp1FilesByName) || len(sftp1FilesByName) != len(sftp2FilesByName) {
		t.Fatalf("os gives %v, sftp1 gives %v, sftp2 gives %v", len(osFilesByName), len(sftp1FilesByName), len(sftp2FilesByName))
	}

	for name, osF := range osFilesByName {
		sftp1F, ok := sftp1FilesByName[name]
		if !ok {
			t.Fatalf("%v present in os but not sftp1", name)
		}
		sftp2F, ok := sftp2FilesByName[name]
		if !ok {
			t.Fatalf("%v present in os but not sftp2", name)
		}

		//t.Logf("%v: %v %v %v", name, osF, sftp1F, sftp2F)
		if osF.Size() != sftp1F.Size() || sftp1F.Size() != sftp2F.Size() {
			t.Fatalf("size %v %v %v", osF.Size(), sftp1F.Size(), sftp2F.Size())
		}
		if osF.IsDir() != sftp1F.IsDir() || sftp1F.IsDir() != sftp2F.IsDir() {
			t.Fatalf("isdir %v %v %v", osF.IsDir(), sftp1F.IsDir(), sftp2F.IsDir())
		}
		if osF.ModTime().Sub(sftp1F.ModTime()) > time.Second || sftp1F.ModTime() != sftp2F.ModTime() {
			t.Fatalf("modtime %v %v %v", osF.ModTime(), sftp1F.ModTime(), sftp2F.ModTime())
		}
		if osF.Mode() != sftp1F.Mode() || sftp1F.Mode() != sftp2F.Mode() {
			t.Fatalf("mode %x %x %x", osF.Mode(), sftp1F.Mode(), sftp2F.Mode())
		}
	}
}

var clientReadTests = []struct {
	n int64
}{
	{0},
	{1},
	{1000},
	{1024},
	{1025},
	{2048},
	{4096},
	{1 << 12},
	{1 << 13},
	{1 << 14},
	{1 << 15},
	{1 << 16},
	{1 << 17},
	{1 << 18},
	{1 << 19},
	{1 << 20},
}

func TestClientRead(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-read")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	for _, disableConcurrentReads := range []bool{true, false} {
		for _, tt := range clientReadTests {
			f, err := ioutil.TempFile(d, "read-test")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			hash := writeN(t, f, tt.n)
			sftp.disableConcurrentReads = disableConcurrentReads
			f2, err := sftp.Open(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			defer f2.Close()
			hash2, n := readHash(t, f2)
			if hash != hash2 || tt.n != n {
				t.Errorf("Read: hash: want: %q, got %q, read: want: %v, got %v", hash, hash2, tt.n, n)
			}
		}
	}
}

// readHash reads r until EOF returning the number of bytes read
// and the hash of the contents.
func readHash(t *testing.T, r io.Reader) (string, int64) {
	h := sha1.New()
	read, err := io.Copy(h, r)
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil)), read
}

// writeN writes n bytes of random data to w and returns the
// hash of that data.
func writeN(t *testing.T, w io.Writer, n int64) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	h := sha1.New()

	mw := io.MultiWriter(w, h)

	written, err := io.CopyN(mw, r, n)
	if err != nil {
		t.Fatal(err)
	}
	if written != n {
		t.Fatalf("CopyN(%v): wrote: %v", n, written)
	}
	return string(h.Sum(nil))
}

var clientWriteTests = []struct {
	n     int
	total int64 // cumulative file size
}{
	{0, 0},
	{1, 1},
	{0, 1},
	{999, 1000},
	{24, 1024},
	{1023, 2047},
	{2048, 4095},
	{1 << 12, 8191},
	{1 << 13, 16383},
	{1 << 14, 32767},
	{1 << 15, 65535},
	{1 << 16, 131071},
	{1 << 17, 262143},
	{1 << 18, 524287},
	{1 << 19, 1048575},
	{1 << 20, 2097151},
	{1 << 21, 4194303},
}

func TestClientWrite(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-write")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	f := path.Join(d, "writeTest")
	w, err := sftp.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for _, tt := range clientWriteTests {
		got, err := w.Write(make([]byte, tt.n))
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.n {
			t.Errorf("Write(%v): wrote: want: %v, got %v", tt.n, tt.n, got)
		}
		fi, err := fsApi.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if total := fi.Size(); total != tt.total {
			t.Errorf("Write(%v): size: want: %v, got %v", tt.n, tt.total, total)
		}
	}
}

// ReadFrom is basically Write with io.Reader as the arg
func TestClientReadFrom(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-readfrom")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	f := path.Join(d, "writeTest")
	w, err := sftp.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for _, tt := range clientWriteTests {
		got, err := w.ReadFrom(bytes.NewReader(make([]byte, tt.n)))
		if err != nil {
			t.Fatal(err)
		}
		if got != int64(tt.n) {
			t.Errorf("Write(%v): wrote: want: %v, got %v", tt.n, tt.n, got)
		}
		fi, err := fsApi.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if total := fi.Size(); total != tt.total {
			t.Errorf("Write(%v): size: want: %v, got %v", tt.n, tt.total, total)
		}
	}
}

// Issue #145 in github
// Deadlock in ReadFrom when network drops after 1 good packet.
// Deadlock would occur anytime desiredInFlight-inFlight==2 and 2 errors
// occurred in a row. The channel to report the errors only had a buffer
// of 1 and 2 would be sent.
var errFakeNet = errors.New("Fake network issue")

func TestClientReadFromDeadlock(t *testing.T) {
	for i := 0; i < 5; i++ {
		clientWriteDeadlock(t, i, func(f *File) {
			b := make([]byte, 32768*4)
			content := bytes.NewReader(b)
			_, err := f.ReadFrom(content)
			if !errors.Is(err, errFakeNet) {
				t.Fatal("Didn't receive correct error:", err)
			}
		})
	}
}

// Write has exact same problem
func TestClientWriteDeadlock(t *testing.T) {
	for i := 0; i < 5; i++ {
		clientWriteDeadlock(t, i, func(f *File) {
			b := make([]byte, 32768*4)

			_, err := f.Write(b)
			if !errors.Is(err, errFakeNet) {
				t.Fatal("Didn't receive correct error:", err)
			}
		})
	}
}

type timeBombWriter struct {
	count int
	w     io.WriteCloser
}

func (w *timeBombWriter) Write(b []byte) (int, error) {
	if w.count < 1 {
		return 0, errFakeNet
	}

	w.count--
	return w.w.Write(b)
}

func (w *timeBombWriter) Close() error {
	return w.w.Close()
}

// shared body for both previous tests
func clientWriteDeadlock(t *testing.T, N int, badfunc func(*File)) {
	fsApi := apis.NewAVFS()
	if !*testServerImpl {
		t.Skipf("skipping without -testserver")
	}
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-writedeadlock")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	f := path.Join(d, "writeTest")
	w, err := sftp.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Override the clienConn Writer with a failing version
	// Replicates network error/drop part way through (after N good writes)
	wrap := sftp.clientConn.conn.WriteCloser
	sftp.clientConn.conn.WriteCloser = &timeBombWriter{
		count: N,
		w:     wrap,
	}

	// this locked (before the fix)
	badfunc(w)
}

// Read/WriteTo has this issue as well
func TestClientReadDeadlock(t *testing.T) {
	for i := 0; i < 3; i++ {
		clientReadDeadlock(t, i, func(f *File) {
			b := make([]byte, 32768*4)

			_, err := f.Read(b)
			if !errors.Is(err, errFakeNet) {
				t.Fatal("Didn't receive correct error:", err)
			}
		})
	}
}

func TestClientWriteToDeadlock(t *testing.T) {
	for i := 0; i < 3; i++ {
		clientReadDeadlock(t, i, func(f *File) {
			b := make([]byte, 32768*4)

			buf := bytes.NewBuffer(b)

			_, err := f.WriteTo(buf)
			if !errors.Is(err, errFakeNet) {
				t.Fatal("Didn't receive correct error:", err)
			}
		})
	}
}

func clientReadDeadlock(t *testing.T, N int, badfunc func(*File)) {
	fsApi := apis.NewAVFS()
	if !*testServerImpl {
		t.Skipf("skipping without -testserver")
	}
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest-readdeadlock")
	if err != nil {
		t.Fatal(err)
	}
	defer fsApi.RemoveAll(d)

	f := path.Join(d, "writeTest")

	w, err := sftp.Create(f)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// write the data for the read tests
	b := make([]byte, 32768*4)
	w.Write(b)

	// open new copy of file for read tests
	r, err := sftp.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Override the clienConn Writer with a failing version
	// Replicates network error/drop part way through (after N good writes)
	wrap := sftp.clientConn.conn.WriteCloser
	sftp.clientConn.conn.WriteCloser = &timeBombWriter{
		count: N,
		w:     wrap,
	}

	// this locked (before the fix)
	badfunc(r)
}

func TestClientSyncGo(t *testing.T) {
	if !*testServerImpl {
		t.Skipf("skipping without -testserver")
	}
	err := testClientSync(t)

	// Since Server does not support the fsync extension, we can only
	// check that we get the right error.
	require.Error(t, err)

	switch err := err.(type) {
	case *StatusError:
		assert.Equal(t, ErrSSHFxOpUnsupported, err.FxCode())
	default:
		t.Error(err)
	}
}

func TestClientSyncSFTP(t *testing.T) {
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}
	err := testClientSync(t)
	assert.NoError(t, err)
}

func testClientSync(t *testing.T) error {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	d, err := ioutil.TempDir("", "sftptest.sync")
	require.NoError(t, err)
	defer fsApi.RemoveAll(d)

	f := path.Join(d, "syncTest")
	w, err := sftp.Create(f)
	require.NoError(t, err)
	defer w.Close()

	return w.Sync()
}

// taken from github.com/kr/fs/walk_test.go

type Node struct {
	name    string
	entries []*Node // nil if the entry is a file
	mark    int
}

var tree = &Node{
	"testdata",
	[]*Node{
		{"a", nil, 0},
		{"b", []*Node{}, 0},
		{"c", nil, 0},
		{
			"d",
			[]*Node{
				{"x", nil, 0},
				{"y", []*Node{}, 0},
				{
					"z",
					[]*Node{
						{"u", nil, 0},
						{"v", nil, 0},
					},
					0,
				},
			},
			0,
		},
	},
	0,
}

func walkTree(n *Node, path string, f func(path string, n *Node)) {
	f(path, n)
	for _, e := range n.entries {
		walkTree(e, filepath.Join(path, e.name), f)
	}
}

func makeTree(t *testing.T) {
	fsApi := apis.NewAVFS()
	walkTree(tree, tree.name, func(path string, n *Node) {
		if n.entries == nil {
			fd, err := fsApi.Create(path)
			if err != nil {
				t.Errorf("makeTree: %v", err)
				return
			}
			fd.Close()
		} else {
			fsApi.Mkdir(path, 0770)
		}
	})
}

func markTree(n *Node) { walkTree(n, "", func(path string, n *Node) { n.mark++ }) }

func checkMarks(t *testing.T, report bool) {
	walkTree(tree, tree.name, func(path string, n *Node) {
		if n.mark != 1 && report {
			t.Errorf("node %s mark = %d; expected 1", path, n.mark)
		}
		n.mark = 0
	})
}

// Assumes that each node name is unique. Good enough for a test.
// If clear is true, any incoming error is cleared before return. The errors
// are always accumulated, though.
func mark(path string, info os.FileInfo, err error, errors *[]error, clear bool) error {
	if err != nil {
		*errors = append(*errors, err)
		if clear {
			return nil
		}
		return err
	}
	name := info.Name()
	walkTree(tree, tree.name, func(path string, n *Node) {
		if n.name == name {
			n.mark++
		}
	})
	return nil
}

func TestClientWalk(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	fsApi := apis.NewAVFS()
	defer cmd.Wait()
	defer sftp.Close()

	makeTree(t)
	errors := make([]error, 0, 10)
	clear := true
	markFn := func(walker *fs.Walker) error {
		for walker.Step() {
			err := mark(walker.Path(), walker.Stat(), walker.Err(), &errors, clear)
			if err != nil {
				return err
			}
		}
		return nil
	}
	// Expect no errors.
	err := markFn(sftp.Walk(tree.name))
	if err != nil {
		t.Fatalf("no error expected, found: %s", err)
	}
	if len(errors) != 0 {
		t.Fatalf("unexpected errors: %s", errors)
	}
	checkMarks(t, true)
	errors = errors[0:0]

	// Test permission errors.  Only possible if we're not root
	// and only on some file systems (AFS, FAT).  To avoid errors during
	// all.bash on those file systems, skip during go test -short.
	if os.Getuid() > 0 && !testing.Short() {
		// introduce 2 errors: chmod top-level directories to 0
		fsApi.Chmod(filepath.Join(tree.name, tree.entries[1].name), 0)
		fsApi.Chmod(filepath.Join(tree.name, tree.entries[3].name), 0)

		// 3) capture errors, expect two.
		// mark respective subtrees manually
		markTree(tree.entries[1])
		markTree(tree.entries[3])
		// correct double-marking of directory itself
		tree.entries[1].mark--
		tree.entries[3].mark--
		err := markFn(sftp.Walk(tree.name))
		if err != nil {
			t.Fatalf("expected no error return from Walk, got %s", err)
		}
		if len(errors) != 2 {
			t.Errorf("expected 2 errors, got %d: %s", len(errors), errors)
		}
		// the inaccessible subtrees were marked manually
		checkMarks(t, true)
		errors = errors[0:0]

		// 4) capture errors, stop after first error.
		// mark respective subtrees manually
		markTree(tree.entries[1])
		markTree(tree.entries[3])
		// correct double-marking of directory itself
		tree.entries[1].mark--
		tree.entries[3].mark--
		clear = false // error will stop processing
		err = markFn(sftp.Walk(tree.name))
		if err == nil {
			t.Fatalf("expected error return from Walk")
		}
		if len(errors) != 1 {
			t.Errorf("expected 1 error, got %d: %s", len(errors), errors)
		}
		// the inaccessible subtrees were marked manually
		checkMarks(t, false)
		errors = errors[0:0]

		// restore permissions
		fsApi.Chmod(filepath.Join(tree.name, tree.entries[1].name), 0770)
		fsApi.Chmod(filepath.Join(tree.name, tree.entries[3].name), 0770)
	}

	// cleanup
	if err := fsApi.RemoveAll(tree.name); err != nil {
		t.Errorf("removeTree: %v", err)
	}
}

type MatchTest struct {
	pattern, s string
	match      bool
	err        error
}

var matchTests = []MatchTest{
	{"abc", "abc", true, nil},
	{"*", "abc", true, nil},
	{"*c", "abc", true, nil},
	{"a*", "a", true, nil},
	{"a*", "abc", true, nil},
	{"a*", "ab/c", false, nil},
	{"a*/b", "abc/b", true, nil},
	{"a*/b", "a/c/b", false, nil},
	{"a*b*c*d*e*/f", "axbxcxdxe/f", true, nil},
	{"a*b*c*d*e*/f", "axbxcxdxexxx/f", true, nil},
	{"a*b*c*d*e*/f", "axbxcxdxe/xxx/f", false, nil},
	{"a*b*c*d*e*/f", "axbxcxdxexxx/fff", false, nil},
	{"a*b?c*x", "abxbbxdbxebxczzx", true, nil},
	{"a*b?c*x", "abxbbxdbxebxczzy", false, nil},
	{"ab[c]", "abc", true, nil},
	{"ab[b-d]", "abc", true, nil},
	{"ab[e-g]", "abc", false, nil},
	{"ab[^c]", "abc", false, nil},
	{"ab[^b-d]", "abc", false, nil},
	{"ab[^e-g]", "abc", true, nil},
	{"a\\*b", "a*b", true, nil},
	{"a\\*b", "ab", false, nil},
	{"a?b", "a☺b", true, nil},
	{"a[^a]b", "a☺b", true, nil},
	{"a???b", "a☺b", false, nil},
	{"a[^a][^a][^a]b", "a☺b", false, nil},
	{"[a-ζ]*", "α", true, nil},
	{"*[a-ζ]", "A", false, nil},
	{"a?b", "a/b", false, nil},
	{"a*b", "a/b", false, nil},
	{"[\\]a]", "]", true, nil},
	{"[\\-]", "-", true, nil},
	{"[x\\-]", "x", true, nil},
	{"[x\\-]", "-", true, nil},
	{"[x\\-]", "z", false, nil},
	{"[\\-x]", "x", true, nil},
	{"[\\-x]", "-", true, nil},
	{"[\\-x]", "a", false, nil},
	{"[]a]", "]", false, ErrBadPattern},
	{"[-]", "-", false, ErrBadPattern},
	{"[x-]", "x", false, ErrBadPattern},
	{"[x-]", "-", false, ErrBadPattern},
	{"[x-]", "z", false, ErrBadPattern},
	{"[-x]", "x", false, ErrBadPattern},
	{"[-x]", "-", false, ErrBadPattern},
	{"[-x]", "a", false, ErrBadPattern},
	{"\\", "a", false, ErrBadPattern},
	{"[a-b-c]", "a", false, ErrBadPattern},
	{"[", "a", false, ErrBadPattern},
	{"[^", "a", false, ErrBadPattern},
	{"[^bc", "a", false, ErrBadPattern},
	{"a[", "ab", false, ErrBadPattern},
	{"*x", "xxx", true, nil},

	// The following test behaves differently on Go 1.15.3 and Go tip as
	// https://github.com/golang/go/commit/b5ddc42b465dd5b9532ee336d98343d81a6d35b2
	// (pre-Go 1.16). TODO: reevaluate when Go 1.16 is released.
	//{"a[", "a", false, nil},
}

func errp(e error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Error()
}

// contains returns true if vector contains the string s.
func contains(vector []string, s string) bool {
	for _, elem := range vector {
		if elem == s {
			return true
		}
	}
	return false
}

var globTests = []struct {
	pattern, result string
}{
	{"match.go", "match.go"},
	{"mat?h.go", "match.go"},
	{"ma*ch.go", "match.go"},
	{`\m\a\t\c\h\.\g\o`, "match.go"},
	{"../*/match.go", "../sftp/match.go"},
}

type globTest struct {
	pattern string
	matches []string
}

func (test *globTest) buildWant(root string) []string {
	var want []string
	for _, m := range test.matches {
		want = append(want, root+filepath.FromSlash(m))
	}
	sort.Strings(want)
	return want
}

func TestMatch(t *testing.T) {
	for _, tt := range matchTests {
		pattern := tt.pattern
		s := tt.s
		ok, err := Match(pattern, s)
		if ok != tt.match || err != tt.err {
			t.Errorf("Match(%#q, %#q) = %v, %q want %v, %q", pattern, s, ok, errp(err), tt.match, errp(tt.err))
		}
	}
}

func TestGlob(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	for _, tt := range globTests {
		pattern := tt.pattern
		result := tt.result
		matches, err := sftp.Glob(pattern)
		if err != nil {
			t.Errorf("Glob error for %q: %s", pattern, err)
			continue
		}
		if !contains(matches, result) {
			t.Errorf("Glob(%#q) = %#v want %v", pattern, matches, result)
		}
	}
	for _, pattern := range []string{"no_match", "../*/no_match"} {
		matches, err := sftp.Glob(pattern)
		if err != nil {
			t.Errorf("Glob error for %q: %s", pattern, err)
			continue
		}
		if len(matches) != 0 {
			t.Errorf("Glob(%#q) = %#v want []", pattern, matches)
		}
	}
}

func TestGlobError(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	_, err := sftp.Glob("[7]")
	if err != nil {
		t.Error("expected error for bad pattern; got none")
	}
}

func TestGlobUNC(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()
	// Just make sure this runs without crashing for now.
	// See issue 15879.
	sftp.Glob(`\\?\C:\*`)
}

// sftp/issue/42, abrupt server hangup would result in client hangs.
func TestServerRoughDisconnect(t *testing.T) {
	skipIfWindows(t)
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := sftp.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cmd.Process.Kill()
	}()

	_, err = io.Copy(ioutil.Discard, f)
	assert.Error(t, err)
}

// sftp/issue/181, abrupt server hangup would result in client hangs.
// due to broadcastErr filling up the request channel
// this reproduces it about 50% of the time
func TestServerRoughDisconnect2(t *testing.T) {
	skipIfWindows(t)
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := sftp.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 32768*100)
	go func() {
		time.Sleep(1 * time.Millisecond)
		cmd.Process.Kill()
	}()
	for {
		_, err = f.Read(b)
		if err != nil {
			break
		}
	}
}

// sftp/issue/234 - abrupt shutdown during ReadFrom hangs client
func TestServerRoughDisconnect3(t *testing.T) {
	skipIfWindows(t)
	fsApi := apis.NewAVFS()
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}

	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	dest, err := sftp.OpenFile("/dev/null", syscall.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()

	src, err := fsApi.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cmd.Process.Kill()
	}()

	_, err = io.Copy(dest, src)
	assert.Error(t, err)
}

// sftp/issue/234 - also affected Write
func TestServerRoughDisconnect4(t *testing.T) {
	skipIfWindows(t)
	fsApi := apis.NewAVFS()
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	dest, err := sftp.OpenFile("/dev/null", syscall.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()

	src, err := fsApi.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cmd.Process.Kill()
	}()

	b := make([]byte, 32768*200)
	src.Read(b)
	for {
		_, err = dest.Write(b)
		if err != nil {
			assert.NotEqual(t, io.EOF, err)
			break
		}
	}

	_, err = io.Copy(dest, src)
	assert.Error(t, err)
}

// sftp/issue/390 - server disconnect should not cause io.EOF or
// io.ErrUnexpectedEOF in sftp.File.Read, because those confuse io.ReadFull.
func TestServerRoughDisconnectEOF(t *testing.T) {
	skipIfWindows(t)
	if *testServerImpl {
		t.Skipf("skipping with -testserver")
	}
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := sftp.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cmd.Process.Kill()
	}()

	_, err = io.ReadFull(f, make([]byte, 10))
	assert.Error(t, err)
	assert.NotEqual(t, io.ErrUnexpectedEOF, err)
}

// sftp/issue/26 writing to a read only file caused client to loop.
func TestClientWriteToROFile(t *testing.T) {
	skipIfWindows(t)
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer func() {
		err := sftp.Close()
		assert.NoError(t, err)
	}()

	f, err := sftp.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = f.Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected error, got", err)
	}
}

func benchmarkRead(b *testing.B, bufsize int, delay time.Duration) {
	skipIfWindows(b)
	size := 10*1024*1024 + 123 // ~10MiB

	// open sftp client
	sftp, cmd := testClient(b, READONLY, delay)
	defer cmd.Wait()
	defer sftp.Close()

	buf := make([]byte, bufsize)

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		offset := 0

		f2, err := sftp.Open("/dev/zero")
		if err != nil {
			b.Fatal(err)
		}

		for offset < size {
			n, err := io.ReadFull(f2, buf)
			offset += n
			if err == io.ErrUnexpectedEOF && offset != size {
				b.Fatalf("read too few bytes! want: %d, got: %d", size, n)
			}

			if err != nil {
				b.Fatal(err)
			}

			offset += n
		}

		f2.Close()
	}
}

func BenchmarkRead1k(b *testing.B) {
	benchmarkRead(b, 1*1024, NODELAY)
}

func BenchmarkRead16k(b *testing.B) {
	benchmarkRead(b, 16*1024, NODELAY)
}

func BenchmarkRead32k(b *testing.B) {
	benchmarkRead(b, 32*1024, NODELAY)
}

func BenchmarkRead128k(b *testing.B) {
	benchmarkRead(b, 128*1024, NODELAY)
}

func BenchmarkRead512k(b *testing.B) {
	benchmarkRead(b, 512*1024, NODELAY)
}

func BenchmarkRead1MiB(b *testing.B) {
	benchmarkRead(b, 1024*1024, NODELAY)
}

func BenchmarkRead4MiB(b *testing.B) {
	benchmarkRead(b, 4*1024*1024, NODELAY)
}

func BenchmarkRead4MiBDelay10Msec(b *testing.B) {
	benchmarkRead(b, 4*1024*1024, 10*time.Millisecond)
}

func BenchmarkRead4MiBDelay50Msec(b *testing.B) {
	benchmarkRead(b, 4*1024*1024, 50*time.Millisecond)
}

func BenchmarkRead4MiBDelay150Msec(b *testing.B) {
	benchmarkRead(b, 4*1024*1024, 150*time.Millisecond)
}

func benchmarkWrite(b *testing.B, bufsize int, delay time.Duration) {
	size := 10*1024*1024 + 123 // ~10MiB
	fsApi := apis.NewAVFS()

	// open sftp client
	sftp, cmd := testClient(b, false, delay)
	defer cmd.Wait()
	defer sftp.Close()

	data := make([]byte, size)

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		offset := 0

		f, err := ioutil.TempFile("", "sftptest-benchwrite")
		if err != nil {
			b.Fatal(err)
		}
		defer fsApi.Remove(f.Name()) // actually queue up a series of removes for these files

		f2, err := sftp.Create(f.Name())
		if err != nil {
			b.Fatal(err)
		}

		for offset < size {
			buf := data[offset:]
			if len(buf) > bufsize {
				buf = buf[:bufsize]
			}

			n, err := f2.Write(buf)
			if err != nil {
				b.Fatal(err)
			}

			if offset+n < size && n != bufsize {
				b.Fatalf("wrote too few bytes! want: %d, got: %d", size, n)
			}

			offset += n
		}

		f2.Close()

		fi, err := fsApi.Stat(f.Name())
		if err != nil {
			b.Fatal(err)
		}

		if fi.Size() != int64(size) {
			b.Fatalf("wrong file size: want %d, got %d", size, fi.Size())
		}

		fsApi.Remove(f.Name())
	}
}

func BenchmarkWrite1k(b *testing.B) {
	benchmarkWrite(b, 1*1024, NODELAY)
}

func BenchmarkWrite16k(b *testing.B) {
	benchmarkWrite(b, 16*1024, NODELAY)
}

func BenchmarkWrite32k(b *testing.B) {
	benchmarkWrite(b, 32*1024, NODELAY)
}

func BenchmarkWrite128k(b *testing.B) {
	benchmarkWrite(b, 128*1024, NODELAY)
}

func BenchmarkWrite512k(b *testing.B) {
	benchmarkWrite(b, 512*1024, NODELAY)
}

func BenchmarkWrite1MiB(b *testing.B) {
	benchmarkWrite(b, 1024*1024, NODELAY)
}

func BenchmarkWrite4MiB(b *testing.B) {
	benchmarkWrite(b, 4*1024*1024, NODELAY)
}

func BenchmarkWrite4MiBDelay10Msec(b *testing.B) {
	benchmarkWrite(b, 4*1024*1024, 10*time.Millisecond)
}

func BenchmarkWrite4MiBDelay50Msec(b *testing.B) {
	benchmarkWrite(b, 4*1024*1024, 50*time.Millisecond)
}

func BenchmarkWrite4MiBDelay150Msec(b *testing.B) {
	benchmarkWrite(b, 4*1024*1024, 150*time.Millisecond)
}

func benchmarkReadFrom(b *testing.B, bufsize int, delay time.Duration) {
	size := 10*1024*1024 + 123 // ~10MiB
	fsApi := apis.NewAVFS()

	// open sftp client
	sftp, cmd := testClient(b, false, delay)
	defer cmd.Wait()
	defer sftp.Close()

	data := make([]byte, size)

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		f, err := ioutil.TempFile("", "sftptest-benchreadfrom")
		if err != nil {
			b.Fatal(err)
		}
		defer fsApi.Remove(f.Name())

		f2, err := sftp.Create(f.Name())
		if err != nil {
			b.Fatal(err)
		}
		defer f2.Close()

		f2.ReadFrom(bytes.NewReader(data))
		f2.Close()

		fi, err := fsApi.Stat(f.Name())
		if err != nil {
			b.Fatal(err)
		}

		if fi.Size() != int64(size) {
			b.Fatalf("wrong file size: want %d, got %d", size, fi.Size())
		}

		fsApi.Remove(f.Name())
	}
}

func BenchmarkReadFrom1k(b *testing.B) {
	benchmarkReadFrom(b, 1*1024, NODELAY)
}

func BenchmarkReadFrom16k(b *testing.B) {
	benchmarkReadFrom(b, 16*1024, NODELAY)
}

func BenchmarkReadFrom32k(b *testing.B) {
	benchmarkReadFrom(b, 32*1024, NODELAY)
}

func BenchmarkReadFrom128k(b *testing.B) {
	benchmarkReadFrom(b, 128*1024, NODELAY)
}

func BenchmarkReadFrom512k(b *testing.B) {
	benchmarkReadFrom(b, 512*1024, NODELAY)
}

func BenchmarkReadFrom1MiB(b *testing.B) {
	benchmarkReadFrom(b, 1024*1024, NODELAY)
}

func BenchmarkReadFrom4MiB(b *testing.B) {
	benchmarkReadFrom(b, 4*1024*1024, NODELAY)
}

func BenchmarkReadFrom4MiBDelay10Msec(b *testing.B) {
	benchmarkReadFrom(b, 4*1024*1024, 10*time.Millisecond)
}

func BenchmarkReadFrom4MiBDelay50Msec(b *testing.B) {
	benchmarkReadFrom(b, 4*1024*1024, 50*time.Millisecond)
}

func BenchmarkReadFrom4MiBDelay150Msec(b *testing.B) {
	benchmarkReadFrom(b, 4*1024*1024, 150*time.Millisecond)
}

func benchmarkWriteTo(b *testing.B, bufsize int, delay time.Duration) {
	size := 10*1024*1024 + 123 // ~10MiB
	fsApi := apis.NewAVFS()

	// open sftp client
	sftp, cmd := testClient(b, false, delay)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-benchwriteto")
	if err != nil {
		b.Fatal(err)
	}
	defer fsApi.Remove(f.Name())

	data := make([]byte, size)

	f.Write(data)
	f.Close()

	buf := bytes.NewBuffer(make([]byte, 0, size))

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		buf.Reset()

		f2, err := sftp.Open(f.Name())
		if err != nil {
			b.Fatal(err)
		}

		f2.WriteTo(buf)
		f2.Close()

		if buf.Len() != size {
			b.Fatalf("wrote buffer size: want %d, got %d", size, buf.Len())
		}
	}
}

func BenchmarkWriteTo1k(b *testing.B) {
	benchmarkWriteTo(b, 1*1024, NODELAY)
}

func BenchmarkWriteTo16k(b *testing.B) {
	benchmarkWriteTo(b, 16*1024, NODELAY)
}

func BenchmarkWriteTo32k(b *testing.B) {
	benchmarkWriteTo(b, 32*1024, NODELAY)
}

func BenchmarkWriteTo128k(b *testing.B) {
	benchmarkWriteTo(b, 128*1024, NODELAY)
}

func BenchmarkWriteTo512k(b *testing.B) {
	benchmarkWriteTo(b, 512*1024, NODELAY)
}

func BenchmarkWriteTo1MiB(b *testing.B) {
	benchmarkWriteTo(b, 1024*1024, NODELAY)
}

func BenchmarkWriteTo4MiB(b *testing.B) {
	benchmarkWriteTo(b, 4*1024*1024, NODELAY)
}

func BenchmarkWriteTo4MiBDelay10Msec(b *testing.B) {
	benchmarkWriteTo(b, 4*1024*1024, 10*time.Millisecond)
}

func BenchmarkWriteTo4MiBDelay50Msec(b *testing.B) {
	benchmarkWriteTo(b, 4*1024*1024, 50*time.Millisecond)
}

func BenchmarkWriteTo4MiBDelay150Msec(b *testing.B) {
	benchmarkWriteTo(b, 4*1024*1024, 150*time.Millisecond)
}

func benchmarkCopyDown(b *testing.B, fileSize int64, delay time.Duration) {
	skipIfWindows(b)
	fsApi := apis.NewAVFS()
	// Create a temp file and fill it with zero's.
	src, err := ioutil.TempFile("", "sftptest-benchcopydown")
	if err != nil {
		b.Fatal(err)
	}
	defer src.Close()
	srcFilename := src.Name()
	defer fsApi.Remove(srcFilename)
	zero, err := fsApi.Open("/dev/zero")
	if err != nil {
		b.Fatal(err)
	}
	n, err := io.Copy(src, io.LimitReader(zero, fileSize))
	if err != nil {
		b.Fatal(err)
	}
	if n < fileSize {
		b.Fatal("short copy")
	}
	zero.Close()
	src.Close()

	sftp, cmd := testClient(b, READONLY, delay)
	defer cmd.Wait()
	defer sftp.Close()
	b.ResetTimer()
	b.SetBytes(fileSize)

	for i := 0; i < b.N; i++ {
		dst, err := ioutil.TempFile("", "sftptest-benchcopydown")
		if err != nil {
			b.Fatal(err)
		}
		defer fsApi.Remove(dst.Name())

		src, err := sftp.Open(srcFilename)
		if err != nil {
			b.Fatal(err)
		}
		defer src.Close()
		n, err := io.Copy(dst, src)
		if err != nil {
			b.Fatalf("copy error: %v", err)
		}
		if n < fileSize {
			b.Fatal("unable to copy all bytes")
		}
		dst.Close()
		fi, err := fsApi.Stat(dst.Name())
		if err != nil {
			b.Fatal(err)
		}

		if fi.Size() != fileSize {
			b.Fatalf("wrong file size: want %d, got %d", fileSize, fi.Size())
		}
		fsApi.Remove(dst.Name())
	}
}

func BenchmarkCopyDown10MiBDelay10Msec(b *testing.B) {
	benchmarkCopyDown(b, 10*1024*1024, 10*time.Millisecond)
}

func BenchmarkCopyDown10MiBDelay50Msec(b *testing.B) {
	benchmarkCopyDown(b, 10*1024*1024, 50*time.Millisecond)
}

func BenchmarkCopyDown10MiBDelay150Msec(b *testing.B) {
	benchmarkCopyDown(b, 10*1024*1024, 150*time.Millisecond)
}

func benchmarkCopyUp(b *testing.B, fileSize int64, delay time.Duration) {
	skipIfWindows(b)
	fsApi := apis.NewAVFS()
	// Create a temp file and fill it with zero's.
	src, err := ioutil.TempFile("", "sftptest-benchcopyup")
	if err != nil {
		b.Fatal(err)
	}
	defer src.Close()
	srcFilename := src.Name()
	defer fsApi.Remove(srcFilename)
	zero, err := fsApi.Open("/dev/zero")
	if err != nil {
		b.Fatal(err)
	}
	n, err := io.Copy(src, io.LimitReader(zero, fileSize))
	if err != nil {
		b.Fatal(err)
	}
	if n < fileSize {
		b.Fatal("short copy")
	}
	zero.Close()
	src.Close()

	sftp, cmd := testClient(b, false, delay)
	defer cmd.Wait()
	defer sftp.Close()

	b.ResetTimer()
	b.SetBytes(fileSize)

	for i := 0; i < b.N; i++ {
		tmp, err := ioutil.TempFile("", "sftptest-benchcopyup")
		if err != nil {
			b.Fatal(err)
		}
		tmp.Close()
		defer fsApi.Remove(tmp.Name())

		dst, err := sftp.Create(tmp.Name())
		if err != nil {
			b.Fatal(err)
		}
		defer dst.Close()
		src, err := fsApi.Open(srcFilename)
		if err != nil {
			b.Fatal(err)
		}
		defer src.Close()
		n, err := io.Copy(dst, src)
		if err != nil {
			b.Fatalf("copy error: %v", err)
		}
		if n < fileSize {
			b.Fatal("unable to copy all bytes")
		}

		fi, err := fsApi.Stat(tmp.Name())
		if err != nil {
			b.Fatal(err)
		}

		if fi.Size() != fileSize {
			b.Fatalf("wrong file size: want %d, got %d", fileSize, fi.Size())
		}
		fsApi.Remove(tmp.Name())
	}
}

func BenchmarkCopyUp10MiBDelay10Msec(b *testing.B) {
	benchmarkCopyUp(b, 10*1024*1024, 10*time.Millisecond)
}

func BenchmarkCopyUp10MiBDelay50Msec(b *testing.B) {
	benchmarkCopyUp(b, 10*1024*1024, 50*time.Millisecond)
}

func BenchmarkCopyUp10MiBDelay150Msec(b *testing.B) {
	benchmarkCopyUp(b, 10*1024*1024, 150*time.Millisecond)
}
