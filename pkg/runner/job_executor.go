package runner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	log "github.com/sirupsen/logrus"
)

// jobResultTimedOut is the distinct conclusion of a job that was terminated
// because it exceeded its declared timeout-minutes. It is intentionally
// different from "failure" and from a manual "cancelled" termination and is
// visible to downstream jobs via the needs context.
const jobResultTimedOut = "timed_out"

// jobTimeoutDuration converts the declared timeout-minutes into the actual
// deadline; it exists as a package level function so tests can shrink the
// deadline.
var jobTimeoutDuration = func(timeoutMinutes int64) time.Duration {
	return time.Duration(timeoutMinutes) * time.Minute
}

type jobInfo interface {
	matrix() map[string]interface{}
	steps() []*model.Step
	startContainer() common.Executor
	stopContainer() common.Executor
	closeContainer() common.Executor
	interpolateOutputs() common.Executor
	result(result string)
}

//nolint:contextcheck
func newJobExecutor(info jobInfo, sf stepFactory, rc *RunContext) common.Executor {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	steps = append(steps, func(ctx context.Context) error {
		logger := common.Logger(ctx)
		if len(info.matrix()) > 0 {
			logger.Infof("\U0001F9EA  Matrix: %v", info.matrix())
		}
		return nil
	})

	infoSteps := info.steps()

	if len(infoSteps) == 0 {
		return common.NewDebugExecutor("No steps found")
	}

	preSteps = append(preSteps, func(ctx context.Context) error {
		// Have to be skipped for some Tests
		if rc.Run == nil {
			return nil
		}
		rc.ExprEval = rc.NewExpressionEvaluator(ctx)
		// evaluate environment variables since they can contain
		// GitHub's special environment variables.
		for k, v := range rc.GetEnv() {
			rc.Env[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		return nil
	})

	var setJobError = func(ctx context.Context, err error) error {
		if err == nil {
			return nil
		}
		logger := common.Logger(ctx)
		logger.Errorf("%v", err)
		common.SetJobError(ctx, err)
		return err
	}

	for i, stepModel := range infoSteps {
		if stepModel == nil {
			return func(_ context.Context) error {
				return fmt.Errorf("invalid Step %v: missing run or uses key", i)
			}
		}
		if stepModel.ID == "" {
			stepModel.ID = fmt.Sprintf("%d", i)
		}

		step, err := sf.newStep(stepModel, rc)

		if err != nil {
			return common.NewErrorExecutor(err)
		}

		preSteps = append(preSteps, useStepLogger(rc, stepModel, stepStagePre, step.pre().ThenError(setJobError)))

		stepExec := step.main()
		steps = append(steps, useStepLogger(rc, stepModel, stepStageMain, func(ctx context.Context) error {
			err := stepExec(ctx)
			if err != nil {
				_ = setJobError(ctx, err)
			} else if ctx.Err() != nil {
				_ = setJobError(ctx, ctx.Err())
			}
			return nil
		}))

		postExec := useStepLogger(rc, stepModel, stepStagePost, step.post().ThenError(setJobError))
		if postExecutor != nil {
			// run the post executor in reverse order
			postExecutor = postExec.Finally(postExecutor)
		} else {
			postExecutor = postExec
		}
	}

	var stopContainerExecutor common.Executor = func(ctx context.Context) error {
		jobError := common.JobError(ctx)
		var err error
		if rc.Config.AutoRemove || jobError == nil {
			// always allow 1 min for stopping and removing the runner, even if we were cancelled
			ctx, cancel := context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), time.Minute)
			defer cancel()

			logger := common.Logger(ctx)
			logger.Infof("Cleaning up container for job %s", rc.JobName)
			if err = info.stopContainer()(ctx); err != nil {
				logger.Errorf("Error while stop job container: %v", err)
			}
		}
		return err
	}

	var setJobResultExecutor common.Executor = func(ctx context.Context) error {
		jobTimedOut := common.IsJobTimedOut(ctx)
		jobError := common.JobError(ctx)
		if jobTimedOut {
			markSkippedStepsForJobTimeout(ctx, rc, info)
		}
		setJobResult(ctx, info, rc, jobError == nil, jobTimedOut)
		setJobOutputs(ctx, rc)
		return nil
	}

	pipeline := make([]common.Executor, 0)
	pipeline = append(pipeline, preSteps...)
	pipeline = append(pipeline, steps...)

	jobPipeline := common.NewPipelineExecutor(
		common.NewFieldExecutor("step", "Set up job", common.NewFieldExecutor("stepid", []string{"--setup-job"},
			common.NewPipelineExecutor(common.NewInfoExecutor("\u2B50 Run Set up job"), info.startContainer(), rc.InitializeNodeTool()).
				Then(common.NewFieldExecutor("stepResult", model.StepStatusSuccess, common.NewInfoExecutor("  \u2705  Success - Set up job"))).
				ThenError(setJobError).OnError(common.NewFieldExecutor("stepResult", model.StepStatusFailure, common.NewInfoExecutor("  \u274C  Failure - Set up job"))))),
		common.NewPipelineExecutor(pipeline...).
			Finally(func(ctx context.Context) error { //nolint:contextcheck
				var cancel context.CancelFunc
				if ctx.Err() == context.Canceled {
					// in case of an aborted run, we still should execute the
					// post steps to allow cleanup.
					ctx, cancel = context.WithTimeout(common.WithLogger(context.Background(), common.Logger(ctx)), 5*time.Minute)
					defer cancel()
				}
				return postExecutor(ctx)
			}).
			Finally(common.NewFieldExecutor("step", "Complete job", common.NewFieldExecutor("stepid", []string{"--complete-job"},
				common.NewInfoExecutor("\u2B50 Run Complete job").
					Finally(stopContainerExecutor).
					Finally(
						info.interpolateOutputs().Finally(info.closeContainer()).Then(common.NewFieldExecutor("stepResult", model.StepStatusSuccess, common.NewInfoExecutor("  \u2705  Success - Complete job"))).
							OnError(common.NewFieldExecutor("stepResult", model.StepStatusFailure, common.NewInfoExecutor("  \u274C  Failure - Complete job"))),
					))))).Finally(setJobResultExecutor)

	return func(ctx context.Context) error {
		// evaluate the job-level timeout-minutes before the job (and its
		// containers) starts; an invalid declaration aborts the job up front
		timeoutMinutes, hasTimeout, err := evaluateJobTimeoutMinutes(ctx, rc)
		if err != nil {
			common.Logger(ctx).Errorf("%v", err)
			return err
		}
		if !hasTimeout {
			return jobPipeline(ctx)
		}
		return executeJobWithTimeout(ctx, rc, jobTimeoutDuration(timeoutMinutes), jobPipeline)
	}
}

