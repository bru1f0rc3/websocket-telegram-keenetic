package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tg-ws-proxy/internal/config"
	"tg-ws-proxy/internal/dns"
	"tg-ws-proxy/internal/telegram"
	"tg-ws-proxy/internal/ws"
)

// Stats tracks connection statistics atomically.
type Stats struct {
	Total, WS, TCPFallback, HTTPReject, Passthrough, WSErrors int64
	BytesUp, BytesDown                                        int64
}

func (s *Stats) Summary() string {
	return fmt.Sprintf("total=%d ws=%d tcp_fb=%d http_skip=%d pass=%d err=%d up=%s down=%s",
		atomic.LoadInt64(&s.Total), atomic.LoadInt64(&s.WS),
		atomic.LoadInt64(&s.TCPFallback), atomic.LoadInt64(&s.HTTPReject),
		atomic.LoadInt64(&s.Passthrough), atomic.LoadInt64(&s.WSErrors),
		telegram.HumanBytes(atomic.LoadInt64(&s.BytesUp)),
		telegram.HumanBytes(atomic.LoadInt64(&s.BytesDown)))
}

var stats Stats

// Blacklist + fail tracking
var (
	// wsBlacklist stores time.Time expiry — DC is skipped until that time.
	wsBlacklist  sync.Map
	dcFailUntil  sync.Map
	dcFailCount  sync.Map // consecutive WS failure count per DC key
	blacklistTTL = 3 * time.Minute
	// Exponential backoff intervals for repeated WS failures.
	// After DC1/DC3/DC5 fail 4 times they get a 10-min cooldown —
	// virtually eliminating the 10s×2 domain timeout storm.
	wsBackoffs = []time.Duration{
		20 * time.Second,
		45 * time.Second,
		2 * time.Minute,
		10 * time.Minute,
	}
)

// Server is the SOCKS5→WS proxy server.
type Server struct {
	cfg      config.Config
	dcOpt    map[int]string
	pool     *ws.Pool
	resolver *dns.Resolver
	ln       net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	bufSz    int
}

func New(cfg config.Config, dcOpt map[int]string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	// Auto-fill all 5 DC IPs so traffic to DC1/3/5 never falls back to TCP.
	fullDCOpt := telegram.AutoFillDCs(dcOpt)

	var resolver *dns.Resolver
	if urls := dns.URLsFor(cfg.DoH); len(urls) > 0 {
		if len(urls) == 1 {
			resolver = dns.New(urls[0])
			log.Printf("DNS-over-HTTPS: %s", urls[0])
		} else {
			resolver = dns.NewMulti(urls)
			log.Printf("DNS-over-HTTPS: %d providers (racing mode)", len(urls))
		}
	}

	// resolveFunc wraps the DoH resolver for use in the WS pool.
	// DC1/3/5 WebSocket servers live on different IPs than the main MTProto IPs,
	// so we resolve kws{N}.web.telegram.org to find the actual WS endpoint.
	var resolveFunc func(string) (string, error)
	if resolver != nil {
		resolveFunc = func(domain string) (string, error) {
			addrs, err := resolver.LookupHost(domain)
			if err != nil || len(addrs) == 0 {
				return "", fmt.Errorf("no addresses for %s", domain)
			}
			return addrs[0], nil
		}
	}

	return &Server{
		cfg:      cfg,
		dcOpt:    fullDCOpt,
		pool:     ws.NewPool(cfg.PoolSize, cfg.PoolSizeMedia, cfg.Verbose, resolveFunc),
		resolver: resolver,
		ctx:      ctx,
		cancel:   cancel,
		bufSz:    cfg.BufKB * 1024,
	}
}

