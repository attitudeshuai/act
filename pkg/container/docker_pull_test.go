package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"

	log "github.com/sirupsen/logrus"
	assert "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestCleanImage(t *testing.T) {
	tables := []struct {
		imageIn  string
		imageOut string
	}{
		{"myhost.com/foo/bar", "myhost.com/foo/bar"},
		{"localhost:8000/canonical/ubuntu", "localhost:8000/canonical/ubuntu"},
		{"localhost/canonical/ubuntu:latest", "localhost/canonical/ubuntu:latest"},
		{"localhost:8000/canonical/ubuntu:latest", "localhost:8000/canonical/ubuntu:latest"},
		{"ubuntu", "docker.io/library/ubuntu"},
		{"ubuntu:18.04", "docker.io/library/ubuntu:18.04"},
		{"cibuilds/hugo:0.53", "docker.io/cibuilds/hugo:0.53"},
	}

	for _, table := range tables {
		imageOut := cleanImage(context.Background(), table.imageIn)
		assert.Equal(t, table.imageOut, imageOut)
	}
}

func TestGetImagePullOptions(t *testing.T) {
	ctx := context.Background()

	config.SetDir("/non-existent/docker")

	options, err := getImagePullOptions(ctx, NewDockerPullExecutorInput{})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "", options.RegistryAuth, "RegistryAuth should be empty if no username or password is set")

	options, err = getImagePullOptions(ctx, NewDockerPullExecutorInput{
		Image:    "",
		Username: "username",
		Password: "password",
	})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "eyJ1c2VybmFtZSI6InVzZXJuYW1lIiwicGFzc3dvcmQiOiJwYXNzd29yZCJ9", options.RegistryAuth, "Username and Password should be provided")

	config.SetDir("testdata/docker-pull-options")

	options, err = getImagePullOptions(ctx, NewDockerPullExecutorInput{
		Image: "nektos/act",
	})
	assert.Nil(t, err, "Failed to create ImagePullOptions")
	assert.Equal(t, "eyJ1c2VybmFtZSI6InVzZXJuYW1lIiwicGFzc3dvcmQiOiJwYXNzd29yZFxuIiwic2VydmVyYWRkcmVzcyI6Imh0dHBzOi8vaW5kZXguZG9ja2VyLmlvL3YxLyJ9", options.RegistryAuth, "RegistryAuth should be taken from local docker config")
}

func TestValidatePullRetryConfig(t *testing.T) {
	tables := []struct {
		name    string
		cfg     PullRetryConfig
		wantErr bool
	}{
		{"single attempt", PullRetryConfig{MaxAttempts: 1}, false},
		{"multiple attempts", DefaultPullRetryConfig, false},
		{"zero delays", PullRetryConfig{MaxAttempts: 3, InitialDelay: 0, MaxDelay: 0}, false},
		{"zero attempts", PullRetryConfig{MaxAttempts: 0}, true},
		{"negative attempts", PullRetryConfig{MaxAttempts: -1}, true},
		{"negative initial delay", PullRetryConfig{MaxAttempts: 3, InitialDelay: -time.Second}, true},
		{"negative max delay", PullRetryConfig{MaxAttempts: 3, MaxDelay: -time.Second}, true},
		{"initial greater than max", PullRetryConfig{MaxAttempts: 3, InitialDelay: 30 * time.Second, MaxDelay: 5 * time.Second}, true},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			if table.wantErr {
				assert.Error(t, ValidatePullRetryConfig(table.cfg))
			} else {
				assert.NoError(t, ValidatePullRetryConfig(table.cfg))
			}
		})
	}
}

