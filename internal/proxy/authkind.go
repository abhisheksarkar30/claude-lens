package proxy

import (
	"net/http"
	"strings"
)

// AuthKind values classify the *shape* of a credential a request carried —
// never the credential's value. billing_mode is derived later, from the
// configured account, not from this classification.
const (
	AuthOAuth   = "oauth"
	AuthAPIKey  = "api_key"
	AuthAdmin   = "admin"
	AuthCloud   = "cloud"
	AuthUnknown = "unknown"
)

// cloudSignatureHeaders name headers that identify a request signed by a
// cloud provider's own SDK (Bedrock, Vertex, Foundry) rather than sent
// directly to api.anthropic.com. Their presence classifies AuthCloud
// without pricing the call — a cloud-routed call is billed by the cloud
// provider, not Anthropic, so CLAUDE.md's two-billing-models-never-summed
// invariant requires this to stay unpriced.
var cloudSignatureHeaders = []string{
	"X-Amz-Date",
	"X-Amz-Security-Token",
	"X-Goog-Api-Key",
}

// ClassifyAuthKind derives auth_kind from headers, never from the
// credential's value. x-api-key is checked before Authorization because
// Anthropic's API accepts either, and a request carrying both should be
// classified by the one Anthropic actually authenticates with.
func ClassifyAuthKind(headers http.Header) string {
	if apiKey := headers.Get("X-Api-Key"); apiKey != "" {
		if strings.HasPrefix(apiKey, "sk-ant-admin") {
			return AuthAdmin
		}
		return AuthAPIKey
	}

	if auth := headers.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
			return AuthCloud
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if strings.HasPrefix(token, "sk-ant-oat") {
			return AuthOAuth
		}
	}

	for _, h := range cloudSignatureHeaders {
		if headers.Get(h) != "" {
			return AuthCloud
		}
	}

	return AuthUnknown
}
