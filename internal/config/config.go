package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const AppName = "hubhq"

var debugf func(string, ...any)

func SetDebugLogger(logger func(string, ...any)) {
	debugf = logger
}

type Config struct {
	DefaultHub string         `json:"default_hub"`
	Hubs       map[string]Hub `json:"hubs"`
	LLM        LLMConfig      `json:"llm"`
	Apple      AppleConfig    `json:"apple"`
	DukaOne    DukaOneConfig  `json:"dukaone"`
	Location   LocationConfig `json:"location"`
	Cache      Cache          `json:"-"`
}

type Hub struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	Token        string    `json:"-"`
	DiscoveredAt time.Time `json:"discovered_at"`
}

type Cache struct {
	UpdatedAt time.Time       `json:"updated_at"`
	Devices   []CachedItem    `json:"devices"`
	Scenes    []CachedItem    `json:"scenes"`
	Speakers  []CachedSpeaker `json:"speakers,omitempty"`
	RawHome   json.RawMessage `json:"raw_home,omitempty"`
}

type CachedItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CustomName string `json:"custom_name,omitempty"`
	Type       string `json:"type"`
	Category   string `json:"category,omitempty"`
	Room       string `json:"room,omitempty"`
	DeviceType string `json:"device_type,omitempty"`
}

