package mcpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// failure is an HTTP-level outcome with a small, fixed JSON-RPC error body.
// Nothing derived from an internal error ever reaches the body.
type failure struct {
	status     int
	rpcCode    int64
	code       string
	message    string
	retryAfter int // seconds; 0 omits the header
	close      bool
}

var (
	failForbidden      = failure{status: http.StatusForbidden, rpcCode: -32003, code: "forbidden", message: "Host or Origin is not the private MCP origin"}
	failUnauthorized   = failure{status: http.StatusUnauthorized, rpcCode: -32001, code: "unauthorized", message: "A valid private integration bearer credential is required"}
	failBusy           = failure{status: http.StatusServiceUnavailable, rpcCode: -32000, code: "server_busy", message: "Server is busy", retryAfter: 1}
	failUnavailable    = failure{status: http.StatusServiceUnavailable, rpcCode: -32000, code: "unavailable", message: "Service temporarily unavailable", retryAfter: 1}
	failTimeout        = failure{status: http.StatusServiceUnavailable, rpcCode: -32000, code: "timeout", message: "Request exceeded the five-second budget", retryAfter: 1, close: true}
	failRequestTimeout = failure{status: http.StatusRequestTimeout, rpcCode: -32600, code: "request_timeout", message: "Request body was not received in time", close: true}
	failTooLarge       = failure{status: http.StatusRequestEntityTooLarge, rpcCode: -32600, code: "request_too_large", message: "Request body exceeds 32 KiB", close: true}
	failBadRequest     = failure{status: http.StatusBadRequest, rpcCode: -32600, code: "invalid_request", message: "Request body could not be read"}
	failBatch          = failure{status: http.StatusBadRequest, rpcCode: -32600, code: "batch_not_supported", message: "JSON-RPC batches are not supported"}
	failVersionHeader  = failure{status: http.StatusBadRequest, rpcCode: -32600, code: "protocol_version_required", message: "Exactly one MCP-Protocol-Version header is required after initialization"}
	failConcurrency    = failure{status: http.StatusTooManyRequests, rpcCode: -32029, code: "concurrency_limited", message: "Another call with this credential is in progress", retryAfter: 1}
	failMethodNotFound = failure{status: http.StatusOK, rpcCode: -32601, code: "method_not_found", message: "Method not supported by this read-only server"}
	failResultTooLarge = failure{status: http.StatusOK, rpcCode: -32000, code: "result_too_large", message: "The complete response would exceed 256 KiB"}
)

func rateLimited(retryAfterSeconds int) failure {
	return failure{status: http.StatusTooManyRequests, rpcCode: -32029, code: "rate_limited", message: "Rate limit exceeded", retryAfter: retryAfterSeconds}
}

// writeFailure commits a complete safe response. id is the request's JSON-RPC
// ID when known; otherwise the body carries "id":null.
func writeFailure(w http.ResponseWriter, f failure, id jsonrpc.ID) {
	var rawID any
	if id.IsValid() {
		rawID = id.Raw()
	}
	data, _ := json.Marshal(struct {
		Code string `json:"code"`
	}{f.code})
	body, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   any    `json:"error"`
	}{"2.0", rawID, &jsonrpc.Error{Code: f.rpcCode, Message: f.message, Data: data}})
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	if f.status == http.StatusUnauthorized {
		header.Set("WWW-Authenticate", `Bearer realm="baby-tracker-mcp"`)
	}
	if f.retryAfter > 0 {
		header.Set("Retry-After", strconv.Itoa(f.retryAfter))
	}
	if f.close {
		header.Set("Connection", "close")
	}
	w.WriteHeader(f.status)
	_, _ = w.Write(body)
}

// toolMessages are the fixed, client-facing texts for typed tool errors.
var toolMessages = map[string]string{
	"invalid_arguments": "Arguments do not match the tool input schema: baby_id is a lowercase UUID; start and end are RFC 3339 instants; kinds, limit, cursor and timezone follow the schema.",
	"invalid_range":     "start must be before end and both must be between 1900 and 2200.",
	"range_too_long":    "The range may span at most 744 elapsed hours (31 days).",
	"invalid_limit":     "limit must be between 1 and 100.",
	"invalid_kind":      "kinds may contain only diaper, feed, measurement, sleep and supplement.",
	"invalid_baby_id":   "baby_id must be a lowercase UUID from list_babies.",
	"invalid_timezone":  "timezone must be an IANA time zone name such as Europe/Zurich or UTC.",
	"baby_not_found":    "No live baby with this baby_id exists in the family; call list_babies.",
	"invalid_cursor":    "The cursor is not valid for this exact query; restart without a cursor.",
	"cursor_expired":    "The cursor expired; restart from the first page without a cursor.",
	"history_changed":   "The family's data changed since the cursor was issued; restart from the first page without a cursor.",
	"result_too_large":  "The complete result would exceed 256 KiB; narrow the range or kinds. Nothing was truncated or skipped.",
	"unavailable":       "The service is temporarily unavailable; retry later.",
	"data_invalid":      "A stored record failed validation, so no partial result is returned. Ask the server operator to check the data.",
}

// toolError is a typed tool failure: text content only, never structured
// content, so clients do not validate it against the output schema.
func toolError(code string) *mcp.CallToolResult {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": toolMessages[code]}})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}, IsError: true}
}

// readFailure maps a family-read error by code or sentinel only. Authority,
// availability and deadline outcomes become HTTP statuses at final commit;
// domain outcomes become typed tool errors.
func readFailure(ctx context.Context, state *requestState, err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, authority.ErrIntegrationUnauthorized):
		state.fail(failUnauthorized)
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		state.fail(failTimeout)
	default:
		if code := familyread.Code(err); toolMessages[code] != "" {
			return toolError(code)
		}
		state.fail(failUnavailable)
	}
	// The recorded HTTP failure replaces this body at final commit.
	return toolError("unavailable")
}