func TestNormalizePullRetryConfig(t *testing.T) {
	// An unset config gets the conservative default number of attempts;
	// zero delays stay zero as they mean "retry without waiting".
	assert.Equal(t, PullRetryConfig{MaxAttempts: DefaultPullRetryConfig.MaxAttempts}, normalizePullRetryConfig(PullRetryConfig{}))

	normalized := normalizePullRetryConfig(PullRetryConfig{MaxAttempts: 5, InitialDelay: -1, MaxDelay: -1})
	assert.Equal(t, 5, normalized.MaxAttempts)
	assert.Equal(t, DefaultPullRetryConfig.InitialDelay, normalized.InitialDelay)
	assert.Equal(t, DefaultPullRetryConfig.MaxDelay, normalized.MaxDelay)

	// Explicit values are preserved as-is.
	explicit := PullRetryConfig{MaxAttempts: 2, InitialDelay: time.Second, MaxDelay: 4 * time.Second}
	assert.Equal(t, explicit, normalizePullRetryConfig(explicit))
}

func TestPullBackoffDelay(t *testing.T) {
	cfg := PullRetryConfig{MaxAttempts: 5, InitialDelay: 5 * time.Second, MaxDelay: 30 * time.Second}

	assert.Equal(t, 5*time.Second, pullBackoffDelay(1, cfg))
	assert.Equal(t, 10*time.Second, pullBackoffDelay(2, cfg))
	assert.Equal(t, 20*time.Second, pullBackoffDelay(3, cfg))
	assert.Equal(t, 30*time.Second, pullBackoffDelay(4, cfg))
	assert.Equal(t, 30*time.Second, pullBackoffDelay(5, cfg))

	// Zero initial delay retries without waiting.
	assert.Equal(t, time.Duration(0), pullBackoffDelay(3, PullRetryConfig{MaxAttempts: 5}))
}

func TestClassifyPullFailure(t *testing.T) {
	connReset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	tables := []struct {
		name string
		err  error
		want PullFailureCategory
	}{
		{"nil", nil, PullFailureUnknown},
		{"canceled", context.Canceled, PullFailureCanceled},
		{"wrapped canceled", fmt.Errorf("pull: %w", context.Canceled), PullFailureCanceled},
		{"deadline", context.DeadlineExceeded, PullFailureTimeout},
		{
			"typed rate limited",
			fmt.Errorf("Error response from daemon: %w", cerrdefs.ErrResourceExhausted),
			PullFailureRateLimited,
		},
		{
			"typed not found",
			fmt.Errorf("Error response from daemon: %w", cerrdefs.ErrNotFound),
			PullFailureNotFound,
		},
		{
			"typed unauthorized",
			fmt.Errorf("Error response from daemon: %w", cerrdefs.ErrUnauthenticated),
			PullFailureUnauthorized,
		},
		{
			"typed forbidden",
			fmt.Errorf("Error response from daemon: %w", cerrdefs.ErrPermissionDenied),
			PullFailureUnauthorized,
		},
		{
			"typed unavailable",
			fmt.Errorf("Error response from daemon: %w", cerrdefs.ErrUnavailable),
			PullFailureConnection,
		},
		{
			"docker hub rate limit text",
			errors.New("Error response from daemon: toomanyrequests: You have reached your pull rate limit. You may increase the limit by authenticating and upgrading"),
			PullFailureRateLimited,
		},
		{
			"manifest unknown",
			errors.New("Error response from daemon: manifest for example.com/missing:latest not found: manifest unknown: manifest unknown"),
			PullFailureNotFound,
		},
		{
			"repository does not exist",
			errors.New("Error response from daemon: pull access denied for missing-image, repository does not exist or may require 'docker login': denied: requested access to the resource is denied"),
			PullFailureNotFound,
		},
		{
			"unauthorized text",
			errors.New("Error response from daemon: unauthorized: authentication required"),
			PullFailureUnauthorized,
		},
		{
			"denied credentials",
			errors.New("Error response from daemon: Get \"https://registry.example.com/v2/\": denied: requested access to the resource is denied"),
			PullFailureUnauthorized,
		},
		{
			"connection reset",
			fmt.Errorf("Error response from daemon: Get \"https://registry-1.docker.io/v2/\": %w", connReset),
			PullFailureConnection,
		},
		{
			"no such host",
			errors.New("Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: lookup registry-1.docker.io: no such host"),
			PullFailureConnection,
		},
		{
			"unexpected eof while downloading",
			errors.New("unexpected EOF"),
			PullFailureConnection,
		},
		{
			"daemon unreachable",
			errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"),
			PullFailureConnection,
		},
		{
			"i/o timeout",
			errors.New("Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: TLS handshake timeout"),
			PullFailureTimeout,
		},
		{
			"deadline text",
			errors.New("http.Client: context deadline exceeded"),
			PullFailureTimeout,
		},
		{
			"unknown error",
			errors.New("Error response from daemon: something entirely unexpected happened"),
			PullFailureUnknown,
		},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			assert.Equal(t, table.want, classifyPullFailure(table.err))
		})
	}
}