func (s *Server) Run() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln

	log.Println(strings.Repeat("=", 60))
	log.Println("  Telegram WS Bridge Proxy (Go)")
	log.Printf("  Listening on   %s", addr)
	for dc, ip := range s.dcOpt {
		log.Printf("    DC%d: %s", dc, ip)
	}
	log.Println(strings.Repeat("=", 60))
	log.Printf("  SOCKS5 proxy -> %s  (no user/pass)", addr)
	log.Println(strings.Repeat("=", 60))

	s.pool.Warmup(s.dcOpt, telegram.WsDomains)
	go s.statsLoop()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return nil
			default:
				continue
			}
		}
		go s.handle(conn)
	}
}

func (s *Server) Shutdown() {
	s.cancel()
	if s.ln != nil {
		s.ln.Close()
	}
	log.Printf("Shutdown. Stats: %s", stats.Summary())
}

func (s *Server) statsLoop() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			log.Printf("stats: %s", stats.Summary())
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) handle(conn net.Conn) {
	atomic.AddInt64(&stats.Total, 1)
	label := conn.RemoteAddr().String()

	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(30 * time.Second)
		tcp.SetReadBuffer(s.bufSz)
		tcp.SetWriteBuffer(s.bufSz)
	}
	defer conn.Close()

	// === SOCKS5 handshake ===
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	io.ReadFull(conn, methods)
	conn.Write([]byte{5, 0}) // no-auth

	var req [4]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return
	}
	if req[1] != 1 { // CONNECT only
		conn.Write(socks5Reply(0x07))
		return
	}

	var dst string
	switch req[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		io.ReadFull(conn, b)
		dst = net.IP(b).String()
	case 3: // domain
		var dl [1]byte
		io.ReadFull(conn, dl[:])
		b := make([]byte, dl[0])
		io.ReadFull(conn, b)
		hostname := string(b)
		if s.resolver != nil {
			if addrs, err := s.resolver.LookupHost(hostname); err == nil {
				dst = addrs[0]
			} else {
				log.Printf("DoH lookup %s failed: %v", hostname, err)
				dst = hostname
			}
		} else {
			if addrs, err := net.LookupHost(hostname); err == nil {
				for _, a := range addrs {
					if !strings.Contains(a, ":") {
						dst = a
						break
					}
				}
			}
			if dst == "" {
				dst = hostname
			}
		}
	case 4: // IPv6
		b := make([]byte, 16)
		io.ReadFull(conn, b)
		dst = net.IP(b).String()
	default:
		conn.Write(socks5Reply(0x08))
		return
	}

	var portBuf [2]byte
	io.ReadFull(conn, portBuf[:])
	port := int(binary.BigEndian.Uint16(portBuf[:]))
	conn.SetReadDeadline(time.Time{})

	if strings.Contains(dst, ":") {
		conn.Write(socks5Reply(0x05))
		return
	}

	// === Non-Telegram → passthrough ===
	if !telegram.IsTelegramIP(dst) {
		atomic.AddInt64(&stats.Passthrough, 1)
		remote, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", dst, port), 10*time.Second)
		if err != nil {
			conn.Write(socks5Reply(0x05))
			return
		}
		conn.Write(socks5Reply(0x00))
		pipeTCP(conn, remote, s.bufSz)
		return
	}

	// === Telegram ===
	conn.Write(socks5Reply(0x00))

	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	init := make([]byte, 64)
	if _, err := io.ReadFull(conn, init); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	if isHTTP(init) {
		atomic.AddInt64(&stats.HTTPReject, 1)
		// Pass through as raw TCP — these are WebSocket upgrades or CDN downloads
		// by the Telegram client. Dropping them breaks media loading.
		if remote, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", dst, port), 10*time.Second); err == nil {
			remote.Write(init)
			pipeTCP(conn, remote, s.bufSz)
		}
		return
	}

	// Extract DC
	dc, isMedia, ok := telegram.DCFromInit(init)
	initPatched := false
	if !ok {
		if info, exists := telegram.IPtoDC[dst]; exists {
			dc, isMedia, ok = info.DC, info.IsMedia, true
			if _, has := s.dcOpt[dc]; has {
				init = telegram.PatchInitDC(init, dc, isMedia)
				initPatched = true
			}
		}
	}
	if !ok || s.dcOpt[dc] == "" {
		s.tcpFallback(conn, dst, port, init, label, dc, isMedia)
		return
	}

	dcKey := ws.DCKey(dc, isMedia)
	mTag := telegram.MediaTag(isMedia)

	// Blacklist check (with TTL expiry)
	if exp, bl := wsBlacklist.Load(dcKey); bl {
		if time.Now().Before(exp.(time.Time)) {
			s.tcpFallback(conn, dst, port, init, label, dc, isMedia)
			return
		}
		wsBlacklist.Delete(dcKey)
	}

	// Cooldown check: if WS recently failed, skip WS entirely this attempt
	// and go to TCP fallback. Do NOT attempt WS with reduced timeout —
	// that creates an infinite cascade (2s timeout fails → resets 30s cooldown).
	if fu, ok := dcFailUntil.Load(dcKey); ok && time.Now().Before(fu.(time.Time)) {
		s.tcpFallback(conn, dst, port, init, label, dc, isMedia)
		return
	}

	wsTimeout := 5 * time.Second

	domains := telegram.WsDomains(dc, isMedia)
	target := s.dcOpt[dc]

	// Try pool
	wsc := s.pool.Get(dc, isMedia, target, domains)
	if wsc == nil {
		// Direct connect
		allRedirects := true
		anyRedirect := false
		for _, domain := range domains {
			log.Printf("[%s] DC%d%s -> wss://%s/apiws via %s", label, dc, mTag, domain, target)
			var err error
			wsc, err = ws.Connect(target, domain, wsTimeout)
			if err != nil {
				// If configured IP failed, try DoH-resolved IP as fallback
				if s.resolver != nil {
					if addrs, resolveErr := s.resolver.LookupHost(domain); resolveErr == nil && len(addrs) > 0 && addrs[0] != target {
						wsc, err = ws.Connect(addrs[0], domain, wsTimeout)
						if err == nil {
							allRedirects = false
							break
						}
					}
				}
				atomic.AddInt64(&stats.WSErrors, 1)
				if he, ok := err.(*ws.HandshakeError); ok && he.IsRedirect() {
					anyRedirect = true
					continue
				}
				allRedirects = false
				continue
			}
			allRedirects = false
			break
		}

		if wsc == nil {
			if anyRedirect && allRedirects {
				wsBlacklist.Store(dcKey, time.Now().Add(blacklistTTL))
			} else {
				// Exponential backoff: each consecutive failure doubles the cooldown.
				// After 4 failures a DC gets a 10-min cooldown, stopping the
				// repeated 5s×2-domain timeout storm for unsupported WS DCs.
				raw, _ := dcFailCount.LoadOrStore(dcKey, int64(0))
				n := raw.(int64) + 1
				dcFailCount.Store(dcKey, n)
				idx := int(n) - 1
				if idx >= len(wsBackoffs) {
					idx = len(wsBackoffs) - 1
				}
				dcFailUntil.Store(dcKey, time.Now().Add(wsBackoffs[idx]))
			}
			s.tcpFallback(conn, dst, port, init, label, dc, isMedia)
			return
		}
	}

	dcFailUntil.Delete(dcKey)
	dcFailCount.Delete(dcKey)
	atomic.AddInt64(&stats.WS, 1)

	// Proactively warm the sibling pool (non-media↔media) so the next
	// media/control connection hits pool instead of paying TLS handshake.
	siblingKey := ws.DCKey(dc, !isMedia)
	go func() {
		siblingDomains := telegram.WsDomains(dc, !isMedia)
		s.pool.EnsureWarm(siblingKey, target, siblingDomains)
	}()

	var splitter *telegram.MsgSplitter
	if initPatched {
		splitter, _ = telegram.NewMsgSplitter(init)
	}

	if err := wsc.Send(init); err != nil {
		wsc.Close()
		s.tcpFallback(conn, dst, port, init, label, dc, isMedia)
		return
	}

	bridgeWS(conn, wsc, label, dc, mTag, dst, port, splitter, s.bufSz)
}

