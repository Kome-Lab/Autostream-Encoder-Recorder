package archive

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakyUploader struct {
	failures int
	calls    int
}

func (u *flakyUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error) {
	u.calls++
	if u.calls <= u.failures {
		return UploadResult{}, errors.New("transient")
	}
	return UploadResult{DryRun: true, FileIDs: map[string]string{"final.mp4": "ok"}}, nil
}

func TestRetryUploader(t *testing.T) {
	flaky := &flakyUploader{failures: 2}
	uploader := RetryUploader{Inner: flaky, Policy: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}}
	result, err := uploader.Upload(context.Background(), "stream", "s1", time.Now(), []File{{LocalPath: "final.mp4", DrivePath: "final.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts != 3 || flaky.calls != 3 {
		t.Fatalf("unexpected attempts result=%#v calls=%d", result, flaky.calls)
	}
}

func TestRetryUploaderReturnsLastError(t *testing.T) {
	flaky := &flakyUploader{failures: 5}
	uploader := RetryUploader{Inner: flaky, Policy: RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}}
	result, err := uploader.Upload(context.Background(), "stream", "s1", time.Now(), []File{{LocalPath: "final.mp4", DrivePath: "final.mp4"}})
	if err == nil {
		t.Fatal("expected retry failure")
	}
	if result.Attempts != 2 {
		t.Fatalf("unexpected attempts: %#v", result)
	}
}
