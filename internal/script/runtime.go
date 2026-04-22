package script

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	LanguageJavaScript = "javascript"
	RuntimeGoja        = "goja"
)

type DeviceService interface {
	ListDevices(ctx context.Context) ([]map[string]any, error)
	ListScenes(ctx context.Context) ([]map[string]any, error)
	GetLiveDeviceByID(ctx context.Context, id string) (map[string]any, error)
	GetLiveDeviceByName(ctx context.Context, name string) (map[string]any, error)
	ListLiveDevicesByRoom(ctx context.Context, roomName string) ([]map[string]any, error)
	ListLiveDevicesByType(ctx context.Context, deviceType string) ([]map[string]any, error)
	ListLiveDevicesByName(ctx context.Context, name string) ([]map[string]any, error)
	SetAttributes(ctx context.Context, id string, attrs map[string]any) error
	SetAttributesByName(ctx context.Context, name string, attrs map[string]any) error
	SetRoomAttributes(ctx context.Context, roomName string, attrs map[string]any) error
	TriggerScene(ctx context.Context, id string) error
	TriggerSceneByName(ctx context.Context, name string) error
	DeleteDevice(ctx context.Context, id string) error
	DeleteScene(ctx context.Context, id string) error
}

type SpeakerService interface {
	ListSpeakers(ctx context.Context) ([]map[string]any, error)
	PlaySoundByID(ctx context.Context, id, sound string) error
	PlaySoundByName(ctx context.Context, name, sound string) error
	PlayTestByID(ctx context.Context, id string) error
	PlayTestByName(ctx context.Context, name string) error
}

type StateStore interface {
	Snapshot() (map[string]any, error)
	Get(key string) (any, bool)
	Set(key string, value any) error
	Delete(key string) error
}

type HTTPClient interface {
	Do(ctx context.Context, method, url string, body any, headers map[string]string) (map[string]any, error)
}

type Logger interface {
	Printf(format string, args ...any)
}

type ExecContext struct {
	Devices          DeviceService
	DevicesSnapshot  []map[string]any
	ScenesSnapshot   []map[string]any
	Speakers         SpeakerService
	SpeakersSnapshot []map[string]any
	State            StateStore
	HTTP             HTTPClient
	Logger           Logger
	Now              func() time.Time
}

type Result struct {
	Output  any      `json:"output,omitempty"`
	Logs    []string `json:"logs,omitempty"`
	Updated bool     `json:"updated,omitempty"`
}

type Engine interface {
	Validate(doc Document) error
	Execute(ctx context.Context, execCtx ExecContext, doc Document) (*Result, error)
}

func NewEngine(doc Document) (Engine, error) {
	switch normalizedLanguage(doc.Language) {
	case LanguageJavaScript:
		return JavaScriptEngine{}, nil
	default:
		return nil, fmt.Errorf("unsupported script language %q", doc.Language)
	}
}

func ValidateDocument(doc Document) error {
	if strings.TrimSpace(doc.Source) == "" {
		return errors.New("script source is required")
	}
	if normalizedLanguage(doc.Language) == "" {
		return errors.New("script language is required")
	}
	return nil
}

func normalizedLanguage(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}
