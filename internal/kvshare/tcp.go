package kvshare

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

func init() { Register("tcp", func() Transport { return tcpTransport{} }) }

// tcpTransport: one connection per call, large socket buffers, no framing on
// the delta itself: the client half-closes when the body is done.
//
//	request:  "KVT1" | op u8 (1 manifest, 2 put) | key len u16 | key | body (put)
//	response: status u8 (0 ok, 1 error) | json len u32 | json
const (
	tcpMagic    = "KVT1"
	opManifest  = 1
	opPut       = 2
	sockBuffer  = 8 << 20
	maxRespJSON = 64 << 20
)

type tcpTransport struct{}

func (tcpTransport) Name() string { return "tcp" }

type tcpServer struct {
	ln net.Listener
}

func (s *tcpServer) Addr() string { return s.ln.Addr().String() }
func (s *tcpServer) Close() error { return s.ln.Close() }

func (tcpTransport) Listen(addr string, h Handler) (Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTCP(c, h)
		}
	}()
	return &tcpServer{ln}, nil
}

func tune(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(sockBuffer)
		_ = tc.SetWriteBuffer(sockBuffer)
		_ = tc.SetNoDelay(true)
	}
}

func serveTCP(c net.Conn, h Handler) {
	defer c.Close()
	tune(c)
	br := bufio.NewReaderSize(c, 4<<20)
	var hdr [7]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil || string(hdr[:4]) != tcpMagic {
		return
	}
	key := make([]byte, binary.LittleEndian.Uint16(hdr[5:]))
	if _, err := io.ReadFull(br, key); err != nil {
		return
	}
	ctx := context.Background()
	var (
		v   any
		err error
	)
	switch {
	case !ValidKey(string(key)):
		err = fmt.Errorf("kvshare: invalid key %q", key)
	case hdr[4] == opManifest:
		v, err = h.Manifest(ctx, string(key))
	case hdr[4] == opPut:
		v, err = h.Put(ctx, string(key), br)
	default:
		err = fmt.Errorf("kvshare: unknown op %d", hdr[4])
	}
	writeTCPResponse(c, v, err)
}

func writeTCPResponse(c net.Conn, v any, err error) {
	status := byte(0)
	if err != nil {
		status, v = 1, err.Error()
	}
	body, _ := json.Marshal(v)
	out := make([]byte, 5, 5+len(body))
	out[0] = status
	binary.LittleEndian.PutUint32(out[1:], uint32(len(body)))
	_, _ = c.Write(append(out, body...))
}

func (tcpTransport) call(ctx context.Context, addr, key string, op byte, body io.Reader, out any) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	tune(c)
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	hdr := make([]byte, 7, 7+len(key))
	copy(hdr, tcpMagic)
	hdr[4] = op
	binary.LittleEndian.PutUint16(hdr[5:], uint16(len(key)))
	if _, err := c.Write(append(hdr, key...)); err != nil {
		return err
	}
	if body != nil {
		if _, err := io.Copy(c, body); err != nil {
			return err
		}
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	var rh [5]byte
	if _, err := io.ReadFull(c, rh[:]); err != nil {
		return fmt.Errorf("kvshare: reading the answer: %w", err)
	}
	n := binary.LittleEndian.Uint32(rh[1:])
	if n > maxRespJSON {
		return fmt.Errorf("kvshare: answer of %d bytes", n)
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(c, js); err != nil {
		return err
	}
	if rh[0] != 0 {
		var msg string
		_ = json.Unmarshal(js, &msg)
		return errors.New(msg)
	}
	return json.Unmarshal(js, out)
}

func (t tcpTransport) Manifest(ctx context.Context, addr, key string) (*Manifest, error) {
	var m *Manifest
	err := t.call(ctx, addr, key, opManifest, nil, &m)
	return m, err
}

func (t tcpTransport) Put(ctx context.Context, addr, key string, delta io.Reader) (PutResult, error) {
	var r PutResult
	err := t.call(ctx, addr, key, opPut, delta, &r)
	return r, err
}
