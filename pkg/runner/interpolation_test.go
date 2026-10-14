package runner

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newInterpolationFailureTestRC(t *testing.T, strict bool) *RunContext {
	t.Helper()
	return &RunContext{
		Config: &Config{
			Workdir:           ".",
			StrictExpressions: strict,
			Secrets:           map[string]string{},
			Vars:              map[string]string{},
		},
		Env: map[string]string{
			"GOOD": "value",
		},
		Run: &model.Run{
			JobID: "build",
			Workflow: &model.Workflow{
				File: "push.yml",
				Name: "test-workflow",
				Jobs: map[string]*model.Job{
					"build": {},
				},
			},
		},
	}
}

func TestInterpolateFailureDefaultModeIsAttributedAndTolerated(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	broken := "${{ 1 + }}"
	out, err := ee.Interpolate(context.Background(), "env.BROKEN", broken)
	assert.NoError(t, err, "default mode must not return an error")
	assert.Equal(t, "", out, "default mode keeps the empty string replacement")

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 1)
	failure := failures[0]
	assert.Equal(t, "push.yml", failure.File)
	assert.Equal(t, "jobs.build.env.BROKEN", failure.FieldPath)
	assert.Equal(t, broken, failure.Expr, "the original expression text must be recorded")
	assert.Equal(t, exprparser.FailureStageParse, failure.Stage)
	assert.Equal(t, exprparser.FailureReasonSyntax, failure.Reason)
	assert.Contains(t, failure.Message, "Failed to parse")
}

func TestInterpolateFailureMissingContextIsDistinguishableFromSyntax(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	_, err := ee.Interpolate(context.Background(), "env.BROKEN", "${{ notacontext.value }}")
	assert.NoError(t, err)

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 1)
	failure := failures[0]
	assert.Equal(t, exprparser.FailureStageEvaluate, failure.Stage)
	assert.Equal(t, exprparser.FailureReasonUnavailableContext, failure.Reason)
	assert.Contains(t, failure.Message, "Unavailable context: notacontext")
}

func TestInterpolateFailuresAreDeduplicatedAndCounted(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	for i := 0; i < 3; i++ {
		_, err := ee.Interpolate(context.Background(), "env.BROKEN", "${{ missing.ctx }}")
		assert.NoError(t, err)
	}
	// a failure at a different field is reported separately
	_, err := ee.Interpolate(context.Background(), "name", "${{ missing.ctx }}")
	assert.NoError(t, err)

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 2)
	assert.Equal(t, 3, failures[0].count)
	assert.Equal(t, 1, failures[1].count)
}

func TestInterpolateLegalEmptyStringResultsAreNotFailures(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	// missing properties on existing contexts evaluate to the empty string
	// and are legal expressions, they must not be reported
	for _, in := range []string{
		"${{ env.UNKNOWN }}",
		"prefix-${{ github.undefined_property }}",
		"${{ hashFiles('**/non-extant-files') }}",
	} {
		out, err := ee.Interpolate(context.Background(), "env.OK", in)
		assert.NoError(t, err, in)
		assert.Contains(t, []string{"", "prefix-"}, out, in)
	}

	assert.Empty(t, rc.Config.interpolationReporter().snapshot())
}

func TestInterpolateTypeMismatchDoesNotPanic(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := expressionEvaluator{
		rc:          rc,
		interpreter: &stubInterpreter{value: 42},
	}

	assert.NotPanics(t, func() {
		out, err := ee.Interpolate(context.Background(), "env.NUMBER", "${{ 42 }}")
		assert.NoError(t, err)
		assert.Equal(t, "", out)
	})

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 1)
	failure := failures[0]
	assert.Equal(t, exprparser.FailureStageEvaluate, failure.Stage)
	assert.Equal(t, exprparser.FailureReasonTypeMismatch, failure.Reason)
	assert.Contains(t, failure.Message, "expected string")
	assert.Contains(t, failure.Message, "int")
	assert.Equal(t, "jobs.build.env.NUMBER", failure.FieldPath)
}

func TestInterpolateNestedFailureIsAttributedToOuterField(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	original := "pre-${{ missingctx.foo }}-${{ env.GOOD }}-post"
	out, err := ee.Interpolate(context.Background(), "env.OUTER", original)
	assert.NoError(t, err)
	assert.Equal(t, "", out)

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 1)
	failure := failures[0]
	assert.Equal(t, "jobs.build.env.OUTER", failure.FieldPath)
	assert.Equal(t, original, failure.Expr)
	assert.Equal(t, exprparser.FailureReasonUnavailableContext, failure.Reason)
}

