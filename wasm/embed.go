package wasm

import (
	"context"
	_ "embed"
)

//go:embed qoder_auth_wasm_bg.wasm
var embeddedWasm []byte

// NewEmbedded creates a Runtime from the WASM module embedded in this binary,
// so callers don't need to ship qoder_auth_wasm_bg.wasm as a separate file.
func NewEmbedded(ctx context.Context) (*Runtime, error) {
	return NewFromBytes(ctx, embeddedWasm)
}
