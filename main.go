// Command prompt-filter is a native dynamic-library plugin for CLIProxyAPI.
//
// It declares the `request_body_patcher_before` capability: the host hands over
// the raw request body before auth selection, and the plugin answers with
// byte-range edits instead of a re-serialized JSON document. The host splices
// the edits in a single pass and validates the result.
//
// The plugin removes configured text fragments from the `content` of messages
// whose `role` matches a configured set (default: system). It exists because
// some upstream providers reject requests whose system prompt carries
// vendor-specific boilerplate, and removing the offending fragments is cheaper
// and less invasive than rewriting prompts client-side.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyGoCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyGoFree(void*, size_t);
extern void cliproxyGoShutdown(void);

static int bridge_call(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return cliproxyGoCall((char*)method, (uint8_t*)request, request_len, response);
}

static void bridge_free(void* ptr, size_t len) {
	cliproxyGoFree(ptr, len);
}

static void bridge_shutdown(void) {
	cliproxyGoShutdown();
}

static int fill_plugin_api(cliproxy_plugin_api* api) {
	if (api == NULL) {
		return 1;
	}
	api->abi_version = 1;
	api->call = bridge_call;
	api->free_buffer = bridge_free;
	api->shutdown = bridge_shutdown;
	return 0;
}
*/
import "C"

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"unsafe"
)

const (
	pluginABIVersion    = 1
	pluginSchemaVersion = 8
	pluginName          = "prompt-filter"
	pluginVersion       = "1.0.0"
	pluginAuthor        = "CMK"
	pluginRepository    = "https://github.com/MainasuK/cpa-prompt-filter"
)

// registeredConfig is the last successfully parsed configuration. The host may
// call plugin.reconfigure at any time, including concurrently with patch calls,
// so every access goes through the mutex.
var (
	configMu    sync.RWMutex
	activeRules rules
	initialized bool
)

type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcEnvelopeErr `json:"error,omitempty"`
}

type rpcEnvelopeErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registerRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registerResult struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

// The host decodes metadata into a struct without JSON tags, so keys must match
// the Go field names. Underscored keys such as github_repository do not decode.
type metadata struct {
	Name             string `json:"Name"`
	Version          string `json:"Version"`
	Author           string `json:"Author"`
	GitHubRepository string `json:"GitHubRepository"`
}

type capabilities struct {
	RequestBodyPatcherBefore bool `json:"request_body_patcher_before"`
}

type patchResult struct {
	Edits []byteEdit `json:"edits,omitempty"`
}

type byteEdit struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement,omitempty"`
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	// The host API is not used by this plugin yet; it is accepted so the
	// function table signature matches the ABI.
	_ = host
	if rc := C.fill_plugin_api(api); rc != 0 {
		return rc
	}
	logf("initialized (abi=%d schema=%d)", pluginABIVersion, pluginSchemaVersion)
	return 0
}

//export cliproxyGoShutdown
func cliproxyGoShutdown() {
	configMu.Lock()
	activeRules = rules{}
	initialized = false
	configMu.Unlock()
	logf("shutdown")
}

//export cliproxyGoFree
func cliproxyGoFree(ptr unsafe.Pointer, length C.size_t) {
	_ = length
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyGoCall
func cliproxyGoCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	var payload []byte
	if request != nil && requestLen > 0 {
		// Read-only view over host memory; valid for the duration of this call.
		payload = unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen))
	}

	out := dispatch(C.GoString(method), payload)
	writeResponse(response, out)
	return 0
}

func writeResponse(response *C.cliproxy_buffer, data []byte) {
	if response == nil {
		return
	}
	if len(data) == 0 {
		response.ptr = nil
		response.len = 0
		return
	}
	response.ptr = C.CBytes(data)
	response.len = C.size_t(len(data))
}

func dispatch(method string, payload []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return handleRegister(payload)
	case "request.patch_body_before", "request.patch_body_after":
		return handlePatch(payload)
	case "plugin.quiesce", "plugin.shutdown":
		return okEnvelope(struct{}{})
	default:
		return errEnvelope("unsupported_method", "method "+method+" is not implemented")
	}
}

func handleRegister(payload []byte) []byte {
	var req registerRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errEnvelope("invalid_request", "decode register request: "+err.Error())
	}
	parsed, err := parseRules(req.ConfigYAML)
	if err != nil {
		return errEnvelope("invalid_config", err.Error())
	}

	configMu.Lock()
	changed := !initialized || !sameRules(activeRules, parsed)
	activeRules = parsed
	initialized = true
	configMu.Unlock()

	// The host re-sends the configuration whenever it re-registers plugins, so
	// only report an actual change.
	if changed {
		logf("registered %d pattern(s) for roles %v", len(parsed.patterns), parsed.roles)
	}

	return okEnvelope(registerResult{
		SchemaVersion: pluginSchemaVersion,
		Metadata: metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
		},
		Capabilities: capabilities{RequestBodyPatcherBefore: true},
	})
}

func handlePatch(body []byte) []byte {
	configMu.RLock()
	current := activeRules
	ready := initialized
	configMu.RUnlock()

	if !ready || len(body) == 0 {
		return okEnvelope(patchResult{})
	}
	edits := current.editsFor(body)
	if len(edits) == 0 {
		return okEnvelope(patchResult{})
	}
	logf("patching %d fragment(s) in a %d byte body", len(edits), len(body))
	return okEnvelope(patchResult{Edits: edits})
}

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return errEnvelope("encode_failed", err.Error())
	}
	out, err := json.Marshal(rpcEnvelope{OK: true, Result: raw})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"encode_failed","message":"envelope"}}`)
	}
	return out
}

func errEnvelope(code, message string) []byte {
	out, err := json.Marshal(rpcEnvelope{OK: false, Error: &rpcEnvelopeErr{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"encode_failed","message":"envelope"}}`)
	}
	return out
}

func logf(format string, args ...any) {
	logger := log.New(os.Stderr, "["+pluginName+"] ", log.LstdFlags)
	logger.Printf(format, args...)
}

// main is required by the toolchain for package main. A c-shared library only
// runs package initialization; the host drives everything through the exported
// C entry points.
func main() {}
