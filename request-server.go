package sftp

import (
	"encoding"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/pkg/errors"
)

var maxTxPacket uint32 = 1 << 15

type handleHandler func(string) string

// Handlers contains the 4 SFTP server request handlers.
type Handlers struct {
	FileGet  FileReader
	FilePut  FileWriter
	FileCmd  FileCmder
	FileList FileLister
}

// RequestServer abstracts the sftp protocol with an http request-like protocol
type RequestServer struct {
	*serverConn
	Handlers        Handlers
	pktMgr          *packetManager
	openRequests    map[string]Request
	openRequestLock sync.RWMutex
	handleCount     int
}

// NewRequestServer creates/allocates/returns new RequestServer.
// Normally there there will be one server per user-session.
func NewRequestServer(rwc io.ReadWriteCloser, h Handlers) *RequestServer {
	svrConn := &serverConn{
		conn: conn{
			Reader:      rwc,
			WriteCloser: rwc,
		},
	}
	return &RequestServer{
		serverConn:   svrConn,
		Handlers:     h,
		pktMgr:       newPktMgr(svrConn),
		openRequests: make(map[string]Request),
	}
}

// Note that we are explicitly saving the Request as a value.
func (rs *RequestServer) nextRequest(r *Request) string {
	rs.openRequestLock.Lock()
	defer rs.openRequestLock.Unlock()
	rs.handleCount++
	handle := strconv.Itoa(rs.handleCount)
	rs.openRequests[handle] = *r
	return handle
}

// Returns pointer to new copy of Request object
func (rs *RequestServer) getRequest(handle string) (*Request, bool) {
	rs.openRequestLock.RLock()
	defer rs.openRequestLock.RUnlock()
	r, ok := rs.openRequests[handle]
	return &r, ok
}

func (rs *RequestServer) closeRequest(handle string) {
	rs.openRequestLock.Lock()
	defer rs.openRequestLock.Unlock()
	if r, ok := rs.openRequests[handle]; ok {
		r.close()
		delete(rs.openRequests, handle)
	}
}

// Close the read/write/closer to trigger exiting the main server loop
func (rs *RequestServer) Close() error { return rs.conn.Close() }

// Serve requests for user session
func (rs *RequestServer) Serve() error {
	var wg sync.WaitGroup
	runWorker := func(ch requestChan) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rs.packetWorker(ch); err != nil {
				rs.conn.Close() // shuts down recvPacket
			}
		}()
	}
	pktChan := rs.pktMgr.workerChan(runWorker)

	var err error
	var pkt requestPacket
	var pktType uint8
	var pktBytes []byte
	for {
		pktType, pktBytes, err = rs.recvPacket()
		if err != nil {
			break
		}

		pkt, err = makePacket(rxPacket{fxp(pktType), pktBytes})
		if err != nil {
			debug("makePacket err: %v", err)
			rs.conn.Close() // shuts down recvPacket
			break
		}

		pktChan <- pkt
	}

	close(pktChan) // shuts down sftpServerWorkers
	wg.Wait()      // wait for all workers to exit

	return err
}

func (rs *RequestServer) packetWorker(pktChan chan requestPacket) error {
	for pkt := range pktChan {
		var rpkt responsePacket
		switch pkt := pkt.(type) {
		case *sshFxInitPacket:
			rpkt = sshFxVersionPacket{sftpProtocolVersion, nil}
		case *sshFxpClosePacket:
			handle := pkt.getHandle()
			rs.closeRequest(handle)
			rpkt = statusFromError(pkt, nil)
		case *sshFxpRealpathPacket:
			rpkt = cleanPacketPath(pkt)
		case isOpener:
			handle := rs.nextRequest(requestFromPacket(pkt))
			rpkt = sshFxpHandlePacket{pkt.id(), handle}
		case *sshFxpFstatPacket:
			handle := pkt.getHandle()
			request, ok := rs.getRequest(handle)
			if !ok {
				rpkt = statusFromError(pkt, syscall.EBADF)
			} else {
				request = requestFromPacket(
					&sshFxpStatPacket{ID: pkt.id(), Path: request.Filepath})
				rpkt = request.handle(rs.Handlers)
			}
		case *sshFxpFsetstatPacket:
			handle := pkt.getHandle()
			request, ok := rs.getRequest(handle)
			if !ok {
				rpkt = statusFromError(pkt, syscall.EBADF)
			} else {
				request = requestFromPacket(
					&sshFxpSetstatPacket{ID: pkt.id(), Path: request.Filepath,
						Flags: pkt.Flags, Attrs: pkt.Attrs,
					})
				rpkt = request.handle(rs.Handlers)
			}
		case hasHandle:
			handle := pkt.getHandle()
			request, ok := rs.getRequest(handle)
			request.update(pkt)
			if !ok {
				rpkt = statusFromError(pkt, syscall.EBADF)
			} else {
				rpkt = request.handle(rs.Handlers)
			}
		case hasPath:
			request := requestFromPacket(pkt)
			rpkt = request.handle(rs.Handlers)
		default:
			return errors.Errorf("unexpected packet type %T", pkt)
		}

		err := rs.sendPacket(rpkt)
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanPacketPath(pkt *sshFxpRealpathPacket) responsePacket {
	path := cleanPath(pkt.getPath())
	return &sshFxpNamePacket{
		ID: pkt.id(),
		NameAttrs: []sshFxpNameAttr{{
			Name:     path,
			LongName: path,
			Attrs:    emptyFileStat,
		}},
	}
}

func cleanPath(path string) string {
	cleanSlashPath := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(cleanSlashPath, "/") {
		return "/" + cleanSlashPath
	}
	return cleanSlashPath
}

// Wrap underlying connection methods to use packetManager
func (rs *RequestServer) sendPacket(m encoding.BinaryMarshaler) error {
	if pkt, ok := m.(responsePacket); ok {
		rs.pktMgr.readyPacket(pkt)
	} else {
		return errors.Errorf("unexpected packet type %T", m)
	}
	return nil
}

func (rs *RequestServer) sendError(p ider, err error) error {
	return rs.sendPacket(statusFromError(p, err))
}
