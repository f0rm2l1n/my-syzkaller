// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/google/syzkaller/pkg/aflow/backend/openai"
)

var (
	flagOpenAIBaseURL     = flag.String("openai-base-url", "", "OpenAI-compatible API root (or OPENAI_BASE_URL)")
	flagOpenAITimeout     = flag.Duration("openai-request-timeout", openai.DefaultRequestTimeout, "timeout per model request")
	flagOpenAIMaxOutput   = flag.Int("openai-max-output-tokens", openai.DefaultMaxOutputTokens, "model output token limit")
	flagOpenAIOutputField = flag.String("openai-output-token-field", openai.DefaultOutputTokenField,
		"output limit field: max_tokens or max_completion_tokens")
)

func init() {
	RegisterProvider("openai", func(ctx context.Context, model string) (backend.Provider, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if *flagOpenAITimeout <= 0 || *flagOpenAIMaxOutput <= 0 {
			return nil, fmt.Errorf("openai timeout and output token limit must be positive")
		}
		return openai.NewProvider(openai.Config{
			BaseURL: cmp.Or(*flagOpenAIBaseURL, os.Getenv("OPENAI_BASE_URL")),
			Model:   model, APIKey: os.Getenv("OPENAI_API_KEY"),
			RequestTimeout: *flagOpenAITimeout, MaxOutputTokens: *flagOpenAIMaxOutput,
			OutputTokenField: *flagOpenAIOutputField,
		})
	})
}
