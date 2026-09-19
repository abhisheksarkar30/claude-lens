// Package replay holds the request-replay machinery that is not HTTP: the
// pure body-editing logic (edit.go) and the compact Outcome view of a call
// that the replay endpoint returns and `clens replay` diffs (replay.go).
//
// The split mirrors internal/analyze's: the trickiest part — a JSON-path walk
// into nested structures — is a pure function of bytes with no I/O, so it can
// be tested exhaustively without a network, a store, or a running server.
// Nothing in this package sends anything; the send path is the proxy's own
// transport, driven by internal/api's replay endpoint, so a replay is captured
// and recorded by exactly the code that handles live traffic (br-GI-1-16).
package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Edit is one `--set <path>=<value>` operation. New is the JSON encoding of
// the replacement value and Old is filled in by ApplyEdits with the value that
// was there before it, which is what lets the caller serialize a whole edit
// list verbatim as the events.replay_edits column's [{path, old, new}] array
// — an edited replay is self-documenting rather than an unexplained variant.
//
// New is a json.RawMessage rather than a string because the "JSON when it
// parses, a plain string otherwise" decision is made once, by ParseSet, before
// the walker ever sees it: the walker never has to guess at a value's type.
// Field order matches the recorded {path, old, new} shape.
type Edit struct {
	Path string          `json:"path"`
	Old  json.RawMessage `json:"old"`
	New  json.RawMessage `json:"new"`
}

// ParseSet parses one --set argument, "<path>=<value>". It splits on the first
// '=' so a value may itself contain one (a base64 blob, a URL). The value is
// JSON when it parses as JSON and a JSON string otherwise, which is what makes
// `--set temperature=0.7` a number, `--set stream=true` a boolean, and both
// `--set messages.0.content='"hi"'` and `--set messages.0.content=hi` the
// string "hi".
func ParseSet(arg string) (Edit, error) {
	path, val, ok := strings.Cut(arg, "=")
	if !ok {
		return Edit{}, fmt.Errorf("--set %q: want <jsonpath>=<value>", arg)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return Edit{}, fmt.Errorf("--set %q: empty path", arg)
	}
	return Edit{Path: path, New: encodeValue(val)}, nil
}

// ParseSets parses every --set argument, in order. ApplyEdits applies them in
// that same order, so a later edit to a path an earlier one touched wins.
func ParseSets(args []string) ([]Edit, error) {
	edits := make([]Edit, 0, len(args))
	for _, a := range args {
		e, err := ParseSet(a)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	return edits, nil
}

// encodeValue is the "parsed as JSON when it parses as JSON" rule, in one
// predicate: a value that is valid JSON is used verbatim (so `0.7` stays a
// number and `true` a boolean), and anything else is marshalled as a JSON
// string.
func encodeValue(v string) json.RawMessage {
	if json.Valid([]byte(v)) {
		return json.RawMessage(v)
	}
	b, _ := json.Marshal(v) // cannot fail for a string
	return b
}

// MarshalEdits serializes applied edits as the events.replay_edits column's
// JSON text. No edits yields "" — a replay with no edits records no edits blob
// rather than an empty array, so the column's emptiness keeps meaning "nothing
// was changed".
func MarshalEdits(edits []Edit) (string, error) {
	if len(edits) == 0 {
		return "", nil
	}
	b, err := json.Marshal(edits)
	if err != nil {
		return "", fmt.Errorf("replay: encode edits: %w", err)
	}
	return string(b), nil
}

// ApplyEdits applies edits to body in order and returns the re-encoded body,
// recording in each edit's Old the value it replaced. It is pure: no I/O, and
// the body slice it is handed is never written to.
//
// Applying is all-or-nothing. Every edit walks the decoded tree and any failure
// abandons the whole operation with a nil body and a non-nil error, so a caller
// can never observe — and therefore never send or store — a partially edited
// result. That is what makes the "error, no mutation" guarantee true rather
// than merely likely. Edits apply in sequence against the accumulated result,
// so a second edit to the same path sees the first edit's value (and records
// it as its Old) and wins.
//
// Byte preservation, stated plainly rather than claimed: the body is decoded
// into interface{} values and re-encoded with json.Marshal, so this does NOT
// preserve the original bytes. Unrelated fields keep their values, and because
// the decoder uses UseNumber every number keeps its original literal text
// (64000 stays 64000, never 64000.0; a value beyond float64's precision is not
// silently rounded). But the re-encoding normalizes whitespace and re-sorts
// object keys alphabetically, so any formatting the original body had is gone.
// An empty edit list returns body byte-identical (no decode at all), and a body
// that is not a JSON value is refused rather than guessed at.
func ApplyEdits(body []byte, edits []Edit) ([]byte, error) {
	if len(edits) == 0 {
		return body, nil
	}
	root, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("replay: body is not valid JSON: %w", err)
	}
	for i := range edits {
		if err := applyOne(root, &edits[i]); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("replay: re-encode body: %w", err)
	}
	return out, nil
}