func (s *Server) tcpFallback(client net.Conn, dst string, port int, init []byte,
	label string, dc int, isMedia bool) {
	remote, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", dst, port), 10*time.Second)
	if err != nil {
		return
	}
	atomic.AddInt64(&stats.TCPFallback, 1)
	remote.Write(init)
	pipeTCP(client, remote, s.bufSz)
}

// bridgeWS does bidirectional TCP↔WS forwarding.
//
// Upload (TCP→WS): reads from client with a large buffer, applies WS framing.
// The WS Conn already has a 512KB bufio.Writer over TLS, so multiple frames
// get coalesced into TLS records efficiently.
//
// Download (WS→TCP): writes IMMEDIATELY to client on every WS frame.
// No buffering here — buffering would delay auth/handshake frames and break
// the MTProto session. The kernel TCP send buffer (1MB) handles coalescing.
func bridgeWS(client net.Conn, wsc *ws.Conn, label string,
	dc int, mTag, dst string, port int,
	splitter *telegram.MsgSplitter, bufSz int) {

	start := time.Now()
	var upBytes, downBytes, upPkts, downPkts int64
	done := make(chan struct{}, 2)

	// TCP → WS (upload)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, bufSz)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				atomic.AddInt64(&stats.BytesUp, int64(n))
				atomic.AddInt64(&upBytes, int64(n))
				atomic.AddInt64(&upPkts, 1)

				var sendErr error
				if splitter != nil {
					parts := splitter.Split(chunk)
					if len(parts) > 1 {
						sendErr = wsc.SendBatch(parts)
					} else {
						sendErr = wsc.Send(parts[0])
					}
				} else {
					sendErr = wsc.Send(chunk)
				}
				if sendErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WS → TCP (download)
	// Direct write — no bufio.Writer. Each WS frame is immediately written
	// to the client TCP socket. The OS kernel TCP buffer handles the rest.
	// This is critical: MTProto auth frames are ~200-400 bytes and the client
	// waits for them before sending the next message.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			data, err := wsc.Recv()
			if err != nil || data == nil {
				return
			}
			n := len(data)
			atomic.AddInt64(&stats.BytesDown, int64(n))
			atomic.AddInt64(&downBytes, int64(n))
			atomic.AddInt64(&downPkts, 1)
			if _, werr := client.Write(data); werr != nil {
				return
			}
		}
	}()

	<-done
	elapsed := time.Since(start)
	log.Printf("[%s] DC%d%s (%s:%d) closed: ^%s(%d) v%s(%d) %.1fs",
		label, dc, mTag, dst, port,
		telegram.HumanBytes(atomic.LoadInt64(&upBytes)), atomic.LoadInt64(&upPkts),
		telegram.HumanBytes(atomic.LoadInt64(&downBytes)), atomic.LoadInt64(&downPkts),
		elapsed.Seconds())
	wsc.Close()
	client.Close()
}

// pipeTCP is a zero-overhead bidirectional TCP relay.
func pipeTCP(a, b net.Conn, bufSz int) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, bufSz)
		io.CopyBuffer(dst, src, buf)
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	a.Close()
	b.Close()
}

func socks5Reply(s byte) []byte {
	return []byte{5, s, 0, 1, 0, 0, 0, 0, 0, 0}
}

func isHTTP(d []byte) bool {
	if len(d) < 4 {
		return false
	}
	s := string(d[:8])
	return strings.HasPrefix(s, "POST ") || strings.HasPrefix(s, "GET ") ||
		strings.HasPrefix(s, "HEAD ") || strings.HasPrefix(s, "OPTIONS ")
}
