package llm

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/script"
)

const (
	defaultCacheTTL        = 24 * time.Hour
	defaultCacheMaxEntries = 256
)

type cacheEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	CreatedAt time.Time       `json:"created_at"`
}

type cachedScript struct {
	Script script.Document `json:"script"`
}

type cacheFile struct {
	Entries []cacheEntry `json:"entries"`
}

type diskCache struct {
	path       string
	ttl        time.Duration
	maxEntries int

	mu     sync.Mutex
	loaded bool
	file   cacheFile
}

func newDiskCache(cfg config.LLMConfig) *diskCache {
	path, err := config.LLMCachePath()
	if err != nil {
		return nil
	}
	ttl := defaultCacheTTL
	if cfg.CacheTTLSeconds > 0 {
		ttl = time.Duration(cfg.CacheTTLSeconds) * time.Second
	}
	maxEntries := defaultCacheMaxEntries
	if cfg.CacheMaxEntries > 0 {
		maxEntries = cfg.CacheMaxEntries
	}
	if cfg.CacheTTLSeconds < 0 || cfg.CacheMaxEntries < 0 {
		return nil
	}
	return &diskCache{
		path:       path,
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (c *diskCache) get(key string, into any) (bool, error) {
	if c == nil {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(); err != nil {
		return false, err
	}
	c.pruneExpiredLocked(time.Now().UTC())
	for _, entry := range c.file.Entries {
		if entry.Key != key {
			continue
		}
		if err := json.Unmarshal(entry.Value, into); err != nil {
			return false, fmt.Errorf("decode llm cache entry: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func (c *diskCache) put(key string, value json.RawMessage) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(); err != nil {
		return err
	}
	now := time.Now().UTC()
	c.pruneExpiredLocked(now)
	filtered := c.file.Entries[:0]
	for _, entry := range c.file.Entries {
		if entry.Key != key {
			filtered = append(filtered, entry)
		}
	}
	c.file.Entries = append(filtered, cacheEntry{
		Key:       key,
		Value:     append(json.RawMessage(nil), value...),
		CreatedAt: now,
	})
	sort.Slice(c.file.Entries, func(i, j int) bool {
		return c.file.Entries[i].CreatedAt.After(c.file.Entries[j].CreatedAt)
	})
	if len(c.file.Entries) > c.maxEntries {
		c.file.Entries = c.file.Entries[:c.maxEntries]
	}
	return c.saveLocked()
}

func (c *diskCache) loadLocked() error {
	if c.loaded {
		return nil
	}
	c.loaded = true
	data, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read llm cache: %w", err)
	}
	if err := json.Unmarshal(data, &c.file); err != nil {
		return fmt.Errorf("parse llm cache: %w", err)
	}
	return nil
}

func (c *diskCache) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("create llm cache dir: %w", err)
	}
	data, err := json.MarshalIndent(c.file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal llm cache: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return fmt.Errorf("write llm cache: %w", err)
	}
	return nil
}

func (c *diskCache) pruneExpiredLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	filtered := c.file.Entries[:0]
	for _, entry := range c.file.Entries {
		if now.Sub(entry.CreatedAt) <= c.ttl {
			filtered = append(filtered, entry)
		}
	}
	c.file.Entries = filtered
}

func promptCacheKey(baseURL, model, system, prompt, schema string, home HomeContext) string {
	payload := map[string]any{
		"base_url": baseURL,
		"model":    model,
		"system":   system,
		"prompt":   strings.TrimSpace(prompt),
		"schema":   schema,
		"home":     normalizeHomeContextForCache(home),
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func normalizeHomeContextForCache(home HomeContext) HomeContext {
	normalized := HomeContext{
		HubHost: strings.TrimSpace(home.HubHost),
		Scenes:  make([]map[string]any, 0, len(home.Scenes)),
		Devices: make([]map[string]any, 0, len(home.Devices)),
	}
	for _, scene := range home.Scenes {
		normalized.Scenes = append(normalized.Scenes, map[string]any{
			"id":   fmt.Sprint(scene["id"]),
			"name": fmt.Sprint(scene["name"]),
			"type": fmt.Sprint(scene["type"]),
		})
	}
	for _, device := range home.Devices {
		room := ""
		if nested, ok := device["room"].(map[string]any); ok {
			room = fmt.Sprint(nested["name"])
		}
		normalized.Devices = append(normalized.Devices, map[string]any{
			"id":          fmt.Sprint(device["id"]),
			"name":        fmt.Sprint(device["name"]),
			"custom_name": fmt.Sprint(device["custom_name"]),
			"type":        fmt.Sprint(device["type"]),
			"room":        room,
			"columns": map[string]any{
				"ID":   fmt.Sprint(mapLookup(device, "columns", "ID")),
				"NAME": fmt.Sprint(mapLookup(device, "columns", "NAME")),
				"TYPE": fmt.Sprint(mapLookup(device, "columns", "TYPE")),
				"ROOM": fmt.Sprint(mapLookup(device, "columns", "ROOM")),
			},
			"lookup_terms": cloneLookupTerms(device["lookup_terms"]),
		})
	}
	sort.Slice(normalized.Scenes, func(i, j int) bool {
		return fmt.Sprint(normalized.Scenes[i]["id"]) < fmt.Sprint(normalized.Scenes[j]["id"])
	})
	sort.Slice(normalized.Devices, func(i, j int) bool {
		return fmt.Sprint(normalized.Devices[i]["id"]) < fmt.Sprint(normalized.Devices[j]["id"])
	})
	return normalized
}

func mapLookup(item map[string]any, keys ...string) any {
	var current any = item
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

func cloneLookupTerms(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(fmt.Sprint(item))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}
