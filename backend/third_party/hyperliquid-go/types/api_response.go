package types

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/valyala/fastjson"
)

// Pool of parsers to avoid allocations
var parserPool = sync.Pool{
	New: func() any {
		return &fastjson.Parser{}
	},
}

// APIResponse is the generic envelope returned by /exchange and /info
// endpoints. Ok reports whether the server returned status:"ok". On
// failure the human-readable message lives in Err; on success the
// payload is decoded into Data and the wire type tag into Type.
type APIResponse[T any] struct {
	Status string
	Data   T
	Type   string
	Err    string
	Ok     bool
}

// UnmarshalJSON parses the Hyperliquid response envelope into r,
// promoting nested response.data into r.Data and shaping error fields
// when status is not "ok".
func (r *APIResponse[T]) UnmarshalJSON(data []byte) error {
	// Get parser from pool
	parser := parserPool.Get().(*fastjson.Parser)
	defer parserPool.Put(parser)

	parsed, err := parser.ParseBytes(data)
	if err != nil {
		return fmt.Errorf("failed to parse JSON response: %w", err)
	}

	// Get status
	r.Status = string(parsed.GetStringBytes("status"))
	r.Ok = r.Status == "ok"

	if !r.Ok {
		// When status is not "ok", "response" is usually a string error message
		r.Err = string(parsed.GetStringBytes("response"))
		return nil
	}

	// When status is "ok", "response" contains "type" and "data"
	r.Type = string(parsed.GetStringBytes("response", "type"))

	// Check if response.data exists (nested structure)
	if parsed.Exists("response", "data") {
		// The data is nested under response.data
		dataBytes := parsed.Get("response", "data").MarshalTo(nil)
		if err := json.Unmarshal(dataBytes, &r.Data); err != nil {
			return fmt.Errorf("failed to unmarshal response.data: %w", err)
		}
	} else if parsed.Exists("response") {
		// The data is directly under response
		responseBytes := parsed.Get("response").MarshalTo(nil)
		if err := json.Unmarshal(responseBytes, &r.Data); err != nil {
			return fmt.Errorf("failed to unmarshal response: %w", err)
		}
	} else {
		return fmt.Errorf("missing response.data field in successful response")
	}

	return nil
}
