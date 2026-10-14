package container

import (
	"fmt"
	"time"
)

// PullRetryConfig configures bounded retries for image pulls.
//
// Only failures classified as transient (rate limiting, connection errors
// and timeouts) are retried. Deterministic failures such as a missing image
// or invalid credentials fail on the first attempt.
type PullRetryConfig struct {
	// MaxAttempts is the total number of pull attempts, including the initial
	// attempt. A value of 1 disables retries.
	MaxAttempts int
	// InitialDelay is the backoff before the second attempt. The backoff
	// doubles after every failed attempt and is capped by MaxDelay. A zero
	// value retries without waiting.
	InitialDelay time.Duration
	// MaxDelay caps the backoff between attempts. A zero value removes the
	// cap.
	MaxDelay time.Duration
}

// DefaultPullRetryConfig is the conservative default: up to three attempts
// (the initial attempt plus two retries), starting with a five second
// backoff and capped at thirty seconds between attempts.
var DefaultPullRetryConfig = PullRetryConfig{
	MaxAttempts:  3,
	InitialDelay: 5 * time.Second,
	MaxDelay:     30 * time.Second,
}

// ValidatePullRetryConfig validates pull retry parameters. It is called at
// parameter parsing time so invalid configuration is reported before any
// workflow runs.
func ValidatePullRetryConfig(cfg PullRetryConfig) error {
	if cfg.MaxAttempts < 1 {
		return fmt.Errorf("max pull attempts must be at least 1, got %d", cfg.MaxAttempts)
	}
	if cfg.InitialDelay < 0 {
		return fmt.Errorf("initial pull retry delay must not be negative, got %s", cfg.InitialDelay)
	}
	if cfg.MaxDelay < 0 {
		return fmt.Errorf("maximum pull retry delay must not be negative, got %s", cfg.MaxDelay)
	}
	if cfg.MaxDelay > 0 && cfg.InitialDelay > cfg.MaxDelay {
		return fmt.Errorf("initial pull retry delay (%s) must not be greater than the maximum pull retry delay (%s)", cfg.InitialDelay, cfg.MaxDelay)
	}
	return nil
}

// normalizePullRetryConfig fills unset fields with conservative defaults.
func normalizePullRetryConfig(cfg PullRetryConfig) PullRetryConfig {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultPullRetryConfig.MaxAttempts
	}
	if cfg.InitialDelay < 0 {
		cfg.InitialDelay = DefaultPullRetryConfig.InitialDelay
	}
	if cfg.MaxDelay < 0 {
		cfg.MaxDelay = DefaultPullRetryConfig.MaxDelay
	}
	return cfg
}

// pullBackoffDelay returns the backoff after the given 1-based failed
// attempt. Attempt 1 waits InitialDelay, every further attempt doubles the
// previous backoff until MaxDelay is reached.
func pullBackoffDelay(attempt int, cfg PullRetryConfig) time.Duration {
	delay := cfg.InitialDelay
	for i := 1; i < attempt; i++ {
		if cfg.MaxDelay > 0 && delay >= cfg.MaxDelay {
			return cfg.MaxDelay
		}
		delay *= 2
		if delay < 0 {
			// Overflow guard for pathological attempt counts.
			if cfg.MaxDelay > 0 {
				return cfg.MaxDelay
			}
			return 1<<63 - 1
		}
	}
	if cfg.MaxDelay > 0 && delay > cfg.MaxDelay {
		return cfg.MaxDelay
	}
	return delay
}

// PullFailureCategory classifies why an image pull failed.
type PullFailureCategory int

const (
	// PullFailureUnknown is a failure that is not known to be retryable.
	PullFailureUnknown PullFailureCategory = iota
	// PullFailureCanceled means the pull was stopped through context cancellation.
	PullFailureCanceled
	// PullFailureNotFound is a deterministic failure: the image or tag does not exist.
	PullFailureNotFound
	// PullFailureUnauthorized is a deterministic failure: credentials are missing or rejected.
	PullFailureUnauthorized
	// PullFailureRateLimited is a transient failure caused by registry rate limiting.
	PullFailureRateLimited
	// PullFailureConnection is a transient network or connection failure.
	PullFailureConnection
	// PullFailureTimeout is a transient network timeout.
	PullFailureTimeout
)

// String returns a stable, human readable name of the category, used in logs
// and in the summarized pull error.
func (c PullFailureCategory) String() string {
	switch c {
	case PullFailureCanceled:
		return "canceled"
	case PullFailureNotFound:
		return "image not found"
	case PullFailureUnauthorized:
		return "unauthorized"
	case PullFailureRateLimited:
		return "rate limit exceeded"
	case PullFailureConnection:
		return "network connection error"
	case PullFailureTimeout:
		return "timeout"
	default:
		return "unknown error"
	}
}

// retryable reports whether a failure in this category should be retried.
func (c PullFailureCategory) retryable() bool {
	return c == PullFailureRateLimited || c == PullFailureConnection || c == PullFailureTimeout
}

// PullError is returned once all pull attempts are exhausted. It records the
// final failure category together with the number of attempts and the time
// spent pulling, so callers can tell rate limiting, network failures and
// timeouts apart.
type PullError struct {
	Image    string
	Platform string
	// Category is the category of the last failed attempt.
	Category PullFailureCategory
	// Attempts is the number of attempts that were made.
	Attempts int
	// Elapsed is the total time spent across all attempts and backoffs.
	Elapsed time.Duration

	lastError error
}

func (e *PullError) Error() string {
	return fmt.Sprintf("pull image '%s' (%s) failed after %d attempt(s) over %s: %s: %s",
		e.Image, e.Platform, e.Attempts, e.Elapsed.Round(time.Millisecond), e.Category, e.lastError.Error())
}

// Unwrap exposes the last underlying error.
func (e *PullError) Unwrap() error {
	return e.lastError
}