func TestInterpolateStrictModeTerminatesAndListsAllFailurePoints(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, true)
	ee := rc.NewExpressionEvaluator(context.Background())

	_, err := ee.Interpolate(context.Background(), "env.FIRST", "${{ firstctx.x }}")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strict mode")
	assert.Contains(t, err.Error(), "jobs.build.env.FIRST")
	assert.Contains(t, err.Error(), "1 failure point")

	_, err = ee.Interpolate(context.Background(), "env.SECOND", "${{ secondctx.x }}")
	require.Error(t, err)
	// the error of the second failure lists every failure point discovered
	assert.Contains(t, err.Error(), "2 failure point")
	assert.Contains(t, err.Error(), "jobs.build.env.FIRST")
	assert.Contains(t, err.Error(), "jobs.build.env.SECOND")
	assert.Contains(t, err.Error(), "push.yml")
}

func TestInterpolateConcurrentFailuresAreIndependent(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(iterations * 2)
	values := make([]string, iterations*2)
	errs := make([]error, iterations*2)

	for i := 0; i < iterations*2; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				values[i], errs[i] = ee.Interpolate(context.Background(), "env.GOOD", "${{ env.GOOD }}")
			} else {
				values[i], errs[i] = ee.Interpolate(context.Background(), "env.BROKEN", "${{ missingctx.x }}")
			}
		}()
	}
	wg.Wait()

	for i := 0; i < iterations*2; i++ {
		assert.NoError(t, errs[i])
		if i%2 == 0 {
			assert.Equal(t, "value", values[i])
		} else {
			assert.Equal(t, "", values[i], "a broken field must not influence other fields")
		}
	}

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 1)
	assert.Equal(t, iterations, failures[0].count)
}

func TestInterpolateUnclosedExpressionsAreParseFailures(t *testing.T) {
	rc := newInterpolationFailureTestRC(t, false)
	ee := rc.NewExpressionEvaluator(context.Background())

	for _, in := range []string{
		"${{ foo }} ${{ bar",
		"${{ ' }}",
	} {
		out, err := ee.Interpolate(context.Background(), "env.BROKEN", in)
		assert.NoError(t, err, in)
		assert.Equal(t, "", out, in)
	}

	failures := rc.Config.interpolationReporter().snapshot()
	require.Len(t, failures, 2)
	for _, failure := range failures {
		assert.Equal(t, exprparser.FailureStageParse, failure.Stage, failure.Expr)
		assert.Equal(t, exprparser.FailureReasonSyntax, failure.Reason, failure.Expr)
	}
}

func TestInterpolationReporterFlushLogsOnceWithCountAndResets(t *testing.T) {
	reporter := newInterpolationReporter(false)
	reporter.record(InterpolationFailure{
		File:      "push.yml",
		FieldPath: "jobs.build.env.BROKEN",
		Expr:      "${{ missing.x }}",
		Stage:     exprparser.FailureStageEvaluate,
		Reason:    exprparser.FailureReasonUnavailableContext,
		Message:   "Unavailable context: missing",
	})
	reporter.record(InterpolationFailure{
		File:      "push.yml",
		FieldPath: "jobs.build.env.BROKEN",
		Expr:      "${{ missing.x }}",
		Stage:     exprparser.FailureStageEvaluate,
		Reason:    exprparser.FailureReasonUnavailableContext,
		Message:   "Unavailable context: missing",
	})

	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	logger.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableQuote: true})
	ctx := common.WithLogger(context.Background(), logger)

	reporter.flush(ctx)

	logged := buf.String()
	assert.Contains(t, logged, "Expression interpolation failed")
	assert.Contains(t, logged, "push.yml")
	assert.Contains(t, logged, "jobs.build.env.BROKEN")
	assert.Contains(t, logged, "reason: unavailable-context")
	assert.Contains(t, logged, "(occurred 2 times)")
	assert.Empty(t, reporter.snapshot(), "flush resets the collected failures")
}

func TestInterpolationReporterStrictModeDoesNotFlushLogs(t *testing.T) {
	reporter := newInterpolationReporter(true)
	reporter.record(InterpolationFailure{
		FieldPath: "jobs.build.env.BROKEN",
		Expr:      "${{ missing.x }}",
		Stage:     exprparser.FailureStageEvaluate,
		Reason:    exprparser.FailureReasonUnavailableContext,
		Message:   "Unavailable context: missing",
	})

	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	ctx := common.WithLogger(context.Background(), logger)

	reporter.flush(ctx)
	assert.Empty(t, buf.String(), "strict mode surfaces failures via the run error instead of logs")
}

type stubInterpreter struct {
	value interface{}
	err   error
}

func (s *stubInterpreter) Evaluate(string, exprparser.DefaultStatusCheck) (interface{}, error) {
	return s.value, s.err
}
