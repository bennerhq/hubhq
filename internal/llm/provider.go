package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jbenner/hubhq/internal/autos"
	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/script"
)

type ActionPlan struct {
	Mode                 string          `json:"mode"`
	Summary              string          `json:"summary"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
	Destructive          bool            `json:"destructive"`
	Actions              []PlannedAction `json:"actions,omitempty"`
}

type PlannedAction struct {
	Type       string         `json:"type"`
	TargetID   string         `json:"target_id,omitempty"`
	TargetName string         `json:"target_name,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Value      any            `json:"value,omitempty"`
}

type AskScript struct {
	Mode                 string          `json:"mode"`
	Summary              string          `json:"summary"`
	RequiresConfirmation bool            `json:"requires_confirmation"`
	Destructive          bool            `json:"destructive"`
	Notes                []string        `json:"notes,omitempty"`
	Response             any             `json:"response,omitempty"`
	Actions              []PlannedAction `json:"actions,omitempty"`
	Script               script.Document `json:"script"`
}

type AutomationScript struct {
	Summary      string            `json:"summary"`
	Trigger      autos.Trigger     `json:"trigger"`
	Conditions   []autos.Condition `json:"conditions,omitempty"`
	Script       script.Document   `json:"script"`
	Notes        []string          `json:"notes,omitempty"`
	ApplySupport string            `json:"apply_support"`
}

type HomeContext struct {
	HubHost  string           `json:"hub_host"`
	Devices  []map[string]any `json:"devices"`
	Scenes   []map[string]any `json:"scenes"`
	Snapshot map[string]any   `json:"snapshot,omitempty"`
}

type Provider interface {
	GenerateActionPlan(ctx context.Context, prompt string, home HomeContext) (*ActionPlan, error)
	GenerateAutomationSpec(ctx context.Context, prompt string, home HomeContext) (*autos.Spec, error)
	GenerateAskScript(ctx context.Context, prompt string, home HomeContext) (*AskScript, error)
	GenerateAutomationScript(ctx context.Context, prompt string, home HomeContext) (*AutomationScript, error)
}

type DebugLogger interface {
	SetDebugLogger(func(string, ...any))
}

func NewProvider(cfg config.LLMConfig) (Provider, error) {
	resolved := resolveConfig(cfg)
	if resolved.Provider == "" {
		resolved.Provider = "openai-compatible"
	}
	if resolved.BaseURL == "" {
		resolved.BaseURL = "https://api.openai.com/v1"
	}
	if resolved.Model == "" {
		resolved.Model = "gpt-4.1-mini"
	}
	if resolved.APIKey == "" {
		return nil, errors.New("missing LLM API key; set `DIRIGERA_LLM_API_KEY` or save `llm.api_key` in config")
	}

	switch strings.ToLower(resolved.Provider) {
	case "openai", "openai-compatible":
		return NewOpenAICompatibleProvider(resolved), nil
	default:
		return nil, fmt.Errorf("unsupported LLM provider %q", resolved.Provider)
	}
}

func resolveConfig(cfg config.LLMConfig) config.LLMConfig {
	out := cfg
	if value := os.Getenv("DIRIGERA_LLM_PROVIDER"); value != "" {
		out.Provider = value
	}
	if value := os.Getenv("DIRIGERA_LLM_BASE_URL"); value != "" {
		out.BaseURL = value
	}
	if value := os.Getenv("DIRIGERA_LLM_MODEL"); value != "" {
		out.Model = value
	}
	if value := os.Getenv("DIRIGERA_LLM_TIMEOUT_SECONDS"); value != "" {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			out.TimeoutSeconds = parsed
		}
	}
	if value := os.Getenv("DIRIGERA_LLM_API_KEY"); value != "" {
		out.APIKey = value
	}
	if out.APIKey == "" {
		if value := os.Getenv("OPENAI_API_KEY"); value != "" {
			out.APIKey = value
		}
	}
	return out
}