// decode unmarshals body into the generic tree ApplyEdits walks. UseNumber is
// load-bearing: without it every number becomes a float64 and re-encodes with
// float formatting, which would rewrite a token count of 64000 as itself but a
// max_tokens of 10000000000000001 as 1e+16.
func decode(body []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// applyOne walks path from root and replaces the value it names, recording the
// replaced value in e.Old first.
//
// The walk descends only through structure that already exists. A missing key,
// an out-of-range array index, a non-numeric index, and an attempt to descend
// through a scalar are all errors — never an invitation to invent the missing
// intermediate object, which would turn a typo into a silently different
// request.
func applyOne(root interface{}, e *Edit) error {
	parts := strings.Split(e.Path, ".")
	cur := root
	for i, part := range parts {
		last := i == len(parts)-1
		prefix := strings.Join(parts[:i+1], ".")
		switch node := cur.(type) {
		case map[string]interface{}:
			if part == "" {
				return fmt.Errorf("replay: --set %s: empty key in path %q", e.Path, e.Path)
			}
			child, ok := node[part]
			if !ok {
				return fmt.Errorf("replay: --set %s: no key %q — the path must already exist, replay never creates structure", e.Path, prefix)
			}
			if last {
				repl, err := replace(e, child)
				if err != nil {
					return err
				}
				node[part] = repl
				return nil
			}
			cur = child

		case []interface{}:
			idx, err := strconv.Atoi(part)
			if err != nil {
				return fmt.Errorf("replay: --set %s: %q is not an array index (want 0..%d)", e.Path, prefix, len(node)-1)
			}
			if idx < 0 || idx >= len(node) {
				return fmt.Errorf("replay: --set %s: index %d out of range (length %d)", e.Path, idx, len(node))
			}
			if last {
				repl, err := replace(e, node[idx])
				if err != nil {
					return err
				}
				node[idx] = repl
				return nil
			}
			cur = node[idx]

		default:
			return fmt.Errorf("replay: --set %s: cannot descend into %s at %q", e.Path, jsonKind(cur), prefix)
		}
	}
	return nil
}

// replace decodes e.New, records old into e.Old, and returns the decoded
// replacement for the caller to store. Recording happens after the decode
// succeeds, so an unusable value never leaves a half-applied edit behind.
func replace(e *Edit, old interface{}) (interface{}, error) {
	repl, err := decode(e.New)
	if err != nil {
		return nil, fmt.Errorf("replay: --set %s: invalid JSON value %s: %v", e.Path, e.New, err)
	}
	encoded, err := json.Marshal(old)
	if err != nil {
		encoded = []byte("null") // unreachable for a decoded JSON value
	}
	e.Old = encoded
	return repl, nil
}

// jsonKind names a scalar's type for the "cannot descend into" error, so the
// message says what the path actually ran into.
func jsonKind(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	default:
		return "a value"
	}
}
