package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/kardianos/service"

	"tg-ws-proxy/internal/config"
	"tg-ws-proxy/internal/proxy"
	"tg-ws-proxy/internal/ws"
)

const version = "2.0.0"

type program struct {
	srv   *proxy.Server
	cfg   config.Config
	dcOpt map[int]string
}

func (p *program) Start(service.Service) error { go p.run(); return nil }
func (p *program) run() {
	p.srv = proxy.New(p.cfg, p.dcOpt)
	if err := p.srv.Run(); err != nil {
		log.Printf("proxy error: %v", err)
	}
}
func (p *program) Stop(service.Service) error {
	if p.srv != nil {
		p.srv.Shutdown()
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var (
		port        = flag.Int("port", 0, "Listen port (default 1080)")
		host        = flag.String("host", "", "Listen host (default 127.0.0.1)")
		dcIPs       multiFlag
		verb        = flag.Bool("v", false, "Debug logging")
		cfgPath     = flag.String("config", "", "Config file path")
		svcCmd      = flag.String("service", "", "install|uninstall|start|stop|restart|status")
		logFile     = flag.String("log-file", "", "Log to file")
		poolSz      = flag.Int("pool-size", 0, "WS pool size per non-media DC (default 8)")
		poolSzMedia = flag.Int("pool-size-media", 0, "WS pool size per media DC (default 16)")
		bufKB       = flag.Int("buf-kb", 0, "Buffer KB (default 1024)")
		dohFlag     = flag.String("doh", "", "DNS-over-HTTPS provider: cloudflare (default), google, system, or https://... URL")
		tlsFrag     = flag.Int("tls-frag", -1, "TLS ClientHello split offset for DPI bypass (0=off, default 6)")
		showVer     = flag.Bool("version", false, "Show version")
	)
	flag.Var(&dcIPs, "dc-ip", "DC:IP pair, e.g. 2:149.154.167.220")
	flag.Parse()

	if *showVer {
		fmt.Printf("tg-ws-proxy v%s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	// Load config
	cfgFile := *cfgPath
	if cfgFile == "" {
		cfgFile = filepath.Join(config.Dir(), "config.json")
	}
	cfg, err := config.Load(cfgFile)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("config load warning: %v", err)
	}

	// CLI overrides
	if *port != 0 {
		cfg.Port = *port
	}
	if *host != "" {
		cfg.Host = *host
	}
	if len(dcIPs) > 0 {
		cfg.DcIPs = dcIPs
	}
	if *verb {
		cfg.Verbose = true
	}
	if *logFile != "" {
		cfg.LogFile = *logFile
	}
	if *poolSz > 0 {
		cfg.PoolSize = *poolSz
	}
	if *poolSzMedia > 0 {
		cfg.PoolSizeMedia = *poolSzMedia
	}
	if *bufKB > 0 {
		cfg.BufKB = *bufKB
	}
	if *dohFlag != "" {
		cfg.DoH = *dohFlag
	}
	if *tlsFrag >= 0 {
		cfg.TLSFrag = *tlsFrag
	}

	// Defaults
	if cfg.Port == 0 {
		cfg.Port = 1080
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	// NOTE: we do NOT fall back to hardcoded DcIPs here.
	// telegram.AutoFillDCs always provides the latest DefaultDCIPs for all 5 DCs.
	// Hardcoded defaults here would be saved to config.json and become stale on the next update.
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 8
	}
	if cfg.PoolSizeMedia == 0 {
		cfg.PoolSizeMedia = 16
	}
	if cfg.BufKB == 0 {
		cfg.BufKB = 1024
	}
	if cfg.DoH == "" {
		cfg.DoH = "all"
	}
	if cfg.TLSFrag == 0 {
		cfg.TLSFrag = 6
	}

	dcOpt, err := config.ParseDcIPs(cfg.DcIPs)
	if err != nil {
		log.Fatalf("bad dc-ip: %v", err)
	}

	// Merge DC IPs: DefaultDCIPs (base) → config.json → CLI flags (highest priority).
	// CLI flags override config, config overrides defaults.
	proxyDcOpt := dcOpt
	if len(dcIPs) > 0 {
		// CLI flags were passed — use them as overrides on top of defaults
		cliOpt, _ := config.ParseDcIPs(dcIPs)
		for dc, ip := range cliOpt {
			proxyDcOpt[dc] = ip
		}
	}

	// Logging
	if cfg.LogFile != "" {
		os.MkdirAll(filepath.Dir(cfg.LogFile), 0755)
		f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.Ltime)

	// Service
	svcCfg := &service.Config{
		Name:        "TgWsProxy",
		DisplayName: "Telegram WebSocket Proxy",
		Description: "SOCKS5 proxy for Telegram via WebSocket",
		Arguments:   []string{"--config", cfgFile},
	}

	prg := &program{cfg: cfg, dcOpt: proxyDcOpt}
	ws.TLSFragSize = cfg.TLSFrag
	s, err := service.New(prg, svcCfg)
	if err != nil {
		log.Fatalf("service init: %v", err)
	}

	if *svcCmd != "" {
		switch *svcCmd {
		case "install":
			config.Save(cfgFile, cfg)
			check(s.Install(), "install")
		case "uninstall":
			check(s.Uninstall(), "uninstall")
		case "start":
			check(s.Start(), "start")
		case "stop":
			check(s.Stop(), "stop")
		case "restart":
			check(s.Restart(), "restart")
		case "status":
			st, err := s.Status()
			if err != nil {
				log.Fatal(err)
			}
			switch st {
			case service.StatusRunning:
				fmt.Println("running")
			case service.StatusStopped:
				fmt.Println("stopped")
			default:
				fmt.Println("unknown")
			}
		default:
			log.Fatalf("unknown: %s", *svcCmd)
		}
		return
	}

	if service.Interactive() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			prg.Stop(s)
			os.Exit(0)
		}()
	}

	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

func check(err error, op string) {
	if err != nil {
		log.Fatalf("service %s: %v", op, err)
	}
	fmt.Printf("service %s: ok\n", op)
}
