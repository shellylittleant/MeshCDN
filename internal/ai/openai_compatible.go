// OpenAI-compatible provider — base implementation reused by every backend
// that exposes the OpenAI v1/chat/completions request/response shape.
//
// Concrete providers (openai / deepseek / grok / qwen) live in their own
// files and are just thin wrappers that pick a Name + endpoint URL. Claude
// is NOT in this set — Anthropic's API has a different shape; see claude.go.
//
// We pull this out as soon as we have a second OpenAI-compatible backend
// rather than copy-pasting the HTTP plumbing; per V4 root-cause principle.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAICompatibleProvider implements ai.Provider against any backend that
// accepts a POST to <endpoint> with OpenAI-style {model, messages, ...} body
// and returns {choices[0].message.content} responses.
//
// Set name to the provider's canonical identifier so error messages and
// Provider.Name() are informative.
type openAICompatibleProvider struct {
	name     string
	apiKey   string
	model    string
	endpoint string
}

func (p *openAICompatibleProvider) Name() string { return p.name }

func (p *openAICompatibleProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	type apiMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	apiMsgs := make([]apiMsg, len(messages))
	for i, m := range messages {
		apiMsgs[i] = apiMsg{Role: m.Role, Content: m.Content}
	}

	reqBody := map[string]interface{}{
		"model":    p.model,
		"messages": apiMsgs,
		// Modest temperature: we want some flexibility but mostly deterministic
		// command suggestions.
		"temperature": 0.3,
		// Cap response length — most replies should be short.
		"max_tokens": 800,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint,
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("%s request: %w", p.name, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to extract error message from JSON body.
		// All four OpenAI-compatible providers follow the same {error: {message}}
		// shape so we can use one decoder.
		var errResp struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(respBytes, &errResp); err == nil && errResp.Error.Message != "" {
			return "", fmt.Errorf("%s HTTP %d: %s", p.name, resp.StatusCode, errResp.Error.Message)
		}
		// Fallback: raw body (truncated)
		s := string(respBytes)
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return "", fmt.Errorf("%s HTTP %d: %s", p.name, resp.StatusCode, s)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("%s returned no choices", p.name)
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
