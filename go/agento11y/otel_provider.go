package agento11y

import "strings"

var otelProviderStoredToWire = map[string]string{
	"anthropic":          "anthropic",
	"aws.bedrock":        "aws.bedrock",
	"azure.ai.inference": "azure.ai.inference",
	"azure.ai.openai":    "azure.ai.openai",
	"azure-ai-inference": "azure.ai.inference",
	"azure-openai":       "azure.ai.openai",
	"bedrock":            "aws.bedrock",
	"cohere":             "cohere",
	"deepseek":           "deepseek",
	"gcp.gen_ai":         "gcp.gen_ai",
	"gcp.gemini":         "gcp.gemini",
	"gcp.vertex_ai":      "gcp.vertex_ai",
	"gemini":             "gcp.gemini",
	"groq":               "groq",
	"ibm.watsonx.ai":     "ibm.watsonx.ai",
	"mistral":            "mistral_ai",
	"mistral_ai":         "mistral_ai",
	"moonshot_ai":        "moonshot_ai",
	"moonshotai":         "moonshot_ai",
	"openai":             "openai",
	"perplexity":         "perplexity",
	"vertex":             "gcp.vertex_ai",
	"watsonx":            "ibm.watsonx.ai",
	"x-ai":               "x_ai",
	"x_ai":               "x_ai",
}

func otelProviderName(provider string) string {
	trimmed := strings.TrimSpace(provider)
	if wire, ok := otelProviderStoredToWire[strings.ToLower(trimmed)]; ok {
		return wire
	}
	return trimmed
}
