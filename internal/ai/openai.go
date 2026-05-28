// OpenAI provider — thin wrapper over openAICompatibleProvider.
//
// Endpoint: https://api.openai.com/v1/chat/completions
//
// V4 default model: gpt-4o-mini (per design judgement: cheap + fast +
// good enough for natural-language → command translation).
package ai

const openAIEndpoint = "https://api.openai.com/v1/chat/completions"

func newOpenAIProvider(apiKey, model string) Provider {
	return &openAICompatibleProvider{
		name:     "openai",
		apiKey:   apiKey,
		model:    model,
		endpoint: openAIEndpoint,
	}
}
