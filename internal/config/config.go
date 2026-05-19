package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/calmlax/aevons-gateway/internal/model"

	frameworkconfig "github.com/calmlax/aevons-framework/config"
	frameworkconsul "github.com/calmlax/aevons-framework/core/consul"
	"github.com/goccy/go-yaml"
)

type Settings struct {
	Gateway    model.GatewayConfig
	Swagger    model.SwaggerConfig
	Services   []model.ServiceConfig
	ClientAuth model.ClientAuthConfig
}

type fileConfig struct {
	Gateway    model.GatewayConfig    `yaml:"gateway"`
	Swagger    model.SwaggerConfig    `yaml:"swagger"`
	Services   []model.ServiceConfig  `yaml:"services"`
	ClientAuth model.ClientAuthConfig `yaml:"client_auth"`
}

// Load reads local config only.
// For gateway startup, prefer LoadWithConsul so KV-based governance config can override local defaults.
func Load(configDir, env string) (Settings, error) {
	return LoadWithConsul(configDir, env, frameworkconfig.ConsulConfig{})
}

// LoadWithConsul builds settings in three layers:
// 1. framework defaults
// 2. local config.yaml + optional env overlay
// 3. Consul KV overrides for complex governance config such as rate limits
func LoadWithConsul(configDir, env string, consulCfg frameworkconfig.ConsulConfig) (Settings, error) {
	cfg := Settings{
		Gateway: model.GatewayConfig{
			TrustedProxies: []string{"127.0.0.1"},
			TimeoutSeconds: 15,
			MaxBodyBytes:   10 * 1024 * 1024,
			Discovery: model.DiscoveryConfig{
				RefreshSeconds:      3,
				StaleIfErrorSeconds: 30,
			},
			RateLimit: model.RateLimitConfig{
				Enabled:   false,
				FailOpen:  true,
				KeyPrefix: "gateway:rate-limit:",
				Default: model.RateLimitRuleConfig{
					Enabled:       false,
					KeyBy:         []string{"client", "ip"},
					WindowSeconds: 60,
					Limit:         600,
				},
			},
		},
		Swagger: model.SwaggerConfig{
			Enabled:    true,
			UIEnabled:  true,
			AllowedIPs: []string{"127.0.0.1", "::1"},
		},
		ClientAuth: model.ClientAuthConfig{
			Enabled: true,
		},
	}

	if err := mergeFile(filepath.Join(configDir, "config.yaml"), &cfg); err != nil {
		return Settings{}, err
	}

	if env != "" {
		overlayPath := filepath.Join(configDir, fmt.Sprintf("config.%s.yaml", env))
		if _, err := os.Stat(overlayPath); err == nil {
			if err := mergeFile(overlayPath, &cfg); err != nil {
				return Settings{}, err
			}
		}
	}

	if err := mergeConsulKV(&cfg, consulCfg); err != nil {
		return Settings{}, err
	}

	if err := validate(cfg); err != nil {
		return Settings{}, err
	}

	return cfg, nil
}

// mergeFile overlays local YAML onto the in-memory defaults.
// It is intentionally field-by-field so gateway-specific defaults stay explicit and easy to audit.
func mergeFile(path string, cfg *Settings) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read gateway config %s: %w", path, err)
	}

	var next fileConfig
	if err := yaml.Unmarshal(data, &next); err != nil {
		return fmt.Errorf("parse gateway config %s: %w", path, err)
	}

	if len(next.Gateway.TrustedProxies) > 0 {
		cfg.Gateway.TrustedProxies = next.Gateway.TrustedProxies
	}
	if next.Gateway.TimeoutSeconds > 0 {
		cfg.Gateway.TimeoutSeconds = next.Gateway.TimeoutSeconds
	}
	if next.Gateway.MaxBodyBytes > 0 {
		cfg.Gateway.MaxBodyBytes = next.Gateway.MaxBodyBytes
	}
	if next.Gateway.Discovery.RefreshSeconds > 0 {
		cfg.Gateway.Discovery.RefreshSeconds = next.Gateway.Discovery.RefreshSeconds
	}
	if next.Gateway.Discovery.StaleIfErrorSeconds > 0 {
		cfg.Gateway.Discovery.StaleIfErrorSeconds = next.Gateway.Discovery.StaleIfErrorSeconds
	}
	cfg.Gateway.RateLimit.Enabled = next.Gateway.RateLimit.Enabled
	cfg.Gateway.RateLimit.FailOpen = next.Gateway.RateLimit.FailOpen
	if strings.TrimSpace(next.Gateway.RateLimit.KeyPrefix) != "" {
		cfg.Gateway.RateLimit.KeyPrefix = strings.TrimSpace(next.Gateway.RateLimit.KeyPrefix)
	}
	if strings.TrimSpace(next.Gateway.RateLimit.ConsulKVKey) != "" {
		cfg.Gateway.RateLimit.ConsulKVKey = strings.TrimSpace(next.Gateway.RateLimit.ConsulKVKey)
	}
	if next.Gateway.RateLimit.Default.Enabled {
		cfg.Gateway.RateLimit.Default.Enabled = true
	}
	if len(next.Gateway.RateLimit.Default.KeyBy) > 0 {
		cfg.Gateway.RateLimit.Default.KeyBy = next.Gateway.RateLimit.Default.KeyBy
	}
	if next.Gateway.RateLimit.Default.WindowSeconds > 0 {
		cfg.Gateway.RateLimit.Default.WindowSeconds = next.Gateway.RateLimit.Default.WindowSeconds
	}
	if next.Gateway.RateLimit.Default.Limit > 0 {
		cfg.Gateway.RateLimit.Default.Limit = next.Gateway.RateLimit.Default.Limit
	}
	if len(next.Gateway.RateLimit.Rules) > 0 {
		cfg.Gateway.RateLimit.Rules = next.Gateway.RateLimit.Rules
	}

	cfg.Swagger.Enabled = next.Swagger.Enabled
	cfg.Swagger.UIEnabled = next.Swagger.UIEnabled
	if len(next.Swagger.AllowedIPs) > 0 {
		cfg.Swagger.AllowedIPs = next.Swagger.AllowedIPs
	}
	if len(next.Swagger.Docs) > 0 {
		for i := range next.Swagger.Docs {
			if strings.TrimSpace(next.Swagger.Docs[i].Path) == "" {
				next.Swagger.Docs[i].Path = "/api/swagger.json"
			}
		}
		cfg.Swagger.Docs = next.Swagger.Docs
	}

	if len(next.Services) > 0 {
		cfg.Services = next.Services
	}
	cfg.ClientAuth.Enabled = next.ClientAuth.Enabled

	return nil
}

