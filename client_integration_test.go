package sftp

// sftp integration tests
// enable with -integration

import (
	"errors"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"sync"
	"testing"
	"time"
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

	server, err := NewServer(c1, options...)
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
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-lstat")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	want, err := os.Lstat(f.Name())
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
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-lstatisnotexist")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Remove(f.Name())

	if _, err := sftp.Lstat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("os.IsNotExist(%v) = false, want true", err)
	}
}

func TestClientMkdir(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-mkdir")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	sub := path.Join(dir, "mkdir1")
	if err := sftp.Mkdir(sub); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sub); err != nil {
		t.Fatal(err)
	}
}
func TestClientMkdirAll(t *testing.T) {
	sftp, cmd := testClient(t, READWRITE, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	dir, err := ioutil.TempDir("", "sftptest-mkdirall")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	sub := path.Join(dir, "mkdir1", "mkdir2", "mkdir3")
	if err := sftp.MkdirAll(sub); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("Expected mkdirall to create dir at: %s", sub)
	}
}

func TestClientOpen(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	f, err := ioutil.TempFile("", "sftptest-open")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

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

	if _, err := sftp.Open("/doesnt/exist/"); !os.IsNotExist(err) {
		t.Errorf("os.IsNotExist(%v) = false, want true", err)
	}
}

func TestClientStatIsNotExist(t *testing.T) {
	sftp, cmd := testClient(t, READONLY, NODELAY)
	defer cmd.Wait()
	defer sftp.Close()

	if _, err := sftp.Stat("/doesnt/exist/"); !os.IsNotExist(err) {
		t.Errorf("os.IsNotExist(%v) = false, want true", err)
	}
}
