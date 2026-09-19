package replay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// baseBody is the fixture every test edits: a scalar to change, a field to
// leave alone, a nested object inside an array, and an array of objects.
const baseBody = `{"model":"claude-sonnet-5","temperature":0.2,"max_tokens":100,` +
	`"stream":false,` +
	`"messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"name":"a"},{"name":"b"},{"name":"c"}]}`

// apply parses the given --set arguments, applies them, and fails the test on
// any error — the happy-path shorthand.
func apply(t *testing.T, body string, sets ...string) ([]byte, []Edit) {
	t.Helper()
	edits, err := ParseSets(sets)
	if err != nil {
		t.Fatalf("ParseSets(%q): %v", sets, err)
	}
	out, err := ApplyEdits([]byte(body), edits)
	if err != nil {
		t.Fatalf("ApplyEdits(%q, %q): %v", body, sets, err)
	}
	return out, edits
}

// top decodes a result body and returns the object, failing on anything that is
// not one.
func top(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode result %s: %v", body, err)
	}
	return m
}

func TestApplyEditsReplacesScalarAndLeavesSiblings(t *testing.T) {
	out, edits := apply(t, baseBody, "temperature=0.7")
	got := top(t, out)

	if got["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", got["temperature"])
	}
	if got["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want 100 (untouched)", got["max_tokens"])
	}
	if got["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v, want claude-sonnet-5 (untouched)", got["model"])
	}
	if len(edits) != 1 || edits[0].Path != "temperature" {
		t.Errorf("edits = %+v, want one edit on temperature", edits)
	}
}

// A bare --set value that is not itself valid JSON must become a JSON string,
// not a bare token that would make the body unparseable.
func TestApplyEditsSetPlainValueIsAString(t *testing.T) {
	out, _ := apply(t, baseBody, "messages.0.content=hello there")
	if !strings.Contains(string(out), `"content":"hello there"`) {
		t.Errorf("body = %s, want content to be the JSON string \"hello there\"", out)
	}
	if !json.Valid(out) {
		t.Errorf("body is not valid JSON: %s", out)
	}

	// The explicitly quoted form is the same value, and `true`/`0.7` are not
	// strings, because they parse as JSON.
	for _, tc := range []struct{ set, want string }{
		// The quoted form is the same value said explicitly as JSON.
		{`messages.0.content="hello there"`, `"content":"hello there"`},
		{"stream=true", `"stream":true`},
		{"temperature=0.7", `"temperature":0.7`},
	} {
		out, _ := apply(t, baseBody, tc.set)
		if !strings.Contains(string(out), tc.want) {
			t.Errorf("--set %s: body = %s, want it to contain %s", tc.set, out, tc.want)
		}
	}
}

func TestApplyEditsNestedArrayElement(t *testing.T) {
	out, _ := apply(t, baseBody, "messages.0.content=bye")
	got := top(t, out)

	msgs := got["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want one element", msgs)
	}
	first := msgs[0].(map[string]interface{})
	if first["content"] != "bye" {
		t.Errorf("messages.0.content = %v, want bye", first["content"])
	}
	if first["role"] != "user" {
		t.Errorf("messages.0.role = %v, want user (untouched)", first["role"])
	}
}

func TestApplyEditsArrayIndexSelectsTheRightElement(t *testing.T) {
	out, _ := apply(t, baseBody, "tools.2.name=z")
	got := top(t, out)

	tools := got["tools"].([]interface{})
	if len(tools) != 3 {
		t.Fatalf("tools has %d elements, want 3", len(tools))
	}
	for i, want := range []string{"a", "b", "z"} {
		if name := tools[i].(map[string]interface{})["name"]; name != want {
			t.Errorf("tools.%d.name = %v, want %v", i, name, want)
		}
	}
}

