package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the plugin block from plugins.configs.<pluginID>.
//
//	plugins:
//	  configs:
//	    prompt-filter:
//	      enabled: true
//	      priority: 100
//	      roles: ["system"]
//	      patterns:
//	        - "some vendor specific sentence. "
type Config struct {
	// Roles limits filtering to messages with these roles. Defaults to system.
	Roles []string `yaml:"roles"`
	// Patterns are literal fragments removed from matching message content.
	// Quote them in YAML when they end with a space, otherwise trailing
	// whitespace is stripped by the YAML parser.
	Patterns []string `yaml:"patterns"`
}

type rules struct {
	roles    map[string]struct{}
	patterns []string
}

type contentMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messageDocument struct {
	Messages []contentMessage `json:"messages"`
}

func parseRules(raw []byte) (rules, error) {
	out := rules{roles: map[string]struct{}{"system": {}}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return out, fmt.Errorf("decode config: %w", err)
	}

	if len(cfg.Roles) > 0 {
		out.roles = make(map[string]struct{}, len(cfg.Roles))
		for _, role := range cfg.Roles {
			role = strings.ToLower(strings.TrimSpace(role))
			if role != "" {
				out.roles[role] = struct{}{}
			}
		}
	}

	for _, pattern := range cfg.Patterns {
		if pattern == "" {
			continue
		}
		// Deleting a double quote from inside a JSON string would break the
		// literal, so those patterns are refused instead of silently
		// corrupting requests. Backslashes and control characters are fine:
		// they are always escaped in the raw body, so the escaped form
		// removed here is a complete escape sequence.
		if strings.Contains(pattern, "\"") {
			logf("ignoring pattern containing a double quote: %q", pattern)
			continue
		}
		out.patterns = append(out.patterns, escapeJSONString(pattern))
	}
	return out, nil
}

// escapeJSONString returns s as it appears between the quotes of a JSON string
// literal, so it can be located directly in the raw request body.
func escapeJSONString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil || len(encoded) < 2 {
		return s
	}
	return string(encoded[1 : len(encoded)-1])
}

// editsFor returns byte-range deletions for every configured pattern found in a
// matching message. It returns nil when the body does not need modification.
func (r rules) editsFor(body []byte) []byteEdit {
	if len(r.patterns) == 0 {
		return nil
	}

	// Fast path: most requests carry no configured fragment at all. A plain
	// substring scan costs a fraction of a millisecond even for multi-megabyte
	// bodies, while decoding the document does not.
	needed := false
	for _, pattern := range r.patterns {
		if bytes.Contains(body, []byte(pattern)) {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}

	var doc messageDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		// Not a chat-completions shaped document, or malformed. Leave it to the
		// upstream to reject; never guess at byte offsets in an unknown body.
		return nil
	}

	var edits []byteEdit
	cursor := 0
	for _, message := range doc.Messages {
		if _, ok := r.roles[strings.ToLower(message.Role)]; !ok {
			continue
		}
		literal := message.Content
		if len(literal) < 2 || literal[0] != '"' {
			// Multimodal content arrays and nulls are left untouched.
			continue
		}
		found := bytes.Index(body[cursor:], literal)
		if found < 0 {
			continue
		}
		start := cursor + found
		end := start + len(literal)
		cursor = end

		for _, pattern := range r.patterns {
			offset := start
			for offset < end {
				hit := bytes.Index(body[offset:end], []byte(pattern))
				if hit < 0 {
					break
				}
				from := offset + hit
				edits = append(edits, byteEdit{Start: from, End: from + len(pattern)})
				offset = from + len(pattern)
			}
		}
	}
	return normalizeEdits(edits)
}

// sameRules reports whether two rule sets would behave identically.
func sameRules(a, b rules) bool {
	if len(a.patterns) != len(b.patterns) || len(a.roles) != len(b.roles) {
		return false
	}
	for i := range a.patterns {
		if a.patterns[i] != b.patterns[i] {
			return false
		}
	}
	for role := range a.roles {
		if _, ok := b.roles[role]; !ok {
			return false
		}
	}
	return true
}

// normalizeEdits sorts edits and drops overlaps, which the host rejects.
func normalizeEdits(edits []byteEdit) []byteEdit {
	if len(edits) == 0 {
		return nil
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].Start != edits[j].Start {
			return edits[i].Start < edits[j].Start
		}
		// Longer match first so the most specific pattern wins when two
		// configured patterns start at the same offset.
		return edits[i].End > edits[j].End
	})
	out := edits[:0]
	previousEnd := -1
	for _, edit := range edits {
		if edit.Start < previousEnd {
			continue
		}
		out = append(out, edit)
		previousEnd = edit.End
	}
	return out
}
