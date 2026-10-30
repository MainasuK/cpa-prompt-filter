package main

import (
	"encoding/json"
	"testing"
)

func applyEdits(t *testing.T, body []byte, edits []byteEdit) []byte {
	t.Helper()
	out := make([]byte, 0, len(body))
	previous := 0
	for _, edit := range edits {
		out = append(out, body[previous:edit.Start]...)
		out = append(out, edit.Replacement...)
		previous = edit.End
	}
	out = append(out, body[previous:]...)
	return out
}

func mustRules(t testing.TB, patterns ...string) rules {
	t.Helper()
	config, err := json.Marshal(map[string]any{"patterns": patterns})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	// JSON is a YAML subset, so the same bytes exercise the YAML decoder.
	parsed, err := parseRules(config)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	return parsed
}

func TestEditsRemoveSystemFragment(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"Alpha sentence. Beta sentence. Gamma sentence."},` +
		`{"role":"user","content":"Alpha sentence. Beta sentence. Gamma sentence."}]}`)

	parsed := mustRules(t, "Alpha sentence. ", "Beta sentence. ")
	edits := parsed.editsFor(body)
	if len(edits) != 2 {
		t.Fatalf("want 2 edits, got %d", len(edits))
	}

	patched := applyEdits(t, body, edits)
	if !json.Valid(patched) {
		t.Fatalf("patched body is not valid JSON: %s", patched)
	}

	var doc messageDocument
	if err := json.Unmarshal(patched, &doc); err != nil {
		t.Fatalf("unmarshal patched body: %v", err)
	}
	var system, user string
	if err := json.Unmarshal(doc.Messages[0].Content, &system); err != nil {
		t.Fatalf("unmarshal system content: %v", err)
	}
	if err := json.Unmarshal(doc.Messages[1].Content, &user); err != nil {
		t.Fatalf("unmarshal user content: %v", err)
	}
	if system != "Gamma sentence." {
		t.Fatalf("system content not filtered: %q", system)
	}
	if user != "Alpha sentence. Beta sentence. Gamma sentence." {
		t.Fatalf("user content must be untouched: %q", user)
	}
}

func TestNoopWhenPatternAbsent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"nothing to see here"}]}`)
	if edits := mustRules(t, "Alpha sentence. ").editsFor(body); edits != nil {
		t.Fatalf("want no edits, got %d", len(edits))
	}
}

func TestNoopWhenNoPatterns(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"Alpha sentence. "}]}`)
	if edits := mustRules(t).editsFor(body); edits != nil {
		t.Fatalf("want no edits, got %d", len(edits))
	}
}

func TestQuotedPatternRejected(t *testing.T) {
	parsed := mustRules(t, `say "hello"`)
	if len(parsed.patterns) != 0 {
		t.Fatalf("patterns with quotes must be refused, got %v", parsed.patterns)
	}
}

func TestOverlappingPatternsCollapse(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"abcd"}]}`)
	edits := mustRules(t, "abc", "abcd").editsFor(body)
	patched := applyEdits(t, body, edits)
	if !json.Valid(patched) {
		t.Fatalf("patched body is not valid JSON: %s", patched)
	}
	if got := string(patched); got != `{"messages":[{"role":"system","content":""}]}` {
		t.Fatalf("unexpected patch result: %s", got)
	}
}

func TestRoleAllowList(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"Alpha sentence. "}]}`)
	if edits := mustRules(t, "Alpha sentence. ").editsFor(body); edits != nil {
		t.Fatalf("user role must be filtered out, got %d edits", len(edits))
	}
}

func TestNonChatDocumentIgnored(t *testing.T) {
	body := []byte(`{"input":"Alpha sentence. ","model":"m"}`)
	if edits := mustRules(t, "Alpha sentence. ").editsFor(body); edits != nil {
		t.Fatalf("want no edits, got %d", len(edits))
	}
}

func TestEscapedPatternMatchesRawBody(t *testing.T) {
	// A pattern containing a newline is stored escaped in the raw body.
	parsed := mustRules(t, "line one\nline two")
	if len(parsed.patterns) != 1 {
		t.Fatalf("want 1 pattern, got %v", parsed.patterns)
	}
	body := []byte(`{"messages":[{"role":"system","content":"line one\nline two"}]}`)
	edits := parsed.editsFor(body)
	if len(edits) != 1 {
		t.Fatalf("want 1 edit, got %d", len(edits))
	}
	if patched := applyEdits(t, body, edits); !json.Valid(patched) {
		t.Fatalf("patched body is not valid JSON: %s", patched)
	}
}
