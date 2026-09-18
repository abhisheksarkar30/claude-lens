package proxy

import (
	"net/http"
	"testing"
)

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestClassifyAuthKind(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want string
	}{
		{"oauth bearer", headers("Authorization", "Bearer sk-ant-oat01-abc"), AuthOAuth},
		{"api key", headers("X-Api-Key", "sk-ant-api03-abc"), AuthAPIKey},
		{"admin key", headers("X-Api-Key", "sk-ant-admin01-abc"), AuthAdmin},
		{"bedrock sigv4", headers("Authorization", "AWS4-HMAC-SHA256 Credential=..."), AuthCloud},
		{"bedrock via amz date", headers("X-Amz-Date", "20260101T000000Z"), AuthCloud},
		{"vertex", headers("X-Goog-Api-Key", "abc"), AuthCloud},
		{"absent", http.Header{}, AuthUnknown},
		{"unrecognized bearer", headers("Authorization", "Bearer some-other-token"), AuthUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyAuthKind(tt.h); got != tt.want {
				t.Errorf("ClassifyAuthKind(%v) = %q, want %q", tt.h, got, tt.want)
			}
		})
	}
}
