// Package identity owns persistent/identity.json — node identity + cluster
// credentials + AI provider keys + AI provider models.
//
// Per V4-DESIGN §A.1: this is one of the four files preserved across upgrades.
//
// File contents:
//
//	{
//	  "node_ip":   "1.2.3.4",
//	  "bot_token": "1234567890:ABC...",
//	  "group_id":  -1001234567890,
//	  "joined_at": "2026-04-27T12:34:56Z",
//
//	  "ai_provider": "openai",
//
//	  "openai_api_key":   "sk-...",
//	  "openai_model":     "gpt-4o-mini",
//	  "gemini_api_key":   "AIza...",
//	  "gemini_model":     "gemini-2.5-flash-lite",
//	  "grok_api_key":     "",
//	  "grok_model":       "",
//	  "claude_api_key":   "",
//	  "claude_model":     "",
//	  "deepseek_api_key": "",
//	  "deepseek_model":   "",
//	  "qwen_api_key":     "",
//	  "qwen_model":       ""
//	}
//
// V4.0.18 change: each provider has its own model field. Earlier versions
// had a single global ai_model field, which caused the model to "stick"
// to the wrong provider when switching. See AIActiveModel for the lookup
// path: it returns the active provider's specific model, or the registered
// default if unset.
//
// The legacy ai_model field is preserved in the struct for read-compat but
// is cleared at Load time and never written.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Identity struct {
	NodeIP   string    `json:"node_ip"`
	BotToken string    `json:"bot_token"`
	GroupID  int64     `json:"group_id"`
	JoinedAt time.Time `json:"joined_at"`

	// AI configuration
	AIProvider string `json:"ai_provider,omitempty"` // "openai" / "gemini" / "grok" / "claude" / "deepseek" / "qwen"

	// DEPRECATED in v4.0.18: replaced by per-provider model fields below.
	// Kept for read-compat; cleared on Load and never written. Field auto-
	// disappears from on-disk JSON after the first config change.
	AIModel string `json:"ai_model,omitempty"`

	// Per-provider API keys (only the active provider's key is used)
	OpenAIKey   string `json:"openai_api_key,omitempty"`
	GeminiKey   string `json:"gemini_api_key,omitempty"`
	GrokKey     string `json:"grok_api_key,omitempty"`
	ClaudeKey   string `json:"claude_api_key,omitempty"`
	DeepSeekKey string `json:"deepseek_api_key,omitempty"`
	QwenKey     string `json:"qwen_api_key,omitempty"`

	// Per-provider model overrides. Empty → fall back to DefaultModel(provider).
	OpenAIModel   string `json:"openai_model,omitempty"`
	GeminiModel   string `json:"gemini_model,omitempty"`
	GrokModel     string `json:"grok_model,omitempty"`
	ClaudeModel   string `json:"claude_model,omitempty"`
	DeepSeekModel string `json:"deepseek_model,omitempty"`
	QwenModel     string `json:"qwen_model,omitempty"`
}

const DefaultPath = "/etc/meshcdn/persistent/identity.json"

func Path() string {
	if p := os.Getenv("MESHCDN_IDENTITY_PATH"); p != "" {
		return p
	}
	return DefaultPath
}

func Load() (*Identity, error) { return LoadFrom(Path()) }

func LoadFrom(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity %s: %w", path, err)
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("parse identity %s: %w", path, err)
	}
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("invalid identity in %s: %w", path, err)
	}
	// V4.0.18: the legacy global ai_model field is no longer trusted —
	// it could carry a stale value from before per-provider models existed
	// (e.g. ai_provider=openai but ai_model=qwen-turbo because the user
	// switched providers under the v4.0.17 bug). Clear it; AIActiveModel
	// will fall back to the per-provider field or registered default.
	// The field disappears from disk on next Save via omitempty.
	id.AIModel = ""
	return &id, nil
}

func (id *Identity) Save() error { return id.SaveTo(Path()) }

func (id *Identity) SaveTo(path string) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func (id *Identity) Validate() error {
	if id.NodeIP == "" {
		return errors.New("node_ip is empty")
	}
	if net.ParseIP(id.NodeIP) == nil {
		return fmt.Errorf("node_ip %q is not a valid IP", id.NodeIP)
	}
	if id.BotToken == "" {
		return errors.New("bot_token is empty")
	}
	if id.GroupID == 0 {
		return errors.New("group_id is 0")
	}
	return nil
}

