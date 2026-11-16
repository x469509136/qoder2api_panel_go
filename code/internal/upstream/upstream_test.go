package upstream

import (
	"context"
	"strings"
	"testing"
)

func TestStreamEnvelope(t *testing.T) {
	stream := "data: {\"body\":\"{\\\"a\\\":1}\",\"statusCodeValue\":200}\n\n" +
		"data: {\"body\":\"[DONE]\"}\n"
	var seen []string
	err := Stream(context.Background(), strings.NewReader(stream), nil,
		func(inner map[string]any) error {
			seen = append(seen, "chunk")
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("got %d chunks, want 1", len(seen))
	}
}

func TestStreamEnvelopeStatusError(t *testing.T) {
	stream := "data: {\"body\":\"{\\\"code\\\":10605,\\\"isQueued\\\":true}\"," +
		"\"statusCodeValue\":418}\n"
	err := Stream(context.Background(), strings.NewReader(stream), nil,
		func(map[string]any) error { return nil })
	if err == nil {
		t.Fatal("expected an envelope status error")
	}
	ee, ok := err.(*EnvelopeStatusError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if ee.Status != 418 {
		t.Fatalf("status = %d", ee.Status)
	}
	if Wait := QueueWaitSeconds(ee.Detail); Wait == 0 {
		t.Fatal("the queue signal was not detectable in the detail")
	}
}

func TestQueueWaitSeconds(t *testing.T) {
	cases := map[string]int{
		`{"code":"10605","isQueued":true,"retryAfterSeconds":45}`: 45,
		`{"code":"10605","isQueued":true}`:                        30,
		`{"error":"RESOURCE_LIMIT"}`:                              30,
		`{"error":"totally different"}`:                           0,
	}
	for detail, want := range cases {
		if got := QueueWaitSeconds(detail); got != want {
			t.Fatalf("QueueWaitSeconds(%q) = %d, want %d", detail, got, want)
		}
	}
}

func TestTransientClassification(t *testing.T) {
	// A validation error is never worth retrying.
	if IsTransientUpstream(400, "invalid_parameter_error: bad") {
		t.Fatal("a validation error must not be transient")
	}
	// A credential rejection has its own path.
	if IsTransientUpstream(401, "") || IsTransientUpstream(403, "") || IsTransientUpstream(429, "") {
		t.Fatal("auth/rate limits are handled separately")
	}
	if !IsTransientUpstream(502, "provider_error: boom") {
		t.Fatal("502 must be transient")
	}
	if !IsTransientUpstream(418, "provider_error") {
		t.Fatal("418 provider_error must be transient")
	}
	if IsTransientUpstream(500, "DataInspectionFailed") {
		t.Fatal("a content-policy rejection is deterministic, not transient")
	}
}

func TestRequestFault(t *testing.T) {
	if !IsRequestFault(413, "") {
		t.Fatal("413 is a request fault")
	}
	if !IsRequestFault(400, `Range of max_tokens should be [1, 131072]`) {
		t.Fatal("a range violation is a request fault")
	}
	if IsRequestFault(403, "TOKEN_EXPIRE") {
		t.Fatal("an expired credential is an account problem, not a request fault")
	}
}

func TestImageAndFirstTokenTimeout(t *testing.T) {
	if !IsImageConversionTimeout("upstream image conversion timeout after 30s") {
		t.Fatal("image conversion timeout not detected")
	}
	if IsFirstTokenTimeout("upstream image conversion timeout") {
		t.Fatal("an image timeout is not a first-token timeout")
	}
	if !IsFirstTokenTimeout("upstream first token timeout") {
		t.Fatal("first-token timeout not detected")
	}
}

func TestStripDataPrefix(t *testing.T) {
	cases := map[string]string{
		"data: {\"a\":1}": `{"a":1}`,
		"data:data: x":    "x",
		": ping":          "",
		"   ":             "",
		"{\"a\":1}":       `{"a":1}`,
	}
	for in, want := range cases {
		if got := StripDataPrefix(in); got != want {
			t.Fatalf("StripDataPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInnerErrorKind(t *testing.T) {
	cases := map[string]string{
		"DataInspectionFailed":    "content_policy",
		"rate_limit reached":      "rate_limit",
		"invalid_parameter_error": "invalid_request",
		"unauthorized token":      "auth",
		"something else entirely": "other",
		"":                        "",
	}
	for msg, want := range cases {
		if got := InnerErrorKind("", msg); got != want {
			t.Fatalf("InnerErrorKind(%q) = %q, want %q", msg, got, want)
		}
	}
}
