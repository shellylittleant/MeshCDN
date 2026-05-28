// Package ai defines the LLM provider interface and implementations.
//
// Per V4-DESIGN philosophy:
//   - AI is advisory, not authoritative — it suggests commands, the user executes
//   - Multi-provider support (OpenAI / Gemini / Grok / Claude / DeepSeek / Qwen)
//   - V4.0.18: all six providers are wired.
//
// Architecture:
//
//	type Provider interface { Chat(ctx, messages) (response, error) }
//
//	openAICompatibleProvider — generic base for any backend exposing the
//	  OpenAI v1/chat/completions request/response shape (openai, gemini,
//	  deepseek, grok, qwen). Each provider has its own thin file selecting
//	  the endpoint URL.
//	claudeProvider — Anthropic Messages API (different request/response
//	  shape: top-level system field, content blocks, x-api-key header).
//
// The bot doesn't talk to providers directly — it uses an Assistant which
// holds conversation history per user and a Provider.
package ai

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Role for chat messages.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message in a chat conversation.
type Message struct {
	Role    string
	Content string
}

// Provider is what every LLM backend implements.
type Provider interface {
	// Name returns the provider's canonical identifier ("openai", "gemini", ...)
	Name() string

	// Chat sends a conversation and returns the assistant's response.
	Chat(ctx context.Context, messages []Message) (string, error)
}

// ─────────────────────────────────────────────────────────────────────
// Assistant — the bot's wrapper around a Provider
// ─────────────────────────────────────────────────────────────────────

// Assistant manages per-user conversation state and dispatches to a Provider.
//
// Per the user requirement:
//   - Each @mention starts a fresh conversation
//   - Replies to bot messages within an active conversation continue it
//   - Conversations expire after 30 minutes of inactivity
type Assistant struct {
	Provider     Provider
	SystemPrompt string

	mu    sync.Mutex
	convs map[int64]*conversation // userID → conv
}

type conversation struct {
	messages []Message
	updated  time.Time
}

const convTTL = 30 * time.Minute

// NewAssistant returns a new Assistant.
func NewAssistant(provider Provider, systemPrompt string) *Assistant {
	return &Assistant{
		Provider:     provider,
		SystemPrompt: systemPrompt,
		convs:        make(map[int64]*conversation),
	}
}

// Start begins a fresh conversation for userID, discarding any previous state.
// Returns the assistant's response to the first user message.
func (a *Assistant) Start(ctx context.Context, userID int64, userMsg string) (string, error) {
	a.mu.Lock()
	a.convs[userID] = &conversation{
		messages: []Message{
			{Role: RoleSystem, Content: a.SystemPrompt},
			{Role: RoleUser, Content: userMsg},
		},
		updated: time.Now(),
	}
	conv := a.convs[userID]
	a.mu.Unlock()

	resp, err := a.Provider.Chat(ctx, conv.messages)
	if err != nil {
		// Don't store the failed exchange; let user retry
		a.mu.Lock()
		delete(a.convs, userID)
		a.mu.Unlock()
		return "", err
	}

	a.mu.Lock()
	conv.messages = append(conv.messages, Message{Role: RoleAssistant, Content: resp})
	conv.updated = time.Now()
	a.mu.Unlock()
	return resp, nil
}

// Continue adds a follow-up user message to an existing conversation.
// Returns ErrNoConversation if there's no active conversation for userID.
func (a *Assistant) Continue(ctx context.Context, userID int64, userMsg string) (string, error) {
	a.mu.Lock()
	conv, ok := a.convs[userID]
	if !ok {
		a.mu.Unlock()
		return "", ErrNoConversation
	}
	if time.Since(conv.updated) > convTTL {
		delete(a.convs, userID)
		a.mu.Unlock()
		return "", ErrConvExpired
	}
	conv.messages = append(conv.messages, Message{Role: RoleUser, Content: userMsg})
	a.mu.Unlock()

	resp, err := a.Provider.Chat(ctx, conv.messages)
	if err != nil {
		// Roll back the user message we just appended
		a.mu.Lock()
		if len(conv.messages) > 0 && conv.messages[len(conv.messages)-1].Role == RoleUser {
			conv.messages = conv.messages[:len(conv.messages)-1]
		}
		a.mu.Unlock()
		return "", err
	}

	a.mu.Lock()
	conv.messages = append(conv.messages, Message{Role: RoleAssistant, Content: resp})
	conv.updated = time.Now()
	a.mu.Unlock()
	return resp, nil
}

// HasActiveConv reports whether userID has a non-expired conversation.
func (a *Assistant) HasActiveConv(userID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	conv, ok := a.convs[userID]
	if !ok {
		return false
	}
	return time.Since(conv.updated) <= convTTL
}

// Reset clears all state for userID (e.g. when a new @mention arrives).
func (a *Assistant) Reset(userID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.convs, userID)
}

// Sweep removes expired conversations; called periodically by serve.go.
func (a *Assistant) Sweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-convTTL)
	for uid, c := range a.convs {
		if c.updated.Before(cutoff) {
			delete(a.convs, uid)
		}
	}
}

var (
	ErrNoConversation = fmt.Errorf("no active conversation for this user")
	ErrConvExpired    = fmt.Errorf("conversation expired (>30 min idle); please @mention me again")
)

// ─────────────────────────────────────────────────────────────────────
// Provider factory
// ─────────────────────────────────────────────────────────────────────

// NewProvider returns a Provider matching the named backend with the given
// API key + model. Unknown providers return an error.
//
// V4.0.18: all six reserved providers are wired (openai / gemini / claude /
// deepseek / grok / qwen). Gemini uses Google's OpenAI-compatible endpoint.
//
// model="" → fall back to identity.DefaultModel(name). We don't import
// identity here (would create a dep cycle for tests); the caller in
// cli/serve.go passes a non-empty model whenever identity has one set,
// so the fallback here is only hit by direct callers.
func NewProvider(name, apiKey, model string) (Provider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key for %s is empty", name)
	}
	switch name {
	case "openai":
		if model == "" {
			model = "gpt-4o-mini"
		}
		return newOpenAIProvider(apiKey, model), nil
	case "gemini":
		if model == "" {
			model = "gemini-2.5-flash-lite"
		}
		return newGeminiProvider(apiKey, model), nil
	case "claude":
		if model == "" {
			model = "claude-3-5-haiku-20241022"
		}
		return newClaudeProvider(apiKey, model), nil
	case "deepseek":
		if model == "" {
			model = "deepseek-chat"
		}
		return newDeepSeekProvider(apiKey, model), nil
	case "grok":
		if model == "" {
			model = "grok-beta"
		}
		return newGrokProvider(apiKey, model), nil
	case "qwen":
		if model == "" {
			model = "qwen-turbo"
		}
		return newQwenProvider(apiKey, model), nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}
