package ws

import (
	"log"
	"sync"
	"time"
)

const (
	poolMaxAge     = 60 * time.Second // short age = fresh connections
	poolProbeEvery = 20 * time.Second // probe frequently for stale conns
)

type poolEntry struct {
	conn    *Conn
	created time.Time
}

// Pool manages pre-warmed WebSocket connections per DC.
type Pool struct {
	mu        sync.Mutex
	idle      map[[2]int][]poolEntry
	refilling sync.Map
	size      int // non-media pool size per DC
	sizeMedia int // media pool size per DC (larger — Telegram opens many parallel media streams)
	verbose   bool
	// resolveFunc resolves a hostname to an IPv4 address.
	// Used to find the actual WS server IP from a kws domain.
	// DC IPs may be blocked by ISPs, but kws domains resolve to
	// dedicated WebSocket server IPs that are often still reachable.
	resolveFunc func(string) (string, error)
}

func NewPool(size, sizeMedia int, verbose bool, resolveFunc func(string) (string, error)) *Pool {
	p := &Pool{
		idle:        make(map[[2]int][]poolEntry),
		size:        size,
		sizeMedia:   sizeMedia,
		verbose:     verbose,
		resolveFunc: resolveFunc,
	}
	go p.probeLoop()
	return p
}

func DCKey(dc int, isMedia bool) [2]int {
	m := 0
	if isMedia {
		m = 1
	}
	return [2]int{dc, m}
}

// Get retrieves a pooled WS connection or nil.
func (p *Pool) Get(dc int, isMedia bool, targetIP string, domains []string) *Conn {
	key := DCKey(dc, isMedia)
	now := time.Now()

	p.mu.Lock()
	bucket := p.idle[key]
	for len(bucket) > 0 {
		e := bucket[0]
		bucket = bucket[1:]
		p.idle[key] = bucket

		if now.Sub(e.created) > poolMaxAge || e.conn.IsClosed() {
			go e.conn.Close()
			continue
		}
		p.mu.Unlock()
		go p.refill(key, targetIP, domains)
		return e.conn
	}
	p.mu.Unlock()
	go p.refill(key, targetIP, domains)
	return nil
}

func (p *Pool) refill(key [2]int, targetIP string, domains []string) {
	if _, loaded := p.refilling.LoadOrStore(key, true); loaded {
		return
	}
	defer p.refilling.Delete(key)

	p.mu.Lock()
	targetSize := p.size
	if key[1] == 1 { // is_media=true: use larger pool
		targetSize = p.sizeMedia
	}
	needed := targetSize - len(p.idle[key])
	p.mu.Unlock()
	if needed <= 0 {
		return
	}

	var wg sync.WaitGroup
	ch := make(chan *Conn, needed)
	for i := 0; i < needed; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dialOne(targetIP, domains, p.resolveFunc)
			if c != nil {
				ch <- c
			}
		}()
	}
	wg.Wait()
	close(ch)

	p.mu.Lock()
	for c := range ch {
		p.idle[key] = append(p.idle[key], poolEntry{conn: c, created: time.Now()})
	}
	cnt := len(p.idle[key])
	p.mu.Unlock()

	if p.verbose {
		mTag := ""
		if key[1] == 1 {
			mTag = "m"
		}
		log.Printf("WS pool refilled DC%d%s: %d ready", key[0], mTag, cnt)
	}
}

func dialOne(targetIP string, domains []string, resolve func(string) (string, error)) *Conn {
	for _, d := range domains {
		// Try the configured DC IP first (typically the working .220 endpoint)
		c, err := Connect(targetIP, d, 5*time.Second)
		if err != nil {
			// If configured IP failed and DoH is available, try resolved IP as fallback
			if resolve != nil {
				if resolved, rerr := resolve(d); rerr == nil && resolved != targetIP {
					c, err = Connect(resolved, d, 5*time.Second)
					if err == nil {
						return c
					}
				}
			}
			continue
		}
		return c
	}
	return nil
}

// EnsureWarm triggers a background refill for the given key if needed.
// Safe to call speculatively from a sibling connection handler.
func (p *Pool) EnsureWarm(key [2]int, targetIP string, domains []string) {
	go p.refill(key, targetIP, domains)
}

// Warmup pre-fills pool for all configured DCs.
func (p *Pool) Warmup(dcOpt map[int]string, domainFn func(int, bool) []string) {
	for dc, ip := range dcOpt {
		for _, isMedia := range []bool{false, true} {
			key := DCKey(dc, isMedia)
			domains := domainFn(dc, isMedia)
			go p.refill(key, ip, domains)
		}
	}
	log.Printf("WS pool warmup started for %d DC(s)", len(dcOpt))
}

// probeLoop pings idle connections to detect stale ones early.
func (p *Pool) probeLoop() {
	ticker := time.NewTicker(poolProbeEvery)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		for key, bucket := range p.idle {
			live := bucket[:0]
			for _, e := range bucket {
				if e.conn.IsClosed() || time.Since(e.created) > poolMaxAge {
					go e.conn.Close()
					continue
				}
				live = append(live, e)
			}
			p.idle[key] = live
		}
		p.mu.Unlock()
	}
}