func newTestPullLogger() *log.Logger {
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	return logger
}

func TestRunPullWithRetrySucceedsImmediately(t *testing.T) {
	calls := 0
	err := runPullWithRetry(context.Background(), newTestPullLogger(), "image", "linux/amd64",
		PullRetryConfig{MaxAttempts: 3},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			return PullFailureUnknown, nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRunPullWithRetryRecovers(t *testing.T) {
	calls := 0
	err := runPullWithRetry(context.Background(), newTestPullLogger(), "image", "linux/amd64",
		PullRetryConfig{MaxAttempts: 3},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			if calls == 1 {
				return PullFailureConnection, errors.New("unexpected EOF")
			}
			return PullFailureUnknown, nil
		})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestRunPullWithRetryDeterministicFailureIsImmediate(t *testing.T) {
	notFound := errors.New("Error response from daemon: manifest unknown")
	calls := 0
	err := runPullWithRetry(context.Background(), newTestPullLogger(), "image", "linux/amd64",
		PullRetryConfig{MaxAttempts: 3},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			return PullFailureNotFound, notFound
		})
	assert.Equal(t, notFound, err, "deterministic failures must be returned with their original text")
	assert.Equal(t, 1, calls)
}

func TestRunPullWithRetryUnknownFailureIsImmediate(t *testing.T) {
	unknown := errors.New("Error response from daemon: something unexpected")
	calls := 0
	err := runPullWithRetry(context.Background(), newTestPullLogger(), "image", "linux/amd64",
		PullRetryConfig{MaxAttempts: 3},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			return PullFailureUnknown, unknown
		})
	assert.Equal(t, unknown, err)
	assert.Equal(t, 1, calls)
}

func TestRunPullWithRetryExhausted(t *testing.T) {
	start := time.Now()
	calls := 0
	err := runPullWithRetry(context.Background(), newTestPullLogger(), "docker.io/library/ubuntu", "linux/amd64",
		PullRetryConfig{MaxAttempts: 3},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			return PullFailureRateLimited, errors.New("Error response from daemon: toomanyrequests: rate limit")
		})
	require.Error(t, err)

	var pullErr *PullError
	require.ErrorAs(t, err, &pullErr)
	assert.Equal(t, PullFailureRateLimited, pullErr.Category)
	assert.Equal(t, 3, pullErr.Attempts)
	assert.Equal(t, 3, calls)
	assert.GreaterOrEqual(t, time.Since(start), pullErr.Elapsed)
	assert.Contains(t, pullErr.Error(), "3 attempt(s)")
	assert.Contains(t, pullErr.Error(), "rate limit exceeded")
	assert.Contains(t, pullErr.Error(), "docker.io/library/ubuntu")
}

func TestRunPullWithRetryCanceledDuringAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan struct{})

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := runPullWithRetry(ctx, newTestPullLogger(), "image", "",
		PullRetryConfig{MaxAttempts: 3},
		func(ctx context.Context, _ int) (PullFailureCategory, error) {
			calls++
			<-ctx.Done()
			close(done)
			return classifyPullFailure(ctx.Err()), ctx.Err()
		})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls, "no new retry must be scheduled after cancellation")
	<-done
}

func TestRunPullWithRetryCanceledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := runPullWithRetry(ctx, newTestPullLogger(), "image", "",
		PullRetryConfig{MaxAttempts: 3, InitialDelay: time.Minute, MaxDelay: time.Minute},
		func(context.Context, int) (PullFailureCategory, error) {
			calls++
			return PullFailureConnection, errors.New("connection reset by peer")
		})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls, "the retry scheduled during backoff must not run")
}

// fakePullResponse is an empty ImagePullResponse for tests.
type fakePullResponse struct{}

func (fakePullResponse) Read([]byte) (int, error) { return 0, io.EOF }
func (fakePullResponse) Close() error             { return nil }
func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}
func (fakePullResponse) Wait(context.Context) error { return nil }

type fakeImagePuller struct {
	mu       sync.Mutex
	authSeen []string
	errs     []error
}

func (f *fakeImagePuller) ImagePull(_ context.Context, _ string, opts client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authSeen = append(f.authSeen, opts.RegistryAuth)
	if len(f.errs) >= len(f.authSeen) {
		if err := f.errs[len(f.authSeen)-1]; err != nil {
			return nil, err
		}
	}
	return fakePullResponse{}, nil
}

func (f *fakeImagePuller) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authSeen...)
}

func TestImagePullOnceCredentialFallbackSucceeds(t *testing.T) {
	puller := &fakeImagePuller{errs: []error{errors.New("unauthorized: authentication required"), nil}}
	opts := client.ImagePullOptions{RegistryAuth: "encoded-creds"}

	err := imagePullOnce(context.Background(), newTestPullLogger(), puller, "image", "", &opts, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"encoded-creds", ""}, puller.calls(), "the fallback pull must run without credentials")
	assert.Empty(t, opts.RegistryAuth, "credentials must remain disabled after the fallback")
}

func TestImagePullOnceCredentialFallbackUnauthorizedFailsImmediately(t *testing.T) {
	finalErr := errors.New("unauthorized: authentication required")
	puller := &fakeImagePuller{errs: []error{errors.New("unauthorized: authentication required"), finalErr}}
	opts := client.ImagePullOptions{RegistryAuth: "encoded-creds"}

	err := imagePullOnce(context.Background(), newTestPullLogger(), puller, "image", "", &opts, true)
	assert.Equal(t, finalErr, err, "error text must be identical to today's behavior")
	assert.Len(t, puller.calls(), 2)
}

func TestImagePullOnceNoFallbackWithoutFlag(t *testing.T) {
	authErr := errors.New("unauthorized: authentication required")
	puller := &fakeImagePuller{errs: []error{authErr}}
	opts := client.ImagePullOptions{RegistryAuth: "encoded-creds"}

	err := imagePullOnce(context.Background(), newTestPullLogger(), puller, "image", "", &opts, false)
	assert.Equal(t, authErr, err)
	assert.Equal(t, []string{"encoded-creds"}, puller.calls())
	assert.Equal(t, "encoded-creds", opts.RegistryAuth)
}

func TestDoImagePullWithRetriesFallbackThenRetriesWithoutCredentials(t *testing.T) {
	puller := &fakeImagePuller{errs: []error{
		errors.New("unauthorized: authentication required"), // attempt 1 with credentials
		errors.New("connection reset by peer"),              // attempt 1 fallback without credentials
		nil,                                                 // attempt 2 without credentials
	}}
	input := NewDockerPullExecutorInput{
		Image:    "image",
		Username: "username",
		Password: "password",
	}

	err := doImagePullWithRetries(context.Background(), newTestPullLogger(), puller, "image", input,
		PullRetryConfig{MaxAttempts: 2})
	require.NoError(t, err)

	calls := puller.calls()
	assert.Len(t, calls, 3)
	assert.NotEmpty(t, calls[0], "first call uses credentials")
	assert.Empty(t, calls[1], "fallback call runs without credentials")
	assert.Empty(t, calls[2], "retries keep using no credentials after stale credentials")
}

