// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package openai

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/stretchr/testify/require"
)

func TestAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   any
	}{
		{400, "context_length_exceeded", &backend.InputTokenOverflowError{}},
		{400, "content_policy_violation", &backend.BlockedError{}},
		{408, "", &backend.RetryError{}},
		{429, "rate_limit_exceeded", &backend.RetryError{}},
		{500, "", &backend.RetryError{}}, {502, "", &backend.RetryError{}}, {503, "", &backend.RetryError{}},
		{429, "insufficient_quota", nil}, {429, "billing_hard_limit_reached", nil},
		{401, "invalid_api_key", nil}, {403, "", nil}, {404, "model_not_found", nil}, {400, "", nil},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.code), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"error"}}`, tc.code))
			err := parseAPIError(tc.status, nil, body, time.Now(), "")
			require.Error(t, err)
			if tc.want != nil {
				require.IsType(t, tc.want, err)
			} else {
				var retry *backend.RetryError
				require.False(t, errors.As(err, &retry))
			}
		})
	}
	// Non-JSON bodies, including proxy pages, never become raw log content.
	err := parseAPIError(502, nil, []byte("private proxy content"), time.Now(), "")
	require.NotContains(t, err.Error(), "private proxy content")
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		delay time.Duration
		ok    bool
	}{
		{"3", 3 * time.Second, true}, {"0", 0, true},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true},
		{now.Add(-time.Second).Format(http.TimeFormat), 0, true},
		{"-1", 0, false}, {"", 0, false}, {"invalid", 0, false}, {"9223372036854775807", 0, false},
	} {
		delay, ok := retryAfter(tc.value, now)
		require.Equal(t, tc.ok, ok)
		require.Equal(t, tc.delay, delay)
	}
}
