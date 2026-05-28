// Gemini (Google) provider — thin wrapper over openAICompatibleProvider.
//
// Google maintains an OpenAI-compatible endpoint at /v1beta/openai/ that
// accepts the standard OpenAI chat completions request/response shape with
// Authorization: Bearer <gemini-api-key>. Released late 2024, still the
// recommended path for cross-vendor SDK interop as of v4.0.18.
//
// Endpoint reference:
//
//	https://generativelanguage.googleapis.com/v1beta/openai/chat/completions
//
// Auth: Bearer <Google AI Studio API key, typically starts with "AIza">
// Default model: gemini-2.5-flash-lite (per identity.DefaultModel) — Google's
// current cheap+fast model, analogous to gpt-4o-mini / claude-3-5-haiku.
//
// Note: if Google ever drops the OpenAI-compat path, the fallback is the
// native /v1beta/models/<model>:generateContent endpoint with a different
// request schema (systemInstruction top-level, contents[].parts[].text,
// candidates[].content.parts[].text in response). That would warrant a
// separate file rather than reusing openAICompatibleProvider.
package ai

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

func newGeminiProvider(apiKey, model string) Provider {
	return &openAICompatibleProvider{
		name:     "gemini",
		apiKey:   apiKey,
		model:    model,
		endpoint: geminiEndpoint,
	}
}
