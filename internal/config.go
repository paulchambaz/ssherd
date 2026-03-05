package internal

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

type ServerConfig struct {
	Host string
	Port string
}

type Config struct {
	Server    ServerConfig
	Mode      string
	Log       string
	CachePath string `toml:"cache_path"`
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: "1321",
		},
		Mode:      "prod",
		Log:       "/var/log/ssherd/ssherd.log",
		CachePath: filepath.Join(home, ".cache", "ssherd"),
	}
}

func LoadConfig(configPath string) (Config, error) {
	config := defaultConfig()

	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		return Config{}, err
	}

	config.loadFromEnv()

	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

func (cfg *Config) loadFromEnv() {
	if host := os.Getenv("SSHERD_SERVER_HOST"); host != "" {
		cfg.Server.Host = host
	}
	if port := os.Getenv("SSHERD_SERVER_PORT"); port != "" {
		cfg.Server.Port = port
	}
	if mode := os.Getenv("SSHERD_MODE"); mode != "" {
		cfg.Mode = mode
	}
	if log := os.Getenv("SSHERD_LOG"); log != "" {
		cfg.Log = log
	}
	if cache := os.Getenv("SSHERD_CACHE_PATH"); cache != "" {
		cfg.CachePath = cache
	}
}

func (cfg *Config) validate() error {
	if cfg.Server.Host == "" {
		return fmt.Errorf("server.host is required")
	}
	if ip := net.ParseIP(cfg.Server.Host); ip == nil && cfg.Server.Host != "0.0.0.0" {
		return fmt.Errorf("server.host must be a valid IP address")
	}
	if cfg.Server.Port == "" {
		return fmt.Errorf("server.port is required")
	}
	if port, err := strconv.Atoi(cfg.Server.Port); err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("server.port must be a valid port number (1-65535)")
	}
	if cfg.Log == "" {
		return fmt.Errorf("server.log is required")
	}
	if cfg.CachePath == "" {
		return fmt.Errorf("cache_path is required")
	}
	if err := os.MkdirAll(cfg.CachePath, 0755); err != nil {
		return fmt.Errorf("cannot create cache directory %q: %w", cfg.CachePath, err)
	}

	return nil
}
