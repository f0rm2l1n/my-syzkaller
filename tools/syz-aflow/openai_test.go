// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/stretchr/testify/require"
)

func TestOpenAIRegistration(t *testing.T) {
	oldURL, oldTimeout, oldOutput, oldField := *flagOpenAIBaseURL, *flagOpenAITimeout, *flagOpenAIMaxOutput, *flagOpenAIOutputField
	t.Cleanup(func() {
		*flagOpenAIBaseURL, *flagOpenAITimeout = oldURL, oldTimeout
		*flagOpenAIMaxOutput, *flagOpenAIOutputField = oldOutput, oldField
	})
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://localhost/v1")
	*flagOpenAIBaseURL = ""
	factory := providers["openai"]
	require.NotNil(t, factory)
	get := func(model string) backend.Provider {
		p, err := factory(context.Background(), model)
		require.NoError(t, err)
		t.Cleanup(func() { p.Close() })
		return p
	}
	fromEnv := get("test-model").(backend.CacheIdentityProvider).CacheIdentity()
	t.Setenv("OPENAI_BASE_URL", "not-a-url")
	*flagOpenAIBaseURL = "http://localhost/v1"
	fromFlag := get("test-model").(backend.CacheIdentityProvider).CacheIdentity()
	require.Equal(t, fromEnv, fromFlag)
	_, err := factory(context.Background(), "")
	require.ErrorContains(t, err, "model")
	*flagOpenAITimeout = 0
	_, err = factory(context.Background(), "test-model")
	require.Error(t, err)
	*flagOpenAITimeout = time.Minute
	*flagOpenAIMaxOutput = 0
	_, err = factory(context.Background(), "test-model")
	require.Error(t, err)
}
