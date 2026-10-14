//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"

	"github.com/nektos/act/pkg/common"
)

// Error message fragments used to classify registry/daemon failures that
// only reach us as plain text. The Docker API client types the HTTP status
// for the initial request, but failures happening inside the daemon or
// inside the JSON progress stream only carry a message.
var (
	pullRateLimitMarkers = []string{
		"toomanyrequests",
		"too many requests",
		"pull rate limit",
		"rate limit exceeded",
		"rate-limited",
		"http 429",
		"status 429",
	}
	pullNotFoundMarkers = []string{
		"manifest unknown",
		"no such image",
		"not found",
		"does not exist",
	}
	pullUnauthorizedMarkers = []string{
		"unauthorized",
		"authentication required",
		"unauthenticated",
		"incorrect username or password",
		"403 forbidden",
		"requested access to the resource is denied",
	}
	pullTimeoutMarkers = []string{
		"context deadline exceeded",
		"deadline exceeded",
		"i/o timeout",
		"timed out",
		"timeout",
		"client.timeout exceeded",
	}
	pullConnectionMarkers = []string{
		"connection reset",
		"connection refused",
		"connection aborted",
		"broken pipe",
		"no such host",
		"dial tcp",
		"dial unix",
		"network is unreachable",
		"no route to host",
		"unexpected eof",
		"eof",
		"use of closed network connection",
		"tls handshake",
		"handshake failure",
		"server sent goaway",
		"http2: ",
		"server misbehaving",
		"request canceled",
		"temporarily unavailable",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"error during connect",
		"cannot connect to the docker daemon",
	}
)

func containsAny(msg string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isNetworkError reports whether err is a transport level network error,
// including dial failures, resets and EOFs that may be wrapped by the http
// client or url.Error.
func isNetworkError(err error) bool {
	switch {
	case errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.ECONNABORTED),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed):
		return true
	}

	// net.Error is implemented by *net.OpError, *net.DNSError and *url.Error,
	// so this also covers errors wrapped by the http client.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// classifyPullFailure assigns a pull failure to a retryability category.
func classifyPullFailure(err error) PullFailureCategory {
	if err == nil {
		return PullFailureUnknown
	}

	switch {
	case errors.Is(err, context.Canceled), cerrdefs.IsCanceled(err):
		return PullFailureCanceled
	case errors.Is(err, context.DeadlineExceeded), cerrdefs.IsDeadlineExceeded(err):
		return PullFailureTimeout
	// HTTP 429 as returned by the registry through the Docker API client.
	case cerrdefs.IsResourceExhausted(err):
		return PullFailureRateLimited
	case cerrdefs.IsNotFound(err):
		return PullFailureNotFound
	case cerrdefs.IsUnauthorized(err), cerrdefs.IsPermissionDenied(err):
		return PullFailureUnauthorized
	}

	// Failure to even reach the Docker daemon.
	if client.IsErrConnectionFailed(err) {
		return PullFailureConnection
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return PullFailureTimeout
	}
	if isNetworkError(err) {
		return PullFailureConnection
	}

	// Plain text failures, matched in a deterministic-before-transient
	// order to avoid retrying missing images or rejected credentials.
	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, pullRateLimitMarkers...):
		return PullFailureRateLimited
	case containsAny(msg, pullNotFoundMarkers...):
		return PullFailureNotFound
	case containsAny(msg, pullUnauthorizedMarkers...):
		return PullFailureUnauthorized
	case containsAny(msg, pullTimeoutMarkers...):
		return PullFailureTimeout
	case containsAny(msg, pullConnectionMarkers...):
		return PullFailureConnection
	// HTTP 503 without an informative message.
	case cerrdefs.IsUnavailable(err):
		return PullFailureConnection
	}

	return PullFailureUnknown
}

// pullAttempt performs a single image pull and returns the classified
// category together with its error. Attempts are numbered starting at 1.
type pullAttempt func(ctx context.Context, attempt int) (PullFailureCategory, error)

