package common

import (
	"context"
)

type jobErrorContextKey string

const jobErrorContextKeyVal = jobErrorContextKey("job.error")

type jobCancelCtx string

const JobCancelCtxVal = jobCancelCtx("job.cancel")

type jobTimeoutCtx string

const jobTimeoutCtxVal = jobTimeoutCtx("job.timeout")

// JobError returns the job error for current context if any
func JobError(ctx context.Context) error {
	val := ctx.Value(jobErrorContextKeyVal)
	if val != nil {
		if container, ok := val.(map[string]error); ok {
			return container["error"]
		}
	}
	return nil
}

func SetJobError(ctx context.Context, err error) {
	ctx.Value(jobErrorContextKeyVal).(map[string]error)["error"] = err
}

// WithJobErrorContainer adds a value to the context as a container for an error
func WithJobErrorContainer(ctx context.Context) context.Context {
	container := map[string]error{}
	return context.WithValue(ctx, jobErrorContextKeyVal, container)
}

func WithJobCancelContext(ctx context.Context, cancelContext context.Context) context.Context {
	return context.WithValue(ctx, JobCancelCtxVal, cancelContext)
}

func JobCancelContext(ctx context.Context) context.Context {
	val := ctx.Value(JobCancelCtxVal)
	if val != nil {
		if container, ok := val.(context.Context); ok {
			return container
		}
	}
	return nil
}

// EarlyCancelContext returns a new context based on ctx that is canceled when the first of the provided contexts is canceled.
func EarlyCancelContext(ctx context.Context) (context.Context, context.CancelFunc) {
	val := JobCancelContext(ctx)
	if val != nil {
		context, cancel := context.WithCancel(ctx)
		go func() {
			defer cancel()
			select {
			case <-context.Done():
			case <-ctx.Done():
			case <-val.Done():
			}
		}()
		return context, cancel
	}
	return ctx, func() {}
}

// WithJobTimeoutContext stores a context that is canceled exactly when the
// job-level timeout-minutes deadline is reached. It lets running steps
// distinguish a job timeout from other cancellations.
func WithJobTimeoutContext(ctx context.Context, timeoutContext context.Context) context.Context {
	return context.WithValue(ctx, jobTimeoutCtxVal, timeoutContext)
}

// JobTimeoutContext returns the job timeout context stored in ctx, if any.
func JobTimeoutContext(ctx context.Context) context.Context {
	val := ctx.Value(jobTimeoutCtxVal)
	if val != nil {
		if timeoutContext, ok := val.(context.Context); ok {
			return timeoutContext
		}
	}
	return nil
}

// IsJobTimedOut reports whether the job-level timeout-minutes deadline fired.
func IsJobTimedOut(ctx context.Context) bool {
	timeoutContext := JobTimeoutContext(ctx)
	return timeoutContext != nil && timeoutContext.Err() != nil
}
