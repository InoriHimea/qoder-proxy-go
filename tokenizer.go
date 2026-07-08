package main

import (
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// Qoder's upstream models aren't OpenAI models, so there's no exact BPE
// table for them. cl100k_base (the gpt-4/gpt-3.5-turbo encoding) is used as
// a universal approximation — close enough for usage/dashboard display,
// not billed against. tiktoken-go-loader embeds the BPE rank files via
// go:embed, so this needs no network access at runtime.
var (
	tokenEncOnce sync.Once
	tokenEnc     *tiktoken.Tiktoken
)

func getTokenEncoder() *tiktoken.Tiktoken {
	tokenEncOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			AddSystemLog("Failed to load token encoder, falling back to length/4 estimate: "+err.Error(), "error", "system")
			return
		}
		tokenEnc = enc
	})
	return tokenEnc
}

// countTokens returns the token count for s using the cl100k_base encoding.
// Falls back to a length/4 estimate if the encoder failed to load.
func countTokens(s string) int {
	if s == "" {
		return 0
	}
	enc := getTokenEncoder()
	if enc == nil {
		return len(s) / 4
	}
	return len(enc.Encode(s, nil, nil))
}
