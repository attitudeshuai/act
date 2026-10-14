package common

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestJobTimeoutContext(t *testing.T) {
	assert.Nil(t, JobTimeoutContext(context.Background()))
	assert.False(t, IsJobTimedOut(context.Background()))

	timeoutSignal, markTimedOut := context.WithCancel(context.Background())
	ctx := WithJobTimeoutContext(context.Background(), timeoutSignal)

	assert.Equal(t, timeoutSignal, JobTimeoutContext(ctx))
	assert.False(t, IsJobTimedOut(ctx))

	markTimedOut()
	assert.True(t, IsJobTimedOut(ctx))
}

func TestJobTimeoutContextCancellationPropagates(t *testing.T) {
	timeoutSignal, markTimedOut := context.WithCancel(context.Background())
	ctx := WithJobTimeoutContext(context.Background(), timeoutSignal)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	done := make(chan struct{})
	go func() {
		<-runCtx.Done()
		close(done)
	}()

	markTimedOut()
	// the marker itself is independent from the run context, it only
	// signals that the timeout fired
	select {
	case <-done:
		t.Fatal("run context must not be cancelled by the timeout marker alone")
	case <-time.After(20 * time.Millisecond):
	}

	assert.True(t, IsJobTimedOut(runCtx))
}
