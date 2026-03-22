package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA

	// Massive buffers — critical for media throughput.
	// Telegram sends video chunks in rapid succession of WS frames.
	// Larger buffers = fewer syscalls = much faster media loading.
	readBufSize  = 512 * 1024       // 512 KB read buffer
	writeBufSize = 512 * 1024       // 512 KB write buffer
	maxFrameSize = 64 * 1024 * 1024 // 64 MB max frame
)

type HandshakeError struct {
	Code     int
	Status   string
	Location string
}

func (e *HandshakeError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Status) }
func (e *HandshakeError) IsRedirect() bool {
	return e.Code == 301 || e.Code == 302 || e.Code == 303 || e.Code == 307 || e.Code == 308
}

// Conn is a high-throughput raw WebSocket connection optimized for streaming.
type Conn struct {
	raw    net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	wmu    sync.Mutex // write mutex
	closed bool

	// Reusable mask scratch buffer to avoid allocs in hot path
	maskBuf [4]byte
	// Reusable header buffer
	hdrBuf [14]byte // max header: 1 + 1 + 8 + 4 = 14
}

// TLS session cache — avoids full TLS handshake on reconnects to same DC.
var tlsSessionCache = tls.NewLRUClientSessionCache(128)

var tlsCfg = &tls.Config{
	InsecureSkipVerify: true,
	ClientSessionCache: tlsSessionCache,
	MinVersion:         tls.VersionTLS12,
}

// TLSFragSize is the byte offset at which the first TLS write (ClientHello)
// is split into two TCP segments. This defeats DPI systems that inspect the
// SNI in the ClientHello to block Telegram. Set to 0 to disable.
var TLSFragSize int = 6

// fragmentConn wraps a net.Conn and splits the first Write into two TCP
// segments to bypass DPI inspection of TLS ClientHello.
type fragmentConn struct {
	net.Conn
	fragmented int32 // atomic: 0 = not yet, 1 = done
	splitAt    int
}

func (f *fragmentConn) Write(b []byte) (int, error) {
	if atomic.CompareAndSwapInt32(&f.fragmented, 0, 1) && len(b) > f.splitAt {
		// Send first fragment (cuts inside TLS record header, before SNI)
		n1, err := f.Conn.Write(b[:f.splitAt])
		if err != nil {
			return n1, err
		}
		// Tiny pause to force separate TCP segments
		time.Sleep(50 * time.Millisecond)
		// Send remainder
		n2, err := f.Conn.Write(b[f.splitAt:])
		return n1 + n2, err
	}
	return f.Conn.Write(b)
}

func Connect(ip, domain string, timeout time.Duration) (*Conn, error) {
	cfg := tlsCfg.Clone()
	cfg.ServerName = domain

	rawConn, err := (&net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}).Dial("tcp", ip+":443")
	if err != nil {
		return nil, err
	}

	// Aggressive TCP tuning for max throughput
	if tcp, ok := rawConn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)   // disable Nagle — send immediately
		tcp.SetKeepAlive(true) // detect dead connections
		tcp.SetKeepAlivePeriod(30 * time.Second)
		tcp.SetReadBuffer(1 << 20)  // 1 MB kernel recv buffer
		tcp.SetWriteBuffer(1 << 20) // 1 MB kernel send buffer
		tcp.SetLinger(0)            // RST on close — don't wait
	}

	// Wrap in fragmentConn to split TLS ClientHello for DPI bypass
	var tlsTransport net.Conn = rawConn
	if TLSFragSize > 0 {
		tlsTransport = &fragmentConn{Conn: rawConn, splitAt: TLSFragSize}
	}

	tlsConn := tls.Client(tlsTransport, cfg)
	tlsConn.SetDeadline(time.Now().Add(timeout))
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReaderSize(tlsConn, readBufSize)
	bw := bufio.NewWriterSize(tlsConn, writeBufSize)

	key := make([]byte, 16)
	rand.Read(key)

	fmt.Fprintf(bw,
		"GET /apiws HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\n"+
			"Sec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Protocol: binary\r\n"+
			"Origin: https://web.telegram.org\r\n"+
			"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36\r\n"+
			"\r\n",
		domain, base64.StdEncoding.EncodeToString(key))
	if err := bw.Flush(); err != nil {
		tlsConn.Close()
		return nil, err
	}

	statusLine, err := br.ReadString('\n')
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	code := 0
	if len(statusLine) >= 12 {
		fmt.Sscanf(statusLine[9:12], "%d", &code)
	}

	headers := make(map[string]string, 8)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		line = trimCRLF(line)
		if line == "" {
			break
		}
		if i := indexOf(line, ": "); i > 0 {
			headers[toLower(line[:i])] = line[i+2:]
		}
	}

	tlsConn.SetDeadline(time.Time{})

	if code == 101 {
		c := &Conn{raw: tlsConn, br: br, bw: bw}
		go c.keepalive()
		return c, nil
	}

	tlsConn.Close()
	return nil, &HandshakeError{Code: code, Status: trimCRLF(statusLine), Location: headers["location"]}
}

// Send writes one masked binary frame and flushes.
func (c *Conn) Send(data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	c.writeFrame(OpBinary, data)
	return c.bw.Flush()
}