func (id *Identity) Secret() string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(id.GroupID, 10)))
	h.Write([]byte(id.BotToken))
	return hex.EncodeToString(h.Sum(nil))
}

func New(nodeIP, botToken string, groupID int64) *Identity {
	return &Identity{
		NodeIP:   nodeIP,
		BotToken: botToken,
		GroupID:  groupID,
		JoinedAt: time.Now().UTC(),
	}
}

// ─────────────────────────────────────────────────────────────────────
// AI helpers
// ─────────────────────────────────────────────────────────────────────

// AIConfigured reports whether AI is enabled and has the necessary key.
func (id *Identity) AIConfigured() bool {
	return id.AIProvider != "" && id.GetAPIKey(id.AIProvider) != ""
}

// GetAPIKey returns the API key for the given provider name, or empty if unset.
func (id *Identity) GetAPIKey(provider string) string {
	switch provider {
	case "openai":
		return id.OpenAIKey
	case "gemini":
		return id.GeminiKey
	case "grok":
		return id.GrokKey
	case "claude":
		return id.ClaudeKey
	case "deepseek":
		return id.DeepSeekKey
	case "qwen":
		return id.QwenKey
	}
	return ""
}

// SetAPIKey stores the API key for the given provider.
func (id *Identity) SetAPIKey(provider, key string) error {
	switch provider {
	case "openai":
		id.OpenAIKey = key
	case "gemini":
		id.GeminiKey = key
	case "grok":
		id.GrokKey = key
	case "claude":
		id.ClaudeKey = key
	case "deepseek":
		id.DeepSeekKey = key
	case "qwen":
		id.QwenKey = key
	default:
		return fmt.Errorf("unknown AI provider %q (want: openai/gemini/grok/claude/deepseek/qwen)", provider)
	}
	return nil
}

// GetModel returns the per-provider model override for the given provider,
// or empty if no override is set. Empty means "use DefaultModel(provider)".
func (id *Identity) GetModel(provider string) string {
	switch provider {
	case "openai":
		return id.OpenAIModel
	case "gemini":
		return id.GeminiModel
	case "grok":
		return id.GrokModel
	case "claude":
		return id.ClaudeModel
	case "deepseek":
		return id.DeepSeekModel
	case "qwen":
		return id.QwenModel
	}
	return ""
}

// SetModel stores the per-provider model override for the given provider.
func (id *Identity) SetModel(provider, model string) error {
	switch provider {
	case "openai":
		id.OpenAIModel = model
	case "gemini":
		id.GeminiModel = model
	case "grok":
		id.GrokModel = model
	case "claude":
		id.ClaudeModel = model
	case "deepseek":
		id.DeepSeekModel = model
	case "qwen":
		id.QwenModel = model
	default:
		return fmt.Errorf("unknown AI provider %q (want: openai/gemini/grok/claude/deepseek/qwen)", provider)
	}
	return nil
}

// AIActiveModel returns the model name that will actually be used for the
// currently active provider: the per-provider override if set, otherwise
// the registered default. Returns empty string only if AIProvider is unset
// or unknown.
//
// This is the single source of truth callers should consult — they should
// NOT read id.AIModel (deprecated) or id.GetModel(...) directly.
func (id *Identity) AIActiveModel() string {
	if id.AIProvider == "" {
		return ""
	}
	if m := id.GetModel(id.AIProvider); m != "" {
		return m
	}
	return DefaultModel(id.AIProvider)
}

// DefaultModel returns the default model name for a provider.
//
// These are the cheapest/fastest models in each family that are still strong
// enough for the natural-language → command translation task this bot uses
// AI for. Operators can override with /w ai model <provider>:<model>.
//
// As of v4.0.18 these names reflect each provider's current naming as of
// 2026; older names like gemini-1.5-flash are deprecated upstream and have
// been replaced.
func DefaultModel(provider string) string {
	switch provider {
	case "openai":
		return "gpt-4o-mini"
	case "gemini":
		return "gemini-2.5-flash-lite"
	case "grok":
		return "grok-beta"
	case "claude":
		return "claude-3-5-haiku-20241022"
	case "deepseek":
		return "deepseek-chat"
	case "qwen":
		return "qwen-turbo"
	}
	return ""
}
