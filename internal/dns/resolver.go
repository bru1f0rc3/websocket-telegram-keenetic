// Package dns provides a DNS-over-HTTPS resolver with in-memory TTL caching.
// Supports multiple upstream providers with parallel racing — the first
// successful response wins, giving both speed and redundancy.
package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Built-in DoH providers — contacted by IP to avoid circular DNS dependency.
var Providers = map[string]string{
	"cloudflare": "https://1.1.1.1/dns-query",
	"google":     "https://8.8.8.8/dns-query",
	"quad9":      "https://9.9.9.9:5053/dns-query",
	"adguard":    "https://94.140.14.14/dns-query",
	"controld":   "https://76.76.2.22/dns-query",
	"dns.sb":     "https://185.222.222.222/dns-query",
}

// DefaultProviders is the set used when no specific provider is configured.
// Ordered by typical latency; racing picks the fastest.
var DefaultProviders = []string{
	"https://1.1.1.1/dns-query",         // Cloudflare
	"https://8.8.8.8/dns-query",         // Google
	"https://9.9.9.9:5053/dns-query",    // Quad9
	"https://94.140.14.14/dns-query",    // AdGuard
	"https://76.76.2.22/dns-query",      // ControlD
	"https://185.222.222.222/dns-query", // DNS.SB
}

type cacheEntry struct {
	addrs  []string
	expiry time.Time
}

// Resolver resolves hostnames via DNS-over-HTTPS with multi-provider racing.
type Resolver struct {
	urls    []string
	cache   sync.Map
	clients []*http.Client
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   3 * time.Second,
			ResponseHeaderTimeout: 3 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// New returns a Resolver with a single upstream URL.
func New(dohURL string) *Resolver {
	return &Resolver{
		urls:    []string{dohURL},
		clients: []*http.Client{newHTTPClient()},
	}
}

// NewMulti returns a Resolver that races multiple DoH providers in parallel.
// The first successful response wins.
func NewMulti(urls []string) *Resolver {
	clients := make([]*http.Client, len(urls))
	for i := range urls {
		clients[i] = newHTTPClient()
	}
	return &Resolver{urls: urls, clients: clients}
}

type dohResp struct {
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
		TTL  int    `json:"TTL"`
	} `json:"Answer"`
}

// LookupHost resolves host to IPv4 addresses.
// Races all configured providers in parallel; first valid response wins.
// Falls back to system DNS on total failure.
func (r *Resolver) LookupHost(host string) ([]string, error) {
	// Cache hit
	if v, ok := r.cache.Load(host); ok {
		if e := v.(cacheEntry); time.Now().Before(e.expiry) {
			return e.addrs, nil
		}
		r.cache.Delete(host)
	}

	if len(r.urls) == 1 {
		addrs, ttl, err := r.queryOne(r.urls[0], r.clients[0], host)
		if err != nil {
			return r.systemFallback(host)
		}
		r.cache.Store(host, cacheEntry{addrs: addrs, expiry: time.Now().Add(time.Duration(ttl) * time.Second)})
		return addrs, nil
	}

	// Race all providers
	type result struct {
		addrs []string
		ttl   int
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch := make(chan result, len(r.urls))
	for i, url := range r.urls {
		go func(u string, c *http.Client) {
			addrs, ttl, err := r.queryOneCtx(ctx, u, c, host)
			if err == nil && len(addrs) > 0 {
				ch <- result{addrs, ttl}
			}
		}(url, r.clients[i])
	}

	select {
	case res := <-ch:
		cancel() // stop other goroutines
		r.cache.Store(host, cacheEntry{addrs: res.addrs, expiry: time.Now().Add(time.Duration(res.ttl) * time.Second)})
		return res.addrs, nil
	case <-ctx.Done():
		return r.systemFallback(host)
	}
}

func (r *Resolver) queryOne(url string, client *http.Client, host string) ([]string, int, error) {
	return r.queryOneCtx(context.Background(), url, client, host)
}

func (r *Resolver) queryOneCtx(ctx context.Context, url string, client *http.Client, host string) ([]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url+"?name="+host+"&type=A", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return nil, 0, err
	}

	var doh dohResp
	if err := json.Unmarshal(body, &doh); err != nil {
		return nil, 0, err
	}

	var addrs []string
	ttl := 300
	for _, a := range doh.Answer {
		if a.Type == 1 {
			addrs = append(addrs, a.Data)
			if a.TTL > 0 && a.TTL < ttl {
				ttl = a.TTL
			}
		}
	}
	if len(addrs) == 0 {
		return nil, 0, fmt.Errorf("no A records for %s from %s", host, url)
	}
	return addrs, ttl, nil
}

func (r *Resolver) systemFallback(host string) ([]string, error) {
	all, err := net.LookupHost(host)
	if err != nil {
		return nil, err
	}
	var v4 []string
	for _, a := range all {
		if strings.IndexByte(a, ':') < 0 {
			v4 = append(v4, a)
		}
	}
	if len(v4) == 0 {
		return nil, fmt.Errorf("no IPv4 records for %s", host)
	}
	return v4, nil
}

// URLsFor converts a provider name or URL into a list of DoH endpoint URLs.
//
//	"cloudflare"      → single Cloudflare URL
//	"all"             → all built-in providers (racing mode)
//	"https://..."     → custom URL
//	"system" / ""     → nil (caller uses net.LookupHost)
func URLsFor(name string) []string {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "system", "":
		return nil
	case "all":
		return DefaultProviders
	default:
		if url, ok := Providers[lower]; ok {
			return []string{url}
		}
		if strings.HasPrefix(name, "https://") {
			return []string{name}
		}
		return DefaultProviders
	}
}

// URLFor returns a single DoH endpoint URL for backward compatibility.
func URLFor(name string) string {
	urls := URLsFor(name)
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}
