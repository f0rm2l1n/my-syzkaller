// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"context"
	"testing"

	"github.com/google/syzkaller/pkg/aflow/backend/openai"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProviderRouting(t *testing.T) {
	server := &Server{cfg: &Config{
		OpenAIBaseURL: "http://localhost:8080/v1",
		OpenAIAPIKey:  "test-key",
		Model:         "test-model",
	}}
	provider, err := server.newLLMProvider(context.Background(), backendOpenAI)
	require.NoError(t, err)
	defer provider.Close()

	openaiProvider, ok := provider.(*openai.Provider)
	require.True(t, ok)
	models, err := openaiProvider.Models(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"test-model"}, models)
}

func TestUnknownProviderRouting(t *testing.T) {
	server := &Server{cfg: &Config{}}
	_, err := server.newLLMProvider(context.Background(), "unknown")
	require.ErrorContains(t, err, `unknown LLM backend "unknown"`)
}
