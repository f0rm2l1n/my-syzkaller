// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/syzkaller/pkg/aflow/backend"
)

func isTemporary(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func parseAPIError(status int, header http.Header, body []byte, now time.Time, key string) error {
	var response struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	// Unknown/non-JSON bodies are intentionally not included in logs.
	_ = json.Unmarshal(body, &response)
	var code string
	_ = json.Unmarshal(response.Error.Code, &code)
	message := response.Error.Message
	if key != "" {
		message = strings.ReplaceAll(message, key, "[redacted]")
		code = strings.ReplaceAll(code, key, "[redacted]")
		response.Error.Type = strings.ReplaceAll(response.Error.Type, key, "[redacted]")
	}
	if len(message) > 1024 {
		message = message[:1024] + "..."
	}
	err := fmt.Errorf("openai HTTP %d (%s/%s): %s", status, code, response.Error.Type, message)
	for _, value := range []string{code, response.Error.Type} {
		switch value {
		case "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached", "billing_not_active":
			return err
		case "context_length_exceeded", "input_token_limit_exceeded":
			return &backend.InputTokenOverflowError{Err: err}
		case "content_filter", "content_policy_violation":
			return &backend.BlockedError{Err: err}
		}
	}
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599 {
		if delay, ok := retryAfter(header.Get("Retry-After"), now); ok {
			return &backend.RetryError{Delay: delay, Err: err}
		}
		return &backend.RetryError{Delay: time.Second, IsExponential: true, Err: err}
	}
	return err
}

func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 &&
		seconds <= int64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now)), true
	}
	return 0, false
}
