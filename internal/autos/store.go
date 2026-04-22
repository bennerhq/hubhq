package autos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/script"
)

type Trigger struct {
	Kind         string   `json:"kind"`
	At           string   `json:"at,omitempty"`
	SunEvent     string   `json:"sun_event,omitempty"`
	Offset       string   `json:"offset,omitempty"`
	Cron         string   `json:"cron,omitempty"`
	TimeZone     string   `json:"time_zone,omitempty"`
	EventName    string   `json:"event_name,omitempty"`
	DeviceID     string   `json:"device_id,omitempty"`
	DeviceIDs    []string `json:"device_ids,omitempty"`
	DeviceName   string   `json:"device_name,omitempty"`
	DeviceNames  []string `json:"device_names,omitempty"`
	Attribute    string   `json:"attribute,omitempty"`
	Value        any      `json:"value,omitempty"`
	Notification string   `json:"notification,omitempty"`
}

type Condition struct {
	Type     string `json:"type"`
	Field    string `json:"field,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    any    `json:"value,omitempty"`
}

type Action struct {
	Type       string         `json:"type"`
	TargetID   string         `json:"target_id,omitempty"`
	TargetName string         `json:"target_name,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	ClipName   string         `json:"clip_name,omitempty"`
	Volume     int            `json:"volume,omitempty"`
}

type Spec struct {
	ID            string          `json:"id"`
	Prompt        string          `json:"prompt"`
	Summary       string          `json:"summary"`
	Trigger       Trigger         `json:"trigger"`
	Conditions    []Condition     `json:"conditions,omitempty"`
	Actions       []Action        `json:"actions"`
	Script        script.Document `json:"script,omitempty"`
	Notes         []string        `json:"notes,omitempty"`
	Status        string          `json:"status"`
	ApplySupport  string          `json:"apply_support"`
	CreatedAt     time.Time       `json:"created_at"`
	LastUpdatedAt time.Time       `json:"last_updated_at"`
}

type Store struct {
	Specs []Spec `json:"specs"`
}

func Load() (*Store, string, error) {
	dir, err := config.AppDir()
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "autos.json")
	store := &Store{}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, path, nil
		}
		return nil, "", fmt.Errorf("read autos: %w", err)
	}
	if err := json.Unmarshal(data, store); err != nil {
		return nil, "", fmt.Errorf("parse autos: %w", err)
	}
	return store, path, nil
}

func Save(store *Store) error {
	dir, err := config.AppDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal autos: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "autos.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write autos: %w", err)
	}
	return nil
}

func NewID() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Store) SortedSpecs() []Spec {
	out := append([]Spec(nil), s.Specs...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *Store) Upsert(spec Spec) {
	for i := range s.Specs {
		if s.Specs[i].ID == spec.ID {
			s.Specs[i] = spec
			return
		}
	}
	s.Specs = append(s.Specs, spec)
}

func (s *Store) Find(id string) (Spec, bool) {
	for _, spec := range s.Specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return Spec{}, false
}

func (s *Store) Delete(id string) bool {
	for i, spec := range s.Specs {
		if spec.ID == id {
			s.Specs = append(s.Specs[:i], s.Specs[i+1:]...)
			return true
		}
	}
	return false
}
