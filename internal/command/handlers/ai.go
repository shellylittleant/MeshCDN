// AI configuration handler.
//
// Per V4-DESIGN §8 strict 4-segment form:
//
//	/w ai key   <key-string>     → set API key for current provider
//	/w ai key   <provider>:<key> → set API key for specific provider (openai/gemini/...)
//	/w ai model <model-name>     → set model
//	/w ai model <provider>:<model>
//	/w ai provider <name>        → switch active provider
//
//	/d ai - -                    → disable AI (clear ai_provider, leave keys intact)
//	/d ai key <provider>         → clear key for one provider
//
//	/v ai - -                    → show current AI config (key shown as masked)
//
// We use `scope = "key"|"model"|"provider"` (the "operation"), and params
// holds the value. This matches the `/w internal peer-add ...` style we
// already use for namespaced commands.
package handlers

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/example/meshcdn/internal/ai"
	"github.com/example/meshcdn/internal/command"
	"github.com/example/meshcdn/internal/identity"
)

// AIHandler manipulates identity.json's AI fields.
type AIHandler struct {
	// IdentityPath: where to read/write identity.json. Default: identity.Path()
	IdentityPath string
}

func (h *AIHandler) Type() string { return "ai" }

func (h *AIHandler) PrimaryKey(scope, paramsText string) (string, error) {
	return "ai:" + scope, nil
}

func (h *AIHandler) Validate(cmd *command.Command) error {
	switch cmd.Verb {
	case command.VerbView:
		// /v ai - -  is the only valid form
		if !command.IsPlaceholder(cmd.Scope) {
			return command.NewError(command.ErrBadFormat, "/v ai requires scope=-")
		}
		return nil

	case command.VerbWrite:
		switch cmd.Scope {
		case "key", "model", "provider":
			if command.IsPlaceholder(cmd.Params) || cmd.Params == "" {
				return command.NewError(command.ErrBadParams,
					fmt.Sprintf("/w ai %s requires a value", cmd.Scope))
			}
			return nil
		default:
			return command.NewError(command.ErrBadFormat,
				"/w ai scope must be: key | model | provider")
		}

	case command.VerbDelete:
		// /d ai - -        → disable AI globally (clear ai_provider)
		// /d ai key <prov> → clear key for one provider
		if command.IsPlaceholder(cmd.Scope) {
			return nil
		}
		if cmd.Scope == "key" {
			return nil
		}
		return command.NewError(command.ErrBadFormat,
			"/d ai scope must be - or key")
	}
	return command.NewError(command.ErrBadFormat, "unknown verb")
}

func (h *AIHandler) Write(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	id, err := h.loadIdentity()
	if err != nil {
		return command.Effects{}, err
	}

	switch cmd.Scope {
	case "provider":
		provider := strings.TrimSpace(cmd.Params)
		if !isKnownProvider(provider) {
			return command.Effects{}, command.NewError(command.ErrBadParams,
				fmt.Sprintf("provider must be: openai/gemini/grok/claude/deepseek/qwen, got %q", provider))
		}
		id.AIProvider = provider
		// V4.0.18: do NOT touch any model field here. Each provider has its
		// own per-provider model override; id.AIActiveModel() resolves the
		// override-or-default at display and call time.
		if err := id.Save(); err != nil {
			return command.Effects{}, err
		}
		note := ""
		if !isImplementedProvider(provider) {
			note = "\n注意: " + provider + " 接口暂未实现; 请用其他 provider 或等待后续版本"
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("✅ AI provider 设为 %s (model: %s)%s",
				provider, id.AIActiveModel(), note),
		}, nil

	case "model":
		// "<provider>:<model>" or just "<model>" (apply to current provider)
		val := strings.TrimSpace(cmd.Params)
		var provider, model string
		if colon := strings.Index(val, ":"); colon > 0 {
			provider = val[:colon]
			model = val[colon+1:]
			if !isKnownProvider(provider) {
				return command.Effects{}, command.NewError(command.ErrBadParams,
					fmt.Sprintf("unknown provider %q", provider))
			}
		} else {
			provider = id.AIProvider
			model = val
			if provider == "" {
				return command.Effects{}, command.NewError(command.ErrBadFormat,
					"no AI provider set; use /w ai provider <name> first")
			}
		}
		// V4.0.18: per-provider model storage. Setting qwen's model no longer
		// affects openai's model.
		if err := id.SetModel(provider, model); err != nil {
			return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
		}
		if err := id.Save(); err != nil {
			return command.Effects{}, err
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("✅ %s model 设为 %s", provider, model),
		}, nil

	case "key":
		// "<provider>:<key>" or just "<key>" (apply to current provider)
		val := strings.TrimSpace(cmd.Params)
		var provider, key string
		if colon := strings.Index(val, ":"); colon > 0 {
			provider = val[:colon]
			key = val[colon+1:]
			if !isKnownProvider(provider) {
				return command.Effects{}, command.NewError(command.ErrBadParams,
					fmt.Sprintf("unknown provider %q", provider))
			}
		} else {
			provider = id.AIProvider
			key = val
			if provider == "" {
				// First-time setup: assume OpenAI if no provider configured.
				// Model resolves via AIActiveModel → DefaultModel("openai").
				provider = "openai"
				id.AIProvider = "openai"
			}
		}

		if err := id.SetAPIKey(provider, key); err != nil {
			return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
		}
		if err := id.Save(); err != nil {
			return command.Effects{}, err
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("✅ %s API key 已保存 (前 8 位: %s***)",
				provider, maskKey(key)),
		}, nil
	}
	return command.Effects{}, command.NewError(command.ErrBadFormat, "unreachable")
}

