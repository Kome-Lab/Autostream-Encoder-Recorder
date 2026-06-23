package ingesttoken

import (
	"strings"
	"testing"
	"time"
)

func TestVerifyRejectsServiceIDMismatch(t *testing.T) {
	const secret = "test-signing-secret"
	token, err := Issue(secret, Claims{
		StreamID:    "stream-01",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "worker_events",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(secret, token, Expected{
		StreamID:    "stream-01",
		ServiceID:   "worker-02",
		ServiceType: "worker",
		Purpose:     "worker_events",
		Audience:    "encoder_recorder",
	})
	if err == nil || !strings.Contains(err.Error(), "service id mismatch") {
		t.Fatalf("expected service id mismatch, got %v", err)
	}
}