// mergeConsulKV lets simple bootstrap config stay local while moving complex,
// frequently adjusted governance rules into Consul KV.
func mergeConsulKV(cfg *Settings, consulCfg frameworkconfig.ConsulConfig) error {
	if cfg == nil {
		return nil
	}
	if !cfg.Gateway.RateLimit.Enabled {
		return nil
	}
	key := strings.TrimSpace(cfg.Gateway.RateLimit.ConsulKVKey)
	if key == "" {
		return nil
	}
	if !consulCfg.Enabled {
		return fmt.Errorf("gateway rate_limit consul_kv_key configured but consul is disabled")
	}

	registry, err := frameworkconsul.New(consulCfg)
	if err != nil {
		return err
	}
	raw, err := registry.GetKV(key)
	if err != nil {
		return fmt.Errorf("load gateway rate_limit from consul kv %s: %w", key, err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var override model.RateLimitConfig
	if err := yaml.Unmarshal([]byte(raw), &override); err != nil {
		return fmt.Errorf("parse gateway rate_limit consul kv %s: %w", key, err)
	}

	cfg.Gateway.RateLimit.Enabled = override.Enabled
	cfg.Gateway.RateLimit.FailOpen = override.FailOpen
	if strings.TrimSpace(override.KeyPrefix) != "" {
		cfg.Gateway.RateLimit.KeyPrefix = strings.TrimSpace(override.KeyPrefix)
	}
	if strings.TrimSpace(override.ConsulKVKey) != "" {
		cfg.Gateway.RateLimit.ConsulKVKey = strings.TrimSpace(override.ConsulKVKey)
	}
	if override.Default.Enabled {
		cfg.Gateway.RateLimit.Default.Enabled = true
	}
	if len(override.Default.KeyBy) > 0 {
		cfg.Gateway.RateLimit.Default.KeyBy = override.Default.KeyBy
	}
	if override.Default.WindowSeconds > 0 {
		cfg.Gateway.RateLimit.Default.WindowSeconds = override.Default.WindowSeconds
	}
	if override.Default.Limit > 0 {
		cfg.Gateway.RateLimit.Default.Limit = override.Default.Limit
	}
	if len(override.Rules) > 0 {
		cfg.Gateway.RateLimit.Rules = override.Rules
	}
	return nil
}

// validate enforces the minimum runtime guarantees needed by the gateway:
// at least one service route, and positive limit/window values for enabled rate-limit rules.
func validate(cfg Settings) error {
	if len(cfg.Services) == 0 {
		return errors.New("gateway config requires at least one service")
	}

	seenServiceIDs := make(map[string]struct{}, len(cfg.Services))
	for _, service := range cfg.Services {
		if strings.TrimSpace(service.ID) == "" || strings.TrimSpace(service.Name) == "" || strings.TrimSpace(service.Prefix) == "" {
			return errors.New("gateway service requires id, name and prefix")
		}
		if _, exists := seenServiceIDs[service.ID]; exists {
			return fmt.Errorf("duplicate service id: %s", service.ID)
		}
		seenServiceIDs[service.ID] = struct{}{}
	}

	if cfg.Gateway.RateLimit.Enabled {
		if cfg.Gateway.RateLimit.Default.Enabled {
			if cfg.Gateway.RateLimit.Default.Limit <= 0 || cfg.Gateway.RateLimit.Default.WindowSeconds <= 0 {
				return errors.New("gateway rate_limit.default requires positive limit and window_seconds")
			}
		}
		for _, rule := range cfg.Gateway.RateLimit.Rules {
			if !rule.Enabled {
				continue
			}
			if strings.TrimSpace(rule.Name) == "" {
				return errors.New("gateway rate_limit.rules requires name")
			}
			if rule.Limit <= 0 || rule.WindowSeconds <= 0 {
				return fmt.Errorf("gateway rate_limit rule %s requires positive limit and window_seconds", rule.Name)
			}
		}
	}

	return nil
}
