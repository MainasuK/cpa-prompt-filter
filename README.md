# prompt-filter

A native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that deletes
configured text fragments from request messages before they reach an upstream provider.

It exists for one specific, annoying failure mode: an upstream provider quietly rejects
requests whose `system` message contains vendor-specific agent-harness boilerplate, while
accepting the exact same text in a `user` message — and accepting neutral filler of the same
length. That is content matching, not a size or format limit, and it cannot be fixed by
retrying, trimming history, or swapping API keys.

Rewriting prompts in every client is invasive: instructions move out of the `system` role,
each tool needs its own patch, and the work is invisible to whatever else shares the gateway.
This plugin keeps the fix in one place.

## How it works

The plugin declares the `request_body_patcher_before` capability. For every request the host
passes the **raw request body bytes** across the C ABI and expects byte-range edits back:

```json
{"ok": true, "result": {"edits": [{"start": 1234, "end": 1310}]}}
```

The host splices the edits in a single pass and verifies the result is still valid JSON. The
document is never re-serialized, so the cost stays proportional to the fragments removed
rather than to the body size.

Two paths:

* **Miss** — a plain substring scan over the body. No document decoding, no allocations worth
  mentioning.
* **Hit** — decode the message list, locate the byte span of each matching message's `content`
  string literal, and turn every configured fragment inside that span into a deletion.

Only messages whose `role` is in the configured allow list are touched, so user content that
happens to quote a filtered sentence is left alone.

## Requirements

* A host that supports plugin schema version 8 (`request_body_patcher_before`,
  `request.patch_body_before`). Present from 8.0.44 onward; verified against 8.0.47.
* Go 1.26.x and a C toolchain to build (`cgo` is required by `-buildmode=c-shared`).

## Build

```bash
make build          # -> bin/prompt-filter.so
```

The artifact name is the plugin ID: `bin/prompt-filter.so` registers as `prompt-filter`.

## Install

```bash
sudo install -m 0644 bin/prompt-filter.so /opt/cliproxyapi/plugins/prompt-filter.so
```

```yaml
plugins:
  enabled: true
  dir: "/opt/cliproxyapi/plugins"
  configs:
    prompt-filter:
      enabled: true
      priority: 90
      roles: ["system"]
      patterns:
        - "A sentence the upstream provider refuses to accept. "
        - "Another sentence; upstream matches the pair together. "
```

Restart the gateway and look for the registration line:

```text
pluginhost: plugin registered plugin_id=prompt-filter plugin_name=prompt-filter version=1.0.0
```

## Configuration

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `roles` | list of strings | `["system"]` | Message roles to inspect. Case-insensitive. |
| `patterns` | list of strings | empty | Literal fragments to delete. Empty list disables the plugin. |

Notes:

* **Quote patterns that end with a space.** YAML strips trailing whitespace from unquoted
  scalars, and a fragment ending mid-sentence usually needs the space to avoid leaving a
  double space behind.
* Patterns are matched against the raw JSON, so a pattern containing a newline is looked up in
  its escaped form (`\n`) and removed as a complete escape sequence.
* Patterns containing a double quote are refused at registration time: deleting a quote from
  inside a string literal would corrupt the document.

## Performance

Measured on a 2.1 MB body with 1949 messages, on an aging Xeon-class CPU:

| Path | Time | Allocated |
| --- | --- | --- |
| Fragment present (2 edits) | 22 ms | 3.2 MB |
| No fragment | 0.36 ms | 128 B |

The hit path is dominated by decoding the message list to find the `content` literals. It
still beats a rewriting proxy because nothing is re-encoded, and it beats the host's generic
request interceptor because the body never travels through a JSON-RPC envelope with Base64.

Run the numbers yourself:

```bash
go test -run '^$' -bench . -benchmem
```

## Limitations

* Chat-completions shape only (`messages[].content` as a string). Inputs under other keys and
  multimodal `content` arrays are ignored rather than guessed at.
* If a pattern removes an entire `content` string, the message is left with `""`. Some
  providers reject empty content; configure fragments so at least one sentence survives, or
  drop the message upstream.
* Overlapping patterns collapse to the longest match at the same offset. The host rejects
  overlapping edits, so this is enforced before the response is sent.
* Any parse failure degrades to "no edits". The plugin never guesses at byte offsets in a body
  it does not understand.

## License

MIT. See [LICENSE](LICENSE).
