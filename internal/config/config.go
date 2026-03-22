package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	DcIPs         []string `json:"dc_ip"`
	Verbose       bool     `json:"verbose"`
	PoolSize      int      `json:"pool_size"`
	PoolSizeMedia int      `json:"pool_size_media"`
	BufKB         int      `json:"buf_kb"`
	LogFile       string   `json:"log_file"`
	// DoH is the DNS-over-HTTPS provider: "cloudflare" (default), "google", "system", or a full https:// URL.
	DoH string `json:"doh"`
	// TLSFrag splits the TLS ClientHello at this byte offset for DPI bypass.
	// 0 = disabled, default 6 (splits inside TLS record header).
	TLSFrag int `json:"tls_frag"`
}

func Default() Config {
	return Config{
		Host:          "127.0.0.1",
		Port:          1080,
		DcIPs:         []string{"2:149.154.167.220", "4:149.154.167.220"},
		PoolSize:      8,
		PoolSizeMedia: 16,
		BufKB:         1024,
		DoH:           "all",
		TLSFrag:       6,
	}
}

func Dir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "TgWsProxy")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "TgWsProxy")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "TgWsProxy")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "TgWsProxy")
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func ParseDcIPs(list []string) (map[int]string, error) {
	result := make(map[int]string, len(list))
	for _, entry := range list {
		idx := strings.IndexByte(entry, ':')
		if idx < 0 {
			return nil, fmt.Errorf("invalid dc-ip %q, expected DC:IP", entry)
		}
		var dc int
		if _, err := fmt.Sscanf(entry[:idx], "%d", &dc); err != nil {
			return nil, fmt.Errorf("invalid dc in %q", entry)
		}
		result[dc] = entry[idx+1:]
	}
	return result, nil
}
