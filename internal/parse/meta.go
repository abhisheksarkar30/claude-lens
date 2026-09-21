package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
)

// unsupportedBlockTypes and maxSessionHeaderLen mirror deepseek-lens's
// scanner for the shapes br-GI-1-09's tool/cache-invalidation rules
// compare across turns.
//
// maxSessionHeaderLen bounds whichever header supplies Meta.SessionHeader
// before it is accepted, since the session resolver uses it verbatim as
// the session's id. 200 is generous for a client-chosen id/label and
// small next to SQLite's own TEXT limits. It applies to both sources
// below: x-clens-session (the explicit operator override) and
// x-claude-code-session-id (the request's own conversation id, D7) — an
// overlong header is treated as absent, not truncated, so an unbounded
// client value never becomes an unbounded primary key.
const maxSessionHeaderLen = 200

// ExtractMeta pulls every request-level detail the rest of the system
// needs before storing a request out of the raw JSON body and headers. It
// is a pure function: no I/O, no error return. Malformed JSON or an empty
// body still yields a usable Meta — a request this tool cannot parse must
// still be proxied and stored.
//
// Every accessor is nil-safe against type-confused input (a string where
// an array is expected, an object where a string is expected, a null
// field): this is untrusted third-party input, and a shape mismatch
// degrades to "field absent" rather than panicking.
func ExtractMeta(reqBody []byte, headers http.Header) Meta {
	m := Meta{}

	// x-clens-session is the explicit operator override and is read
	// first; x-claude-code-session-id (the request's own conversation id,
	// D7) fills the value only when x-clens-session is absent. An
	// overlong header is treated as absent rather than truncated: unlike
	// PrefixHash (a bounded 16-char hash), this value rides straight into
	// sessions.id verbatim, so an unbounded client header would mean an
	// unbounded primary key — and an overlong x-clens-session does not
	// supply a value, so the second header is used instead.
	if sh := headers.Get("x-clens-session"); sh != "" && len(sh) <= maxSessionHeaderLen {
		m.SessionHeader = sh
	} else if ccsid := headers.Get("x-claude-code-session-id"); ccsid != "" && len(ccsid) <= maxSessionHeaderLen {
		m.SessionHeader = ccsid
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqBody, &body); err != nil {
		empty := ""
		m.PrefixHash = &empty
		return m
	}

	m.ModelRequested = asString(body["model"])
	m.ServiceTier = asString(body["service_tier"])
	m.Speed = asString(body["speed"])
	m.Effort = asString(body["effort"])
	m.InferenceGeo = asString(body["inference_geo"])

	if thinking, ok := asObject(body["thinking"]); ok {
		m.HasThinking = true
		m.ThinkingBudget = asIntPtr(thinking, "budget_tokens")
	}

	var sites []string

	if sysArr, ok := asArray(body["system"]); ok {
		for i, block := range sysArr {
			scanCacheControl(block, fmt.Sprintf("system[%d]", i), &sites)
		}
	}

	if toolArr, ok := asArray(body["tools"]); ok {
		m.ToolCount = len(toolArr)
		for i, tool := range toolArr {
			to, ok := asObject(tool)
			if !ok {
				continue
			}
			if name := asString(to["name"]); name != "" {
				m.ToolNames = append(m.ToolNames, name)
			}
			if _, has := to["cache_control"]; has {
				sites = append(sites, fmt.Sprintf("tools[%d]", i))
			}
		}
	}

	msgArr, _ := asArray(body["messages"])
	for i, msg := range msgArr {
		msgObj, ok := asObject(msg)
		if !ok {
			continue
		}
		contentArr, ok := asArray(msgObj["content"])
		if !ok {
			continue
		}
		for j, block := range contentArr {
			scanCacheControl(block, fmt.Sprintf("messages[%d].content[%d]", i, j), &sites)
		}
	}

	m.CacheControlSites = sites
	m.HasCacheControl = len(sites) > 0

	// Always computed, header or not: groupKey checks SessionHeader first
	// (see internal/session), so a present header still wins there — but
	// ruleCacheExpiredBetweenTurns and ruleCacheConcurrentWriteRace
	// (internal/analyze) continue on a nil hash, and D7 makes
	// SessionHeader non-empty for ~97% of proxy calls, so nil-ing it here
	// would silently kill both rules for nearly every call.
	h := prefixHash(body["system"], msgArr)
	m.PrefixHash = &h

	return m
}

// scanCacheControl inspects one content block (a system[] entry or a
// messages[].content[] entry) for a cache_control marker, appending path
// to sites when found. A non-object block (type confusion) is silently
// skipped.
func scanCacheControl(block interface{}, path string, sites *[]string) {
	obj, ok := asObject(block)
	if !ok {
		return
	}
	if _, has := obj["cache_control"]; has {
		*sites = append(*sites, path)
	}
}

// prefixHash computes the session-correlation key: SHA-256 over the
// JSON-encoded "system" value concatenated with the JSON encoding of the
// first two "messages" entries, hex-encoded and truncated to 16 chars.
// Re-encoding through encoding/json (rather than hashing the raw bytes)
// makes the hash depend only on content, not on incidental whitespace or
// key order.
func prefixHash(system interface{}, messages []interface{}) string {
	h := sha256.New()
	sysBytes, _ := json.Marshal(system)
	h.Write(sysBytes)
	n := len(messages)
	if n > 2 {
		n = 2
	}
	for i := 0; i < n; i++ {
		b, _ := json.Marshal(messages[i])
		h.Write(b)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:16]
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func asObject(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

func asArray(v interface{}) ([]interface{}, bool) {
	a, ok := v.([]interface{})
	return a, ok
}

// asIntPtr returns a pointer to obj[key] as an int if present and
// numeric, or nil otherwise. The pointer is what lets a caller
// distinguish "budget_tokens: 0" (present, points at 0) from an absent
// field (nil).
func asIntPtr(obj map[string]interface{}, key string) *int {
	f, ok := obj[key].(float64)
	if !ok {
		return nil
	}
	i := int(f)
	return &i
}
