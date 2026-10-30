package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
)

// syntheticBody builds a chat-completions document shaped like a large agent
// request: one system prompt carrying the configured fragments, followed by
// many history messages.
func syntheticBody(tb testing.TB, messages int, withPatterns bool) ([]byte, []string) {
	tb.Helper()
	patterns := []string{
		"This sentence is rejected by the upstream gateway. ",
		"So is this one; the gateway matches the pair together. ",
	}

	system := "You are a coding agent."
	if withPatterns {
		system = patterns[0] + patterns[1] + "You are a coding agent."
	}

	var buf bytes.Buffer
	buf.WriteString(`{"model":"example-model","stream":true,"messages":[`)
	first, _ := json.Marshal(map[string]string{"role": "system", "content": system})
	buf.Write(first)
	filler := bytes.Repeat([]byte("filler text for a history message. "), 40)
	for i := 0; i < messages; i++ {
		buf.WriteByte(',')
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		entry, err := json.Marshal(map[string]string{
			"role":    role,
			"content": role + " " + strconv.Itoa(i) + " " + string(filler),
		})
		if err != nil {
			tb.Fatalf("marshal message: %v", err)
		}
		buf.Write(entry)
	}
	buf.WriteString(`]}`)
	return buf.Bytes(), patterns
}

func BenchmarkPatchLargeBodyHit(b *testing.B) {
	body, patterns := syntheticBody(b, 1900, true)
	parsed := mustRules(b, patterns...)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if edits := parsed.editsFor(body); len(edits) != 2 {
			b.Fatalf("want 2 edits, got %d", len(edits))
		}
	}
}

func BenchmarkPatchLargeBodyMiss(b *testing.B) {
	body, patterns := syntheticBody(b, 1900, false)
	parsed := mustRules(b, patterns...)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if edits := parsed.editsFor(body); edits != nil {
			b.Fatalf("want no edits, got %d", len(edits))
		}
	}
}
