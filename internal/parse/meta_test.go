package parse

import (
	"net/http"
	"testing"
)

func TestExtractMetaBasicFields(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5",
		"service_tier": "priority",
		"speed": "fast",
		"effort": "high",
		"inference_geo": "us",
		"messages": []
	}`)
	m := ExtractMeta(body, http.Header{})
	if m.ModelRequested != "claude-sonnet-5" {
		t.Errorf("ModelRequested = %q, want claude-sonnet-5", m.ModelRequested)
	}
	if m.ServiceTier != "priority" || m.Speed != "fast" || m.Effort != "high" || m.InferenceGeo != "us" {
		t.Errorf("request fields = %+v, unexpected", m)
	}
}

func TestExtractMetaSessionHeader(t *testing.T) {
	h := http.Header{}
	h.Set("x-clens-session", "my-session-1")
	m := ExtractMeta([]byte(`{}`), h)
	if m.SessionHeader != "my-session-1" {
		t.Errorf("SessionHeader = %q, want my-session-1", m.SessionHeader)
	}

	mNoHeader := ExtractMeta([]byte(`{}`), http.Header{})
	if mNoHeader.SessionHeader != "" {
		t.Errorf("SessionHeader with no header set = %q, want empty", mNoHeader.SessionHeader)
	}
}

// x-claude-code-session-id (D7) fills Meta.SessionHeader when
// x-clens-session is absent.
func TestExtractMetaSessionHeaderFromClaudeCodeSessionIDWhenClensSessionAbsent(t *testing.T) {
	h := http.Header{}
	h.Set("x-claude-code-session-id", "conv-abc123")
	m := ExtractMeta([]byte(`{}`), h)
	if m.SessionHeader != "conv-abc123" {
		t.Errorf("SessionHeader = %q, want conv-abc123", m.SessionHeader)
	}
}

// x-clens-session still wins when both headers are present.
func TestExtractMetaSessionHeaderClensSessionWinsOverClaudeCodeSessionID(t *testing.T) {
	h := http.Header{}
	h.Set("x-clens-session", "operator-override")
	h.Set("x-claude-code-session-id", "conv-abc123")
	m := ExtractMeta([]byte(`{}`), h)
	if m.SessionHeader != "operator-override" {
		t.Errorf("SessionHeader = %q, want operator-override (x-clens-session must win)", m.SessionHeader)
	}
}

// An overlong x-clens-session does not supply a value, so a valid
// x-claude-code-session-id is used instead -- the overlong override case.
func TestExtractMetaOverlongClensSessionFallsBackToClaudeCodeSessionID(t *testing.T) {
	h := http.Header{}
	overlong := make([]byte, maxSessionHeaderLen+1)
	for i := range overlong {
		overlong[i] = 'x'
	}
	h.Set("x-clens-session", string(overlong))
	h.Set("x-claude-code-session-id", "conv-abc123")
	m := ExtractMeta([]byte(`{}`), h)
	if m.SessionHeader != "conv-abc123" {
		t.Errorf("SessionHeader = %q, want conv-abc123 (overlong x-clens-session must be treated as absent)", m.SessionHeader)
	}
}

func TestExtractMetaCacheControlSitesAndTools(t *testing.T) {
	body := []byte(`{
		"system": [{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"tools": [{"name":"search","cache_control":{"type":"ephemeral"}}, {"name":"calc"}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}
		]
	}`)
	m := ExtractMeta(body, http.Header{})
	if !m.HasCacheControl {
		t.Fatal("HasCacheControl = false, want true")
	}
	want := []string{"system[0]", "tools[0]", "messages[0].content[0]"}
	if len(m.CacheControlSites) != len(want) {
		t.Fatalf("CacheControlSites = %v, want %v", m.CacheControlSites, want)
	}
	for i, w := range want {
		if m.CacheControlSites[i] != w {
			t.Errorf("CacheControlSites[%d] = %q, want %q", i, m.CacheControlSites[i], w)
		}
	}
	if m.ToolCount != 2 {
		t.Errorf("ToolCount = %d, want 2", m.ToolCount)
	}
	if len(m.ToolNames) != 2 || m.ToolNames[0] != "search" || m.ToolNames[1] != "calc" {
		t.Errorf("ToolNames = %v, want [search calc]", m.ToolNames)
	}
}

func TestExtractMetaThinkingConfig(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":2048}}`)
	m := ExtractMeta(body, http.Header{})
	if !m.HasThinking {
		t.Fatal("HasThinking = false, want true")
	}
	if m.ThinkingBudget == nil || *m.ThinkingBudget != 2048 {
		t.Errorf("ThinkingBudget = %v, want 2048", m.ThinkingBudget)
	}
}

