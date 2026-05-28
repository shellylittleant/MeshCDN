// DeepSeek provider — thin wrapper over openAICompatibleProvider.
//
// DeepSeek exposes an OpenAI-compatible chat completions endpoint.
// Default model: deepseek-chat (per identity.DefaultModel).
//
// Endpoint reference: https://api.deepseek.com/v1/chat/completions
// Auth: Bearer <key>
package ai

const deepseekEndpoint = "https://api.deepseek.com/v1/chat/completions"

func newDeepSeekProvider(apiKey, model string) Provider {
	return &openAICompatibleProvider{
		name:     "deepseek",
		apiKey:   apiKey,
		model:    model,
		endpoint: deepseekEndpoint,
	}
}