// evaluateJobTimeoutMinutes evaluates the job-level `timeout-minutes`
// declaration. A missing declaration or an expression that evaluates to an
// empty string means "no limit" (same behaviour as having no declaration).
// A declaration that fails to evaluate, or that yields anything other than a
// positive integer, is an error naming the job and the raw declaration.
func evaluateJobTimeoutMinutes(ctx context.Context, rc *RunContext) (int64, bool, error) {
	raw := strings.TrimSpace(rc.Run.Job().TimeoutMinutes)
	if raw == "" {
		return 0, false, nil
	}

	value := raw
	if strings.Contains(raw, "${{") && strings.Contains(raw, "}}") {
		expr, _ := rewriteSubExpression(ctx, raw, true)
		evaluated, err := rc.ExprEval.evaluate(ctx, expr, exprparser.DefaultStatusCheckNone)
		if err != nil {
			return 0, false, fmt.Errorf("unable to evaluate 'timeout-minutes: %s' for job '%s': %w", raw, rc.JobName, err)
		}
		value = fmt.Sprintf("%v", evaluated)
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}

	timeoutMinutes, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid 'timeout-minutes: %s' for job '%s': value must be a positive integer", raw, rc.JobName)
	}
	if timeoutMinutes <= 0 {
		return 0, false, fmt.Errorf("invalid 'timeout-minutes: %s' for job '%s': value must be greater than zero", raw, rc.JobName)
	}
	return timeoutMinutes, true, nil
}

