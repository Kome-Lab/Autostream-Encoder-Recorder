package archive

import (
	"context"
	"time"
)

type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Sleep       func(context.Context, time.Duration) error
}

type RetryUploader struct {
	Inner  ArchiveUploader
	Policy RetryPolicy
}

func (u RetryUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error) {
	inner := u.Inner
	if inner == nil {
		inner = DryRunUploader{}
	}
	policy := u.Policy
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = time.Second
	}
	if policy.Sleep == nil {
		policy.Sleep = sleepContext
	}
	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		result, err := inner.Upload(ctx, streamName, streamID, startedAtJST, files)
		if err == nil {
			result.Attempts = attempt
			return result, nil
		}
		lastErr = err
		if attempt == policy.MaxAttempts {
			break
		}
		delay := policy.BaseDelay * time.Duration(1<<(attempt-1))
		if err := policy.Sleep(ctx, delay); err != nil {
			return UploadResult{}, err
		}
	}
	return UploadResult{Attempts: policy.MaxAttempts}, lastErr
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