// A path through a key that does not exist is an error, never an invitation to
// invent the missing object — a typo must not become a silently different
// request.
func TestApplyEditsMissingIntermediateKeyErrors(t *testing.T) {
	body := []byte(baseBody)
	edits, err := ParseSets([]string{"a.b.c=1"})
	if err != nil {
		t.Fatalf("ParseSets: %v", err)
	}

	out, err := ApplyEdits(body, edits)
	if err == nil {
		t.Fatal("ApplyEdits: want an error for a missing intermediate key, got nil")
	}
	if !strings.Contains(err.Error(), `no key "a"`) {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
	if out != nil {
		t.Errorf("out = %s, want nil on error", out)
	}
	if !bytes.Equal(body, []byte(baseBody)) {
		t.Errorf("the input body was mutated: %s", body)
	}
	if edits[0].Old != nil {
		t.Errorf("Old = %s, want unset for an edit that never applied", edits[0].Old)
	}

	// A key missing further down names the whole path segment that was looked
	// for, not just the last one.
	_, err = ApplyEdits(body, []Edit{{Path: "messages.0.metadata.x", New: json.RawMessage(`1`)}})
	if err == nil || !strings.Contains(err.Error(), `no key "messages.0.metadata"`) {
		t.Errorf("error = %v, want it to name the missing key at depth", err)
	}
}

// An out-of-range index must name both the index and the length: "out of range"
// alone does not tell the user whether they miscounted or indexed the wrong
// array.
func TestApplyEditsIndexOutOfRangeNamesIndexAndLength(t *testing.T) {
	edits, _ := ParseSets([]string{"tools.99.name=z"})
	_, err := ApplyEdits([]byte(baseBody), edits)
	if err == nil {
		t.Fatal("ApplyEdits: want an error for an out-of-range index, got nil")
	}
	if !strings.Contains(err.Error(), "index 99 out of range (length 3)") {
		t.Errorf("error = %v, want it to name index 99 and length 3", err)
	}
}

func TestApplyEditsNonNumericIndexErrors(t *testing.T) {
	edits, _ := ParseSets([]string{"tools.x.name=z"})
	_, err := ApplyEdits([]byte(baseBody), edits)
	if err == nil {
		t.Fatal("ApplyEdits: want an error for a non-numeric index, got nil")
	}
	if !strings.Contains(err.Error(), "is not an array index") {
		t.Errorf("error = %v, want it to say the segment is not an array index", err)
	}
}

func TestApplyEditsCannotDescendIntoAScalar(t *testing.T) {
	edits, _ := ParseSets([]string{"temperature.degrees=1"})
	_, err := ApplyEdits([]byte(baseBody), edits)
	if err == nil || !strings.Contains(err.Error(), "cannot descend into a number") {
		t.Errorf("error = %v, want a clear \"cannot descend\" error", err)
	}
}

// A malformed value must fail without leaving anything half-applied: the
// caller must never be able to send or store a partially edited body.
func TestApplyEditsInvalidJSONValueLeavesNoPartialMutation(t *testing.T) {
	body := []byte(baseBody)
	// A hand-built Edit can carry bytes ParseSet would never produce (it always
	// encodes a usable value), which is exactly the case this guards.
	edits := []Edit{
		{Path: "temperature", New: json.RawMessage(`0.9`)},
		{Path: "max_tokens", New: json.RawMessage(`{not json`)},
	}

	out, err := ApplyEdits(body, edits)
	if err == nil {
		t.Fatal("ApplyEdits: want an error for an invalid JSON value, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON value") {
		t.Errorf("error = %v, want it to name the invalid value", err)
	}
	if out != nil {
		t.Errorf("out = %s, want nil: the first edit must not survive the second failing", out)
	}
	if !bytes.Equal(body, []byte(baseBody)) {
		t.Errorf("the input body was mutated: %s", body)
	}
}

// No edits means no decoding at all: the body comes back byte-identical, so a
// --set-free replay sends exactly the stored bytes.
func TestApplyEditsEmptyEditsAreByteIdentical(t *testing.T) {
	// Deliberately not canonical JSON: whitespace and key order that a
	// round-trip through a map would rewrite.
	body := []byte("{\n  \"b\": 2,\n  \"a\": 1\n}\n")
	out, err := ApplyEdits(body, nil)
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("out = %q, want %q byte-for-byte", out, body)
	}
}

func TestApplyEditsApplyInOrderAndLaterEditWins(t *testing.T) {
	out, edits := apply(t, baseBody, "temperature=0.5", "temperature=0.9")
	if got := top(t, out)["temperature"]; got != 0.9 {
		t.Errorf("temperature = %v, want the later edit (0.9) to win", got)
	}
	// The second edit's recorded Old is what the first edit left behind, which
	// is what makes the replay_edits blob readable as a sequence.
	if string(edits[0].Old) != "0.2" || string(edits[1].Old) != "0.5" {
		t.Errorf("Old values = %s then %s, want 0.2 then 0.5", edits[0].Old, edits[1].Old)
	}
	if string(edits[1].New) != "0.9" {
		t.Errorf("New = %s, want 0.9", edits[1].New)
	}
}

// Byte preservation, pinned rather than claimed: unrelated values survive, and
// the re-encoding's own behaviour (compact output, keys sorted alphabetically,
// numbers kept as their original literal text) is asserted so a future change
// to it is caught rather than quietly shipped.
func TestApplyEditsNormalizesEncodingButPreservesValues(t *testing.T) {
	pretty := "{\n  \"z\": \"last\",\n  \"max_tokens\": 64000,\n  \"huge\": 10000000000000001,\n" +
		"  \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}],\n  \"temperature\": 0.2\n}"
	out, _ := apply(t, pretty, "temperature=0.7")

	if bytes.ContainsAny(out, "\n\t") {
		t.Errorf("out = %s, want compact output (the original's whitespace is not preserved)", out)
	}
	if !strings.HasPrefix(string(out), `{"huge":10000000000000001,`) {
		t.Errorf("out = %s, want object keys sorted alphabetically", out)
	}
	// A token count beyond float64's exact range must not be rounded into a
	// different number by the round-trip; that would silently change a request.
	for _, want := range []string{`"max_tokens":64000`, `"huge":10000000000000001`, `"z":"last"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("out = %s, want it to contain %s", out, want)
		}
	}

	var got, want map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if err := json.Unmarshal([]byte(pretty), &want); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &want["messages"].([]interface{})[0]); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if got["z"] != want["z"] || got["max_tokens"] != want["max_tokens"] {
		t.Errorf("unrelated fields changed: got %v, want %v", got, want)
	}
}

// The replay_edits column is [{path, old, new}] with the old value that was
// actually replaced — that is what makes an edited replay self-documenting.
func TestMarshalEditsRecordsPathOldAndNew(t *testing.T) {
	_, edits := apply(t, baseBody, "temperature=0.7", "messages.0.content=bye")

	blob, err := MarshalEdits(edits)
	if err != nil {
		t.Fatalf("MarshalEdits: %v", err)
	}
	var got []struct {
		Path string          `json:"path"`
		Old  json.RawMessage `json:"old"`
		New  json.RawMessage `json:"new"`
	}
	if err := json.Unmarshal([]byte(blob), &got); err != nil {
		t.Fatalf("decode %s: %v", blob, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d edits, want 2: %s", len(got), blob)
	}
	if got[0].Path != "temperature" || string(got[0].Old) != "0.2" || string(got[0].New) != "0.7" {
		t.Errorf("edit 0 = %+v, want temperature 0.2 -> 0.7", got[0])
	}
	if got[1].Path != "messages.0.content" || string(got[1].Old) != `"hi"` || string(got[1].New) != `"bye"` {
		t.Errorf(`edit 1 = %+v, want messages.0.content "hi" -> "bye"`, got[1])
	}

	// The recorded JSON's key order is path, old, new, matching the bead's
	// documented shape rather than merely its content.
	if !strings.HasPrefix(blob, `[{"path":"temperature","old":0.2,"new":0.7}`) {
		t.Errorf("blob = %s, want {path, old, new} key order", blob)
	}

	// A replay with no edits records nothing rather than an empty array: the
	// column's emptiness keeps meaning "nothing changed".
	if empty, err := MarshalEdits(nil); err != nil || empty != "" {
		t.Errorf("MarshalEdits(nil) = %q, %v; want \"\", nil", empty, err)
	}
}

func TestParseSetRejectsMalformedArguments(t *testing.T) {
	for _, arg := range []string{"temperature", "=0.7", "  =x"} {
		if _, err := ParseSet(arg); err == nil {
			t.Errorf("ParseSet(%q): want an error, got nil", arg)
		}
	}

	// A value may itself contain '=' — only the first one separates.
	e, err := ParseSet("metadata.user_id=a=b")
	if err != nil {
		t.Fatalf("ParseSet: %v", err)
	}
	if e.Path != "metadata.user_id" || string(e.New) != `"a=b"` {
		t.Errorf("ParseSet = %+v, want path metadata.user_id and value \"a=b\"", e)
	}
}

func TestApplyEditsRejectsAnUnparseableBody(t *testing.T) {
	edits, _ := ParseSets([]string{"temperature=0.7"})
	if _, err := ApplyEdits([]byte("not json at all"), edits); err == nil {
		t.Fatal("ApplyEdits: want an error for a body that is not JSON, got nil")
	}
}