// SendBatch writes multiple frames with ONE flush — critical for upload perf.
func (c *Conn) SendBatch(parts [][]byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	for _, p := range parts {
		c.writeFrame(OpBinary, p)
	}
	return c.bw.Flush()
}

// SendNoFlush writes a frame without flushing — caller must flush.
func (c *Conn) SendNoFlush(data []byte) {
	c.wmu.Lock()
	c.writeFrame(OpBinary, data)
	c.wmu.Unlock()
}

// Flush flushes the write buffer.
func (c *Conn) Flush() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.bw.Flush()
}

// Recv reads the next data frame. Handles control frames internally.
func (c *Conn) Recv() ([]byte, error) {
	for !c.closed {
		op, payload, err := c.readFrame()
		if err != nil {
			c.closed = true
			return nil, err
		}
		switch op {
		case OpClose:
			c.closed = true
			c.wmu.Lock()
			c.writeFrame(OpClose, payload)
			c.bw.Flush()
			c.wmu.Unlock()
			return nil, io.EOF
		case OpPing:
			c.wmu.Lock()
			c.writeFrame(OpPong, payload)
			c.bw.Flush()
			c.wmu.Unlock()
		case OpPong:
			continue
		case OpBinary, 0x1:
			return payload, nil
		}
	}
	return nil, io.EOF
}

func (c *Conn) Close() {
	if c.closed {
		return
	}
	c.closed = true
	c.wmu.Lock()
	c.writeFrame(OpClose, nil)
	c.bw.Flush()
	c.wmu.Unlock()
	c.raw.Close()
}

func (c *Conn) IsClosed() bool { return c.closed }

// keepalive sends WebSocket Ping frames every 25s to:
//  1. Prevent server-side idle connection timeouts (usually 30–60s)
//  2. Detect dead connections early — if Flush fails, marks conn as closed
//     so the pool's IsClosed() check discards it before handing it out.
func (c *Conn) keepalive() {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if c.closed {
			return
		}
		c.wmu.Lock()
		c.writeFrame(OpPing, nil)
		err := c.bw.Flush()
		c.wmu.Unlock()
		if err != nil {
			c.closed = true
			return
		}
	}
}

// writeFrame builds and writes a masked WS frame.
// Uses word-level XOR masking for 4-8x speedup over byte-level.
// Caller must hold c.wmu.
func (c *Conn) writeFrame(opcode byte, data []byte) {
	length := len(data)

	// Build header into reusable buffer
	hdr := c.hdrBuf[:0]
	hdr = append(hdr, 0x80|opcode) // FIN + opcode

	switch {
	case length < 126:
		hdr = append(hdr, 0x80|byte(length))
	case length < 65536:
		hdr = append(hdr, 0x80|126)
		hdr = append(hdr, byte(length>>8), byte(length))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, byte(length>>56), byte(length>>48),
			byte(length>>40), byte(length>>32),
			byte(length>>24), byte(length>>16),
			byte(length>>8), byte(length))
	}

	// Generate mask
	rand.Read(c.maskBuf[:])
	hdr = append(hdr, c.maskBuf[:]...)
	c.bw.Write(hdr)

	if length == 0 {
		return
	}

	// Fast XOR masking: process 8 bytes at a time using uint64
	mask32 := binary.LittleEndian.Uint32(c.maskBuf[:])
	mask64 := uint64(mask32) | uint64(mask32)<<32

	// For frames > 1KB, allocate masked buffer and do word-level XOR
	// For tiny frames, byte-level is fine (less alloc overhead)
	if length <= 1024 {
		// Small frame — just write byte-by-byte through bufio
		for i := 0; i < length; i++ {
			c.bw.WriteByte(data[i] ^ c.maskBuf[i&3])
		}
		return
	}

	// Allocate masked copy for large payloads
	masked := make([]byte, length)

	// Word-level XOR: 8 bytes at a time
	// Uses binary.LittleEndian for safe unaligned access on mipsle/arm32.
	i := 0
	n64 := length &^ 7 // round down to 8-byte boundary
	for i < n64 {
		v := binary.LittleEndian.Uint64(data[i:])
		binary.LittleEndian.PutUint64(masked[i:], v^mask64)
		i += 8
	}
	// Handle remaining bytes
	for i < length {
		masked[i] = data[i] ^ c.maskBuf[i&3]
		i++
	}
	c.bw.Write(masked)
}

// readFrame reads one WS frame from the buffered reader.
func (c *Conn) readFrame() (byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := hdr[0] & 0x0F
	length := uint64(hdr[1] & 0x7F)
	masked := hdr[1]&0x80 != 0

	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
	}

	if length > maxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			// Word-level unmask
			// Uses binary.LittleEndian for safe unaligned access on mipsle/arm32.
			mask32 := binary.LittleEndian.Uint32(mask[:])
			mask64 := uint64(mask32) | uint64(mask32)<<32
			i := 0
			n64 := int(length) &^ 7
			for i < n64 {
				v := binary.LittleEndian.Uint64(payload[i:])
				binary.LittleEndian.PutUint64(payload[i:], v^mask64)
				i += 8
			}
			for i < int(length) {
				payload[i] ^= mask[i&3]
				i++
			}
		}
	}
	return op, payload, nil
}

// Helpers
func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