func TestExtractMetaPrefixHashStableAndSensitive(t *testing.T) {
	bodyA := []byte(`{"system":"s1","messages":[{"role":"user","content":"hi"}]}`)
	bodyB := []byte(`{"system":"s1","messages":[{"role":"user","content":"hi"}]}`)
	bodyC := []byte(`{"system":"s2","messages":[{"role":"user","content":"hi"}]}`)

	mA := ExtractMeta(bodyA, http.Header{})
	mB := ExtractMeta(bodyB, http.Header{})
	mC := ExtractMeta(bodyC, http.Header{})

	if mA.PrefixHash == nil || mB.PrefixHash == nil || mC.PrefixHash == nil {
		t.Fatal("PrefixHash = nil, want a computed hash when no SessionHeader is set")
	}
	if *mA.PrefixHash != *mB.PrefixHash {
		t.Errorf("PrefixHash not stable for identical bodies: %q vs %q", *mA.PrefixHash, *mB.PrefixHash)
	}
	if *mA.PrefixHash == *mC.PrefixHash {
		t.Error("PrefixHash did not change for a different body")
	}
}

// TestExtractMetaPrefixHashNilWhenSessionHeaderPresent's invariant moved
// (D7): the hash is now always computed, session header or not -- the
// session resolver's groupKey checks SessionHeader first, so a present
// header still wins there without the hash being nil-ed.
func TestExtractMetaPrefixHashNilWhenSessionHeaderPresent(t *testing.T) {
	h := http.Header{}
	h.Set("x-clens-session", "explicit-session")
	m := ExtractMeta([]byte(`{"system":"s","messages":[]}`), h)
	if m.PrefixHash == nil {
		t.Fatal("PrefixHash = nil, want a computed hash even when SessionHeader is set")
	}
	if *m.PrefixHash == "" {
		t.Error("PrefixHash = empty string, want a real computed hash (the body parsed)")
	}
}

// TestExtractMetaPrefixHashEmptyStringWhenBodyDoesNotParse covers the
// other half: "" (not nil) means the body did not parse as JSON at all.
func TestExtractMetaPrefixHashEmptyStringWhenBodyDoesNotParse(t *testing.T) {
	m := ExtractMeta([]byte("not json"), http.Header{})
	if m.PrefixHash == nil {
		t.Fatal("PrefixHash = nil, want a non-nil empty string for an unparseable body")
	}
	if *m.PrefixHash != "" {
		t.Errorf("PrefixHash = %q, want empty string", *m.PrefixHash)
	}
}

// TestExtractMetaJSONLOnlyFieldsEmptyOnProxyRequest pins that a
// proxy-sourced Meta never fabricates the fields only source B (JSONL)
// supplies.
func TestExtractMetaJSONLOnlyFieldsEmptyOnProxyRequest(t *testing.T) {
	m := ExtractMeta([]byte(`{"model":"m"}`), http.Header{})
	if m.ClientVersion != "" || m.Project != "" || m.GitBranch != "" || m.CliEntrypoint != "" || m.IsSidechain {
		t.Errorf("JSONL-only fields not empty on a proxy request: %+v", m)
	}
}

func TestExtractMetaTypeConfusionDoesNotPanic(t *testing.T) {
	body := []byte(`{
		"model": 123,
		"system": "not-an-array",
		"tools": "not-an-array",
		"messages": [ "not-an-object", {"role":"user","content":"not-an-array"} ],
		"thinking": "not-an-object"
	}`)
	// Must not panic.
	m := ExtractMeta(body, http.Header{})
	if m.ModelRequested != "" {
		t.Errorf("ModelRequested = %q, want empty for a non-string model", m.ModelRequested)
	}
	if m.HasThinking {
		t.Error("HasThinking = true, want false for a non-object thinking field")
	}
}
