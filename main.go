// Command commandcode-provider builds the CLIProxyAPI native plugin library
// exposing the CommandCode provider. This file is CGO glue only: every host
// RPC method is forwarded verbatim into internal/plugin.Manager.
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

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/RNGCHEER/cpa-plugin-commandcode-provider/internal/plugin"
)

func main() {}

// abiVersion is the native C ABI shape this plugin exports.
const abiVersion uint32 = pluginabi.ABIVersion

// maxRequestLen bounds buffer lengths before size_t -> C.int conversion:
// values >= 2^31 truncate NEGATIVE and C.GoBytes would panic (makeSlice with
// a negative length) outside any recover, killing the host process.
const maxRequestLen = 1<<31 - 1

var (
	dispatcherOnce sync.Once
	dispatcher     *plugin.Manager
)

// manager lazily builds the dispatcher once the first call arrives; the host
// always calls plugin.register first, but a mis-ordered probe must not nil-
// deref.
func manager() *plugin.Manager {
	dispatcherOnce.Do(func() {
		dispatcher = plugin.NewManager(callHost)
	})
	return dispatcher
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plug *C.cliproxy_plugin_api) C.int {
	if host == nil || host.abi_version != C.uint32_t(abiVersion) {
		return 1
	}
	if plug == nil {
		return 1
	}
	C.store_host_api(host)
	plug.abi_version = C.uint32_t(abiVersion)
	plug.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plug.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plug.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, plugin.ErrEnvelope("invalid_method", "method is required"))
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		if requestLen > maxRequestLen {
			writeResponse(response, plugin.ErrEnvelope("plugin_error", "request exceeds maximum size"))
			return 0
		}
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := manager().HandleCall(C.GoString(method), req)
	if err != nil {
		writeResponse(response, plugin.ErrEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	if dispatcher != nil {
		_, _ = dispatcher.HandleCall("plugin.shutdown", nil)
	}
}

// callHost bridges the plugin's outbound callbacks to the stored C host API.
// The error text carries only the method name and return code - no payload
// material ever reaches logs or errors.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		if len(payload) > maxRequestLen {
			return nil, fmt.Errorf("host callback %s: payload too large", method)
		}
		cPayload := C.CBytes(payload)
		if cPayload == nil {
			return nil, fmt.Errorf("host callback %s: allocation failed", method)
		}
		defer C.free(cPayload)
		req = (*C.uint8_t)(cPayload)
	}
	var resp C.cliproxy_buffer
	rc := C.call_host_api(cMethod, req, C.size_t(len(payload)), &resp)
	if resp.ptr != nil && resp.len > 0 && resp.len <= maxRequestLen {
		out := C.GoBytes(resp.ptr, C.int(resp.len))
		C.free_host_buffer(resp.ptr, resp.len)
		if rc != 0 {
			return out, fmt.Errorf("host callback %s failed (rc=%d)", method, int(rc))
		}
		return out, nil
	}
	if resp.ptr != nil {
		C.free_host_buffer(resp.ptr, resp.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, int(rc))
	}
	return nil, fmt.Errorf("host callback %s returned empty buffer", method)
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 || len(raw) > maxRequestLen {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