// executeJobWithTimeout runs the job pipeline under a job-scoped deadline.
// When the deadline fires it records the timeout, logs it and cancels the job
// context (which stops the running step and prevents new steps from
// starting); the job itself concludes with timed_out and the plan-level
// handleFailure turns that conclusion into a failed run. An external
// cancellation that arrives first wins: only one of the two signals
// terminates the job and the timeout is not reported in that case.
//
// Each matrix instance gets its own invocation, so the deadline is scoped to a
// single instance and cancelling it never cancels sibling instances.
func executeJobWithTimeout(ctx context.Context, rc *RunContext, timeout time.Duration, jobPipeline common.Executor) error {
	logger := common.Logger(ctx)
	timeoutSignal, markTimedOut := context.WithCancel(context.Background())
	ctx = common.WithJobTimeoutContext(ctx, timeoutSignal)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			step := rc.runningStepName()
			if step == "" {
				// the deadline fired before the first step started, e.g. while
				// pulling images or starting containers
				step = "Set up job"
			}
			logger.WithFields(log.Fields{
				"jobResult":      jobResultTimedOut,
				"timeoutMinutes": int64(timeout / time.Minute),
				"step":           step,
			}).Errorf("\u23F1 Job '%s' timed out after %d minute(s) while running step '%s'", rc.JobName, int64(timeout/time.Minute), step)
			// set the state and signal the timeout first, then cancel the job
			// context, so everything observing the cancellation also sees the
			// timeout marker
			rc.TimedOut = true
			markTimedOut()
			cancelRun()
		case <-runCtx.Done():
		}
	}()

	err := jobPipeline(runCtx)
	if timeoutSignal.Err() != nil {
		// a timeout is a terminal job conclusion, just like a step failure it
		// must not short-circuit the plan: sibling jobs and downstream stages
		// keep running and observe the conclusion via needs; the plan-level
		// handleFailure turns the timed_out conclusion into a failed run
		return nil
	}
	return err
}

// markSkippedStepsForJobTimeout marks every main step that never got a result
// because the job timed out as skipped. The step that was running is already
// marked cancelled by runStepExecutor.
func markSkippedStepsForJobTimeout(ctx context.Context, rc *RunContext, info jobInfo) {
	logger := common.Logger(ctx)
	for _, stepModel := range info.steps() {
		if stepModel == nil {
			continue
		}
		if _, ok := rc.StepResults[stepModel.ID]; ok {
			continue
		}
		rc.StepResults[stepModel.ID] = &model.StepResult{
			Outcome:    model.StepStatusSkipped,
			Conclusion: model.StepStatusSkipped,
			Outputs:    make(map[string]string),
		}
		logger.WithField("stepResult", model.StepStatusSkipped).Infof("  \u23ED  Skipped - Main %s (job timed out before the step started)", stepModel.String())
	}
}

func setJobResult(ctx context.Context, info jobInfo, rc *RunContext, success bool, timedOut bool) {
	logger := common.Logger(ctx)

	jobResult := "success"
	switch {
	case timedOut:
		jobResult = jobResultTimedOut
	case !success:
		jobResult = "failure"
	}

	// we have only one result for a whole matrix build, so keep an already
	// reached terminal conclusion; a later failure/timeout only worsens a
	// still successful aggregate result
	if len(info.matrix()) > 0 && rc.Run.Job().Result != "" {
		existing := rc.Run.Job().Result
		if isTerminalJobResult(existing) || jobResult == "success" {
			jobResult = existing
		}
	}

	info.result(jobResult)
	if rc.caller != nil {
		// set reusable workflow job result
		rc.caller.runContext.result(jobResult)
	}

	jobResultMessage := "succeeded"
	switch jobResult {
	case "failure":
		jobResultMessage = "failed"
	case jobResultTimedOut:
		jobResultMessage = "timed out"
	}

	logger.WithField("jobResult", jobResult).Infof("\U0001F3C1  Job %s", jobResultMessage)
}

func isTerminalJobResult(result string) bool {
	return result == "failure" || result == jobResultTimedOut
}

func setJobOutputs(ctx context.Context, rc *RunContext) {
	if rc.caller != nil {
		// map outputs for reusable workflows
		callerOutputs := make(map[string]string)

		ee := rc.NewExpressionEvaluator(ctx)

		for k, v := range rc.Run.Workflow.WorkflowCallConfig().Outputs {
			callerOutputs[k] = ee.Interpolate(ctx, ee.Interpolate(ctx, v.Value))
		}

		rc.caller.runContext.Run.Job().Outputs = callerOutputs
	}
}

func useStepLogger(rc *RunContext, stepModel *model.Step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = withStepLogger(ctx, stepModel.ID, rc.ExprEval.Interpolate(ctx, stepModel.String()), stage.String())

		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		oldout, olderr := rc.JobContainer.ReplaceLogWriter(logWriter, logWriter)
		defer rc.JobContainer.ReplaceLogWriter(oldout, olderr)

		return executor(ctx)
	}
}