func (h *AIHandler) Delete(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	id, err := h.loadIdentity()
	if err != nil {
		return command.Effects{}, err
	}

	if command.IsPlaceholder(cmd.Scope) {
		// /d ai - -  → disable AI by clearing provider (keys + per-provider
		// models preserved, so re-enabling restores prior model choice).
		oldProvider := id.AIProvider
		id.AIProvider = ""
		// id.AIModel is deprecated; LoadFrom already clears it. No-op here.
		if err := id.Save(); err != nil {
			return command.Effects{}, err
		}
		if oldProvider != "" {
			return command.Effects{
				UserMessage: fmt.Sprintf("✅ AI 已禁用 (之前是 %s; 各 provider 的 key/model 保留，重新启用用 /w ai provider <name>)", oldProvider),
			}, nil
		}
		return command.Effects{
			UserMessage: "AI 本来就未启用",
		}, nil
	}

	// /d ai key <provider>
	if cmd.Scope == "key" {
		provider := strings.TrimSpace(cmd.Params)
		if command.IsPlaceholder(provider) || provider == "" {
			return command.Effects{}, command.NewError(command.ErrBadFormat,
				"/d ai key <provider> requires provider name")
		}
		if err := id.SetAPIKey(provider, ""); err != nil {
			return command.Effects{}, command.NewError(command.ErrBadParams, err.Error())
		}
		if err := id.Save(); err != nil {
			return command.Effects{}, err
		}
		return command.Effects{
			UserMessage: fmt.Sprintf("✅ %s API key 已清除", provider),
		}, nil
	}

	return command.Effects{}, command.NewError(command.ErrBadFormat, "unreachable")
}

func (h *AIHandler) View(tx *sql.Tx, cmd *command.Command) (command.Effects, error) {
	id, err := h.loadIdentity()
	if err != nil {
		return command.Effects{}, err
	}

	var sb strings.Builder
	sb.WriteString("🤖 AI 配置\n\n")

	if id.AIProvider == "" {
		sb.WriteString("  状态:        ⚠️  未启用\n")
	} else if !id.AIConfigured() {
		sb.WriteString(fmt.Sprintf("  状态:        ⚠️  provider 已选 (%s) 但 API key 未设置\n", id.AIProvider))
	} else {
		sb.WriteString(fmt.Sprintf("  状态:        ✅ 启用\n"))
		sb.WriteString(fmt.Sprintf("  当前 provider: %s\n", id.AIProvider))
		sb.WriteString(fmt.Sprintf("  当前 model:    %s\n", id.AIActiveModel()))
	}

	sb.WriteString("\n  各 provider 状态 (key | model):\n")
	for _, p := range []string{"openai", "gemini", "grok", "claude", "deepseek", "qwen"} {
		key := id.GetAPIKey(p)
		var keyStatus string
		if key == "" {
			keyStatus = "(未设置)"
		} else {
			keyStatus = maskKey(key) + "***"
		}
		modelStatus := id.GetModel(p)
		if modelStatus == "" {
			modelStatus = identity.DefaultModel(p) + " (默认)"
		}
		impl := ""
		if !isImplementedProvider(p) {
			impl = " [尚未实现]"
		}
		fmt.Fprintf(&sb, "    %-9s %-16s | %s%s\n", p+":", keyStatus, modelStatus, impl)
	}

	sb.WriteString("\n  常用命令:\n")
	sb.WriteString("    /w ai key sk-xxx                    设当前 provider 的 key\n")
	sb.WriteString("    /w ai key gemini:AIza...            设指定 provider 的 key\n")
	sb.WriteString("    /w ai provider gemini               切换 provider\n")
	sb.WriteString("    /w ai model gpt-4o                  改当前 provider 的模型\n")
	sb.WriteString("    /w ai model qwen:qwen-plus          改指定 provider 的模型\n")
	sb.WriteString("    /d ai key qwen                      清除某 provider 的 key\n")
	sb.WriteString("    /d ai - -                           禁用 AI (保留所有 key+model)\n")

	return command.Effects{UserMessage: sb.String()}, nil
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

func (h *AIHandler) loadIdentity() (*identity.Identity, error) {
	if h.IdentityPath != "" {
		return identity.LoadFrom(h.IdentityPath)
	}
	return identity.Load()
}

func isKnownProvider(name string) bool {
	switch name {
	case "openai", "gemini", "grok", "claude", "deepseek", "qwen":
		return true
	}
	return false
}

// isImplementedProvider reports whether ai.NewProvider can actually instantiate
// a working backend for this provider. As of V4.0.18, all six reserved
// providers are wired.
func isImplementedProvider(name string) bool {
	switch name {
	case "openai", "gemini", "claude", "grok", "deepseek", "qwen":
		return true
	}
	return false
}

func maskKey(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// silence unused
var _ = ai.RoleSystem