// runPullWithRetry runs the bounded retry loop. Deterministic failures and
// cancellation are returned immediately (and unwrapped), while transient
// failures are retried after the configured exponential backoff. Once the
// attempts are exhausted a *PullError summarizes the outcome.
func runPullWithRetry(ctx context.Context, logger logrus.FieldLogger, imageRef, platform string, cfg PullRetryConfig, doAttempt pullAttempt) error {
	start := time.Now()
	var lastErr error
	lastCategory := PullFailureUnknown

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		category, err := doAttempt(ctx, attempt)
		if err == nil {
			if attempt > 1 {
				logger.Infof("pull of image '%s' (%s) succeeded on attempt %d/%d", imageRef, platform, attempt, cfg.MaxAttempts)
			}
			return nil
		}
		lastErr, lastCategory = err, category

		// A canceled or expired context stops the loop immediately, no
		// further retry is scheduled.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !category.retryable() {
			// Deterministic failure: return the original error unchanged so
			// the error text is identical to a single attempt pull.
			return err
		}
		if attempt >= cfg.MaxAttempts {
			return &PullError{
				Image:     imageRef,
				Platform:  platform,
				Category:  category,
				Attempts:  attempt,
				Elapsed:   time.Since(start),
				lastError: err,
			}
		}

		delay := pullBackoffDelay(attempt, cfg)
		logger.Warnf("pull attempt %d/%d for image '%s' (%s) failed (%s): %s; waiting %s before next attempt",
			attempt, cfg.MaxAttempts, imageRef, platform, category, err.Error(), delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return &PullError{
		Image:     imageRef,
		Platform:  platform,
		Category:  lastCategory,
		Attempts:  cfg.MaxAttempts,
		Elapsed:   time.Since(start),
		lastError: lastErr,
	}
}

// imagePuller is the slice of the Docker API client used by the pull logic.
// client.APIClient satisfies it; the narrow interface allows faking pulls in
// tests.
type imagePuller interface {
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// doImagePullWithRetries prepares pull options and drives the retry loop.
func doImagePullWithRetries(ctx context.Context, logger logrus.FieldLogger, puller imagePuller, imageRef string, input NewDockerPullExecutorInput, cfg PullRetryConfig) error {
	imagePullOptions, err := getImagePullOptions(ctx, input)
	if err != nil {
		return err
	}
	hadCredentials := imagePullOptions.RegistryAuth != ""

	doAttempt := func(ctx context.Context, attempt int) (PullFailureCategory, error) {
		// The stale-credential fallback is part of the first attempt only:
		// an "unauthorized" response with credentials is followed right
		// away by one pull without credentials, without backoff and without
		// consuming the retry budget. Later attempts run without
		// credentials once they proved stale.
		err := imagePullOnce(ctx, logger, puller, imageRef, input.Platform, &imagePullOptions, attempt == 1 && hadCredentials)
		return classifyPullFailure(err), err
	}

	return runPullWithRetry(ctx, logger, imageRef, input.Platform, cfg, doAttempt)
}

// imagePullOnce performs a single ImagePull call against the daemon and
// drains its progress stream. On the first attempt it preserves the
// established stale-credential fallback: an "unauthorized" failure while
// credentials were supplied is immediately retried once without them.
func imagePullOnce(ctx context.Context, logger logrus.FieldLogger, puller imagePuller, imageRef, platform string, imagePullOptions *client.ImagePullOptions, allowCredentialFallback bool) error {
	reader, err := puller.ImagePull(ctx, imageRef, *imagePullOptions)
	responseErr := logDockerResponse(logger, reader, err != nil)
	if err == nil {
		err = responseErr
	}

	if err != nil && allowCredentialFallback && imagePullOptions.RegistryAuth != "" && strings.Contains(err.Error(), "unauthorized") {
		logger.Errorf("pulling image '%v' (%s) failed with credentials %s retrying without them, please check for stale docker config files", imageRef, platform, err.Error())
		imagePullOptions.RegistryAuth = ""
		reader, err = puller.ImagePull(ctx, imageRef, *imagePullOptions)
		responseErr = logDockerResponse(logger, reader, err != nil)
		if err == nil {
			err = responseErr
		}
	}

	return err
}

// pullFlight represents an in-progress pull shared by concurrent callers.
type pullFlight struct {
	done chan struct{}
	// waiters is the number of concurrent callers waiting for this pull.
	waiters int
	err     error
}

var (
	pullFlightsMu sync.Mutex
	pullFlights   = map[string]*pullFlight{}
)

func pullFlightKey(imageRef, platform string) string {
	return imageRef + "\x00" + platform
}

// joinPullFlight deduplicates concurrent pulls of the same image and
// platform within this process. The first caller runs pull, every other
// concurrent caller waits for its result and receives the identical error.
// Once the in-progress pull finishes (success or failure), later pulls run
// again; this is not a cross-run cache.
func joinPullFlight(ctx context.Context, imageRef, platform string, pull func(context.Context) error) error {
	key := pullFlightKey(imageRef, platform)

	pullFlightsMu.Lock()
	if flight, ok := pullFlights[key]; ok {
		flight.waiters++
		pullFlightsMu.Unlock()
		common.Logger(ctx).Debugf("pull for image '%s' (%s) is already in progress, waiting for its result", imageRef, platform)
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	flight := &pullFlight{done: make(chan struct{})}
	pullFlights[key] = flight
	pullFlightsMu.Unlock()

	flight.err = pull(ctx)
	close(flight.done)

	pullFlightsMu.Lock()
	if current, ok := pullFlights[key]; ok && current == flight {
		delete(pullFlights, key)
	}
	pullFlightsMu.Unlock()

	return flight.err
}