func resetPullFlights() {
	pullFlightsMu.Lock()
	pullFlights = map[string]*pullFlight{}
	pullFlightsMu.Unlock()
}

// waitForFlightWaiters blocks until n waiters queued on the flight for key.
func waitForFlightWaiters(t *testing.T, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pullFlightsMu.Lock()
		flight, ok := pullFlights[key]
		waiters := 0
		if ok {
			waiters = flight.waiters
		}
		pullFlightsMu.Unlock()
		if ok && waiters == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d flight waiters", n)
}

func TestJoinPullFlightDeduplicatesConcurrentPulls(t *testing.T) {
	resetPullFlights()
	const waiters = 4

	var runCount atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	run := func(context.Context) error {
		runCount.Add(1)
		close(started)
		<-release
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, waiters+1)
	var waiterRuns atomic.Int32
	wg.Add(waiters + 1)

	go func() {
		defer wg.Done()
		errs[0] = joinPullFlight(context.Background(), "shared-image", "linux/amd64", run)
	}()
	<-started

	for i := 1; i <= waiters; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = joinPullFlight(context.Background(), "shared-image", "linux/amd64", func(context.Context) error {
				waiterRuns.Add(1)
				return nil
			})
		}(i)
	}

	waitForFlightWaiters(t, pullFlightKey("shared-image", "linux/amd64"), waiters)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), runCount.Load(), "only one actual pull must run")
	assert.Equal(t, int32(0), waiterRuns.Load(), "waiters must not run their own pull")
	for i, err := range errs {
		assert.NoError(t, err, "waiter %d", i)
	}
}

func TestJoinPullFlightSharesFailure(t *testing.T) {
	resetPullFlights()
	const callers = 5
	wantErr := errors.New("pull exhausted: rate limit exceeded")

	started := make(chan struct{})
	release := make(chan struct{})
	run := func(context.Context) error {
		close(started)
		<-release
		return wantErr
	}

	var wg sync.WaitGroup
	errs := make([]error, callers)
	wg.Add(callers)
	go func() { defer wg.Done(); errs[0] = joinPullFlight(context.Background(), "img", "", run) }()
	<-started
	for i := 1; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = joinPullFlight(context.Background(), "img", "", func(context.Context) error { return nil })
		}(i)
	}

	waitForFlightWaiters(t, pullFlightKey("img", ""), callers-1)
	close(release)
	wg.Wait()

	for _, err := range errs {
		assert.Same(t, wantErr, err, "every caller must receive the same error")
	}
}

func TestJoinPullFlightRunsAgainAfterCompletion(t *testing.T) {
	resetPullFlights()
	var runCount atomic.Int32
	run := func(context.Context) error {
		runCount.Add(1)
		return nil
	}

	require.NoError(t, joinPullFlight(context.Background(), "img", "", run))
	require.NoError(t, joinPullFlight(context.Background(), "img", "", run))
	assert.Equal(t, int32(2), runCount.Load(), "finished pulls must not be deduplicated")
}

func TestJoinPullFlightCanceledWaiterDoesNotStopPull(t *testing.T) {
	resetPullFlights()

	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- joinPullFlight(context.Background(), "img", "", func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- joinPullFlight(waiterCtx, "img", "", func(context.Context) error { return nil })
	}()
	waitForFlightWaiters(t, pullFlightKey("img", ""), 1)
	cancel()
	assert.ErrorIs(t, <-waiterDone, context.Canceled, "the canceled waiter returns promptly")

	select {
	case <-release:
		t.Fatal("leader flight must still be running")
	default:
	}

	close(release)
	select {
	case err := <-leaderDone:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("leader flight did not finish after release")
	}
}
