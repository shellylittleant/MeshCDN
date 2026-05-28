// Grok (xAI) provider — thin wrapper over openAICompatibleProvider.
//
// xAI exposes an OpenAI-compatible chat completions endpoint.
// Default model: grok-beta (per identity.DefaultModel — operators should
// override with /w ai model grok:<current-model> as xAI's model names
// evolve).
//
// Endpoint reference: https://api.x.ai/v1/chat/completions
// Auth: Bearer <key>
package ai

const grokEndpoint = "https://api.x.ai/v1/chat/completions"

func newGrokProvider(apiKey, model string) Provider {
	return &openAICompatibleProvider{
		name:     "grok",
		apiKey:   apiKey,
		model:    model,
		endpoint: grokEndpoint,
	}
}
