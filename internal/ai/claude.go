// Claude (Anthropic) provider.
//
// Anthropic's Messages API has a different shape from the OpenAI ecosystem,
// so this provider implements ai.Provider directly rather than reusing the
// openAICompatibleProvider base.
//
// Key shape differences vs OpenAI:
//   - System prompt is a top-level "system" field, NOT a message with
//     role=system. We extract it from the incoming []Message before sending.
//   - "max_tokens" is REQUIRED (not optional like OpenAI).
//   - Authentication uses "x-api-key" + "anthropic-version" headers, not
//     a bearer token.
//   - Response payload uses content blocks: response.content[0].text rather
//     than choices[0].message.content.
//
// Endpoint reference: https://api.anthropic.com/v1/messages
// API version pin: 2023-06-01 (long-standing stable revision; bump only
// when we need a newer feature).
//
// Default model: claude-3-5-haiku-20241022 (per identity.DefaultModel —
// the cheapest, fastest Claude model that's still strong on the
// natural-language → command translation task we use AI for).
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

const (
	claudeEndpoint   = "https://api.anthropic.com/v1/messages"
	claudeAPIVersion = "2023-06-01"
	claudeMaxTokens  = 800
)

type claudeProvider struct {
	apiKey string
	model  string
}

func newClaudeProvider(apiKey, model string) Provider {
	return &claudeProvider{apiKey: apiKey, model: model}
}

func (p *claudeProvider) Name() string { return "claude" }

func (p *claudeProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	// Split out system message (Anthropic puts it top-level, not in messages).
	type apiMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var system string
	apiMsgs := make([]apiMsg, 0, len(messages))
	for _, m := range messages {
		if m.Role == RoleSystem {
			// If multiple system messages appear, concatenate. Our Assistant
			// puts exactly one at conversation start, so this is defensive.
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		apiMsgs = append(apiMsgs, apiMsg{Role: m.Role, Content: m.Content})
	}
	if len(apiMsgs) == 0 {
		return "", fmt.Errorf("claude: no non-system messages to send")
	}

	reqBody := map[string]interface{}{
		"model":       p.model,
		"max_tokens":  claudeMaxTokens, // REQUIRED by Anthropic
		"messages":    apiMsgs,
		"temperature": 0.3,
	}
	if system != "" {
		reqBody["system"] = system
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeEndpoint,
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("claude request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(respBytes, &errResp); err == nil && errResp.Error.Message != "" {
			return "", fmt.Errorf("claude HTTP %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		s := string(respBytes)
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return "", fmt.Errorf("claude HTTP %d: %s", resp.StatusCode, s)
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Content) == 0 {
		return "", fmt.Errorf("claude returned no content (stop_reason=%s)", parsed.StopReason)
	}
	// Concatenate all text blocks in order. In practice Claude returns one,
	// but tool-use responses can interleave; we only want the text.
	var out strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	result := strings.TrimSpace(out.String())
	if result == "" {
		return "", fmt.Errorf("claude returned no text blocks (stop_reason=%s)", parsed.StopReason)
	}
	return result, nil
}
