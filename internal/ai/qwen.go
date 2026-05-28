// Qwen (Alibaba Tongyi) provider — thin wrapper over openAICompatibleProvider.
//
// Uses DashScope's OpenAI-compatible mode, which accepts the standard
// OpenAI chat completions request/response shape.
//
// Endpoint reference:
//
//	https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions
//
// Note: this is NOT the native DashScope endpoint (which lives at
// /api/v1/services/aigc/text-generation/generation and uses a different
// schema). The compatible-mode path is explicitly maintained by Alibaba
// for OpenAI-SDK interop.
//
// Auth: Bearer <DashScope API key, typically starts with "sk-">
// Default model: qwen-turbo (per identity.DefaultModel)
package ai

const qwenEndpoint = "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"

func newQwenProvider(apiKey, model string) Provider {
	return &openAICompatibleProvider{
		name:     "qwen",
		apiKey:   apiKey,
		model:    model,
		endpoint: qwenEndpoint,
	}
}