type CachedSpeaker struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Room     string `json:"room,omitempty"`
	Playback string `json:"playback,omitempty"`
	Volume   string `json:"volume,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type LLMConfig struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	APIKey          string `json:"api_key,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds,omitempty"`
	CacheMaxEntries int    `json:"cache_max_entries,omitempty"`
}

type AppleConfig struct {
	AirPlayCredentials string `json:"airplay_credentials,omitempty"`
	AirPlayPassword    string `json:"airplay_password,omitempty"`
	RAOPCredentials    string `json:"raop_credentials,omitempty"`
	RAOPPassword       string `json:"raop_password,omitempty"`
}

type DukaOneConfig struct {
	Backend  string                `json:"backend,omitempty"`
	Python   string                `json:"python,omitempty"`
	Password string                `json:"password,omitempty"`
	Devices  []DukaOneDeviceConfig `json:"devices,omitempty"`
}

type DukaOneDeviceConfig struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	Room     string `json:"room,omitempty"`
}

type LocationConfig struct {
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	TimeZone  string  `json:"time_zone,omitempty"`
}

type tokenFile struct {
	Hubs map[string]string `json:"hubs"`
}

type legacyConfig struct {
	DefaultHub string               `json:"default_hub"`
	Hubs       map[string]legacyHub `json:"hubs"`
	Cache      Cache                `json:"cache"`
	LLM        LLMConfig            `json:"llm"`
	Apple      AppleConfig          `json:"apple"`
	DukaOne    DukaOneConfig        `json:"dukaone"`
	Location   LocationConfig       `json:"location"`
}

type legacyHub struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	Token        string    `json:"token"`
	DiscoveredAt time.Time `json:"discovered_at"`
}

func Load() (*Config, string, error) {
	path, err := configPath()
	if err != nil {
		return nil, "", err
	}

	cfg := &Config{
		Hubs: map[string]Hub{},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, path, nil
		}
		return nil, "", fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, "", fmt.Errorf("parse config: %w", err)
	}
	if cfg.Hubs == nil {
		cfg.Hubs = map[string]Hub{}
	}
	if err := loadLegacyEmbeddedData(cfg, data); err != nil {
		return nil, "", err
	}
	if err := loadTokens(cfg); err != nil {
		return nil, "", err
	}
	if err := invalidateCachesOnRebuild(); err != nil {
		return nil, "", err
	}
	if err := loadCache(cfg); err != nil {
		return nil, "", err
	}
	return cfg, path, nil
}

func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	configOnly := &Config{
		DefaultHub: cfg.DefaultHub,
		Hubs:       map[string]Hub{},
		LLM:        cfg.LLM,
		Apple:      cfg.Apple,
		DukaOne:    cfg.DukaOne,
		Location:   cfg.Location,
	}
	for id, hub := range cfg.Hubs {
		copyHub := hub
		copyHub.Token = ""
		configOnly.Hubs[id] = copyHub
	}
	data, err := json.MarshalIndent(configOnly, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := saveTokens(cfg); err != nil {
		return err
	}
	if err := saveCache(cfg.Cache); err != nil {
		return err
	}
	return nil
}

func (c *Config) DefaultHubConfig() (Hub, error) {
	if c.DefaultHub == "" {
		return Hub{}, errors.New("no default hub configured; run `hubhq auth` first")
	}
	hub, ok := c.Hubs[c.DefaultHub]
	if !ok {
		return Hub{}, errors.New("default hub missing from config; run `hubhq auth` again")
	}
	return hub, nil
}

func (c *Config) SetHub(h Hub, asDefault bool) {
	if c.Hubs == nil {
		c.Hubs = map[string]Hub{}
	}
	c.Hubs[h.ID] = h
	if asDefault || c.DefaultHub == "" {
		c.DefaultHub = h.ID
	}
}

func (c *Config) SortedHubs() []Hub {
	hubs := make([]Hub, 0, len(c.Hubs))
	for _, hub := range c.Hubs {
		hubs = append(hubs, hub)
	}
	sort.Slice(hubs, func(i, j int) bool {
		return strings.ToLower(hubs[i].Name) < strings.ToLower(hubs[j].Name)
	})
	return hubs
}

func configPath() (string, error) {
	return ConfigPath()
}

func ConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(base, AppName, "config.json"), nil
	}
	return filepath.Join(base, AppName, "config.json"), nil
}

func AppDir() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func TokensPath() (string, error) {
	dir, err := AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tokens.json"), nil
}

func CachePath() (string, error) {
	dir, err := AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.json"), nil
}

func LLMCachePath() (string, error) {
	dir, err := AppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "llm-cache.json"), nil
}

func invalidateCachesOnRebuild() error {
	exePath, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil && strings.TrimSpace(resolved) != "" {
		exePath = resolved
	}
	exeInfo, err := os.Stat(exePath)
	if err != nil {
		return nil
	}
	for _, pathFn := range []func() (string, error){CachePath, LLMCachePath} {
		cachePath, err := pathFn()
		if err != nil {
			return err
		}
		info, err := os.Stat(cachePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if exeInfo.ModTime().After(info.ModTime()) {
			if err := os.Remove(cachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove stale cache %s: %w", cachePath, err)
			}
			if debugf != nil {
				debugf("config: invalidated %s because binary is newer", cachePath)
			}
		}
	}
	return nil
}

func loadTokens(cfg *Config) error {
	path, err := TokensPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read tokens: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return fmt.Errorf("parse tokens: %w", err)
	}
	for id, token := range tf.Hubs {
		hub := cfg.Hubs[id]
		hub.Token = token
		cfg.Hubs[id] = hub
	}
	return nil
}

func saveTokens(cfg *Config) error {
	path, err := TokensPath()
	if err != nil {
		return err
	}
	tf := tokenFile{Hubs: map[string]string{}}
	for id, hub := range cfg.Hubs {
		if hub.Token != "" {
			tf.Hubs[id] = hub.Token
		}
	}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	return nil
}

func loadCache(cfg *Config) error {
	path, err := CachePath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read cache: %w", err)
	}
	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return fmt.Errorf("parse cache: %w", err)
	}
	cfg.Cache = cache
	return nil
}

func saveCache(cache Cache) error {
	path, err := CachePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	return nil
}

func loadLegacyEmbeddedData(cfg *Config, raw []byte) error {
	var legacy legacyConfig
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return fmt.Errorf("parse legacy config: %w", err)
	}
	for id, hub := range legacy.Hubs {
		current, ok := cfg.Hubs[id]
		if !ok {
			current = Hub{
				ID:           hub.ID,
				Name:         hub.Name,
				Host:         hub.Host,
				Port:         hub.Port,
				DiscoveredAt: hub.DiscoveredAt,
			}
		}
		if current.Token == "" {
			current.Token = hub.Token
		}
		cfg.Hubs[id] = current
	}
	if cfg.Cache.UpdatedAt.IsZero() {
		cfg.Cache = legacy.Cache
	}
	return nil
}
