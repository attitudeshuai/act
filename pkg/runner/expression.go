package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strings"
	"time"

	_ "embed"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// ExpressionEvaluator is the interface for evaluating expressions
type ExpressionEvaluator interface {
	evaluate(context.Context, string, exprparser.DefaultStatusCheck) (interface{}, error)
	EvaluateYamlNode(context.Context, *yaml.Node) error
	// Interpolate evaluates all expressions in in and returns the result.
	// fieldPath identifies the workflow field the expression belongs to
	// (relative to the job, e.g. "services.db.image"); it is used to
	// attribute interpolation failures.
	// In the default fault tolerant mode a failed interpolation returns an
	// empty string and no error, the failure is still recorded and logged.
	// In strict mode it returns an error listing all known failure points.
	Interpolate(ctx context.Context, fieldPath string, in string) (string, error)
}

// NewExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewExpressionEvaluator(ctx context.Context) ExpressionEvaluator {
	return rc.NewExpressionEvaluatorWithEnv(ctx, rc.GetEnv())
}

func (rc *RunContext) NewExpressionEvaluatorWithEnv(ctx context.Context, env map[string]string) ExpressionEvaluator {
	var workflowCallResult map[string]*model.WorkflowCallResult

	// todo: cleanup EvaluationEnvironment creation
	using := make(map[string]exprparser.Needs)
	strategy := make(map[string]interface{})
	if rc.Run != nil {
		job := rc.Run.Job()
		if job != nil && job.Strategy != nil {
			strategy["fail-fast"] = job.Strategy.FailFast
			strategy["max-parallel"] = job.Strategy.MaxParallel
		}

		jobs := rc.Run.Workflow.Jobs
		jobNeeds := rc.Run.Job().Needs()

		for _, needs := range jobNeeds {
			using[needs] = exprparser.Needs{
				Outputs: jobs[needs].Outputs,
				Result:  jobs[needs].Result,
			}
		}

		// only setup jobs context in case of workflow_call
		// and existing expression evaluator (this means, jobs are at
		// least ready to run)
		if rc.caller != nil && rc.ExprEval != nil {
			workflowCallResult = map[string]*model.WorkflowCallResult{}

			for jobName, job := range jobs {
				result := model.WorkflowCallResult{
					Outputs: map[string]string{},
				}
				for k, v := range job.Outputs {
					result.Outputs[k] = v
				}
				workflowCallResult[jobName] = &result
			}
		}
	}

	ghc := rc.getGithubContext(ctx)
	inputs := getEvaluatorInputs(ctx, rc, nil, ghc)

	ee := &exprparser.EvaluationEnvironment{
		Github: ghc,
		Env:    env,
		Job:    rc.getJobContext(),
		Jobs:   &workflowCallResult,
		// todo: should be unavailable
		// but required to interpolate/evaluate the step outputs on the job
		Steps:     rc.getStepsContext(),
		Secrets:   getWorkflowSecrets(ctx, rc),
		Vars:      getWorkflowVars(ctx, rc),
		Strategy:  strategy,
		Matrix:    rc.Matrix,
		Needs:     using,
		Inputs:    inputs,
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	if rc.JobContainer != nil {
		ee.Runner = rc.JobContainer.GetRunnerContext(ctx)
	}
	return expressionEvaluator{
		rc: rc,
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "job",
		}),
	}
}

//go:embed hashfiles/index.js
var hashfiles string

// NewStepExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewStepExpressionEvaluator(ctx context.Context, step step) ExpressionEvaluator {
	return rc.NewStepExpressionEvaluatorExt(ctx, step, false)
}

// NewStepExpressionEvaluatorExt creates a new evaluator
func (rc *RunContext) NewStepExpressionEvaluatorExt(ctx context.Context, step step, rcInputs bool) ExpressionEvaluator {
	ghc := rc.getGithubContext(ctx)
	if rcInputs {
		return rc.newStepExpressionEvaluator(ctx, step, ghc, getEvaluatorInputs(ctx, rc, nil, ghc))
	}
	return rc.newStepExpressionEvaluator(ctx, step, ghc, getEvaluatorInputs(ctx, rc, step, ghc))
}

func (rc *RunContext) newStepExpressionEvaluator(ctx context.Context, step step, _ *model.GithubContext, inputs map[string]interface{}) ExpressionEvaluator {
	// todo: cleanup EvaluationEnvironment creation
	job := rc.Run.Job()
	strategy := make(map[string]interface{})
	if job.Strategy != nil {
		strategy["fail-fast"] = job.Strategy.FailFast
		strategy["max-parallel"] = job.Strategy.MaxParallel
	}

	jobs := rc.Run.Workflow.Jobs
	jobNeeds := rc.Run.Job().Needs()

	using := make(map[string]exprparser.Needs)
	for _, needs := range jobNeeds {
		using[needs] = exprparser.Needs{
			Outputs: jobs[needs].Outputs,
			Result:  jobs[needs].Result,
		}
	}

	ee := &exprparser.EvaluationEnvironment{
		Github:   step.getGithubContext(ctx),
		Env:      *step.getEnv(),
		Job:      rc.getJobContext(),
		Steps:    rc.getStepsContext(),
		Secrets:  getWorkflowSecrets(ctx, rc),
		Vars:     getWorkflowVars(ctx, rc),
		Strategy: strategy,
		Matrix:   rc.Matrix,
		Needs:    using,
		// todo: should be unavailable
		// but required to interpolate/evaluate the inputs in actions/composite
		Inputs:    inputs,
		HashFiles: getHashFilesFunction(ctx, rc),
	}
	if rc.JobContainer != nil {
		ee.Runner = rc.JobContainer.GetRunnerContext(ctx)
	}
	return expressionEvaluator{
		rc: rc,
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    "step",
		}),
	}
}

func getHashFilesFunction(ctx context.Context, rc *RunContext) func(v []reflect.Value) (interface{}, error) {
	hashFiles := func(v []reflect.Value) (interface{}, error) {
		if rc.JobContainer != nil {
			timeed, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			name := "workflow/hashfiles/index.js"
			hout := &bytes.Buffer{}
			herr := &bytes.Buffer{}
			patterns := []string{}
			followSymlink := false

			for i, p := range v {
				s := p.String()
				if i == 0 {
					if strings.HasPrefix(s, "--") {
						if strings.EqualFold(s, "--follow-symbolic-links") {
							followSymlink = true
							continue
						}
						return "", fmt.Errorf("Invalid glob option %s, available option: '--follow-symbolic-links'", s)
					}
				}
				patterns = append(patterns, s)
			}
			env := map[string]string{}
			for k, v := range rc.Env {
				env[k] = v
			}
			env["patterns"] = strings.Join(patterns, "\n")
			if followSymlink {
				env["followSymbolicLinks"] = "true"
			}

			stdout, stderr := rc.JobContainer.ReplaceLogWriter(hout, herr)
			_ = rc.JobContainer.Copy(rc.JobContainer.GetActPath(), &container.FileEntry{
				Name: name,
				Mode: 0o644,
				Body: hashfiles,
			}).
				Then(rc.execJobContainer([]string{rc.GetNodeToolFullPath(ctx), path.Join(rc.JobContainer.GetActPath(), name)},
					env, "", "")).
				Finally(func(context.Context) error {
					rc.JobContainer.ReplaceLogWriter(stdout, stderr)
					return nil
				})(timeed)
			output := hout.String() + "\n" + herr.String()
			guard := "__OUTPUT__"
			outstart := strings.Index(output, guard)
			if outstart != -1 {
				outstart += len(guard)
				outend := strings.Index(output[outstart:], guard)
				if outend != -1 {
					return output[outstart : outstart+outend], nil
				}
			}
		}
		return "", nil
	}
	return hashFiles
}

type expressionEvaluator struct {
	rc          *RunContext
	interpreter exprparser.Interpreter
}

func (ee expressionEvaluator) evaluate(ctx context.Context, in string, defaultStatusCheck exprparser.DefaultStatusCheck) (interface{}, error) {
	logger := common.Logger(ctx)
	logger.Debugf("evaluating expression '%s'", in)
	evaluated, err := ee.interpreter.Evaluate(in, defaultStatusCheck)

	printable := regexp.MustCompile(`::add-mask::.*`).ReplaceAllString(fmt.Sprintf("%t", evaluated), "::add-mask::***)")
	logger.Debugf("expression '%s' evaluated to '%s'", in, printable)

	return evaluated, err
}

func (ee expressionEvaluator) evaluateScalarYamlNode(ctx context.Context, node *yaml.Node) (*yaml.Node, error) {
	var in string
	if err := node.Decode(&in); err != nil {
		return nil, err
	}
	if !strings.Contains(in, "${{") || !strings.Contains(in, "}}") {
		return nil, nil
	}
	expr, err := rewriteSubExpression(ctx, in, false)
	if err != nil {
		return nil, err
	}
	res, err := ee.evaluate(ctx, expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return nil, err
	}
	ret := &yaml.Node{}
	if err := ret.Encode(res); err != nil {
		return nil, err
	}
	return ret, err
}

func (ee expressionEvaluator) evaluateMappingYamlNode(ctx context.Context, node *yaml.Node) (*yaml.Node, error) {
	var ret *yaml.Node
	// GitHub has this undocumented feature to merge maps, called insert directive
	insertDirective := regexp.MustCompile(`\${{\s*insert\s*}}`)
	for i := 0; i < len(node.Content)/2; i++ {
		changed := func() error {
			if ret == nil {
				ret = &yaml.Node{}
				if err := ret.Encode(node); err != nil {
					return err
				}
				ret.Content = ret.Content[:i*2]
			}
			return nil
		}
		k := node.Content[i*2]
		v := node.Content[i*2+1]
		ev, err := ee.evaluateYamlNodeInternal(ctx, v)
		if err != nil {
			return nil, err
		}
		if ev != nil {
			if err := changed(); err != nil {
				return nil, err
			}
		} else {
			ev = v
		}
		var sk string
		// Merge the nested map of the insert directive
		if k.Decode(&sk) == nil && insertDirective.MatchString(sk) {
			if ev.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("failed to insert node %v into mapping %v unexpected type %v expected MappingNode", ev, node, ev.Kind)
			}
			if err := changed(); err != nil {
				return nil, err
			}
			ret.Content = append(ret.Content, ev.Content...)
		} else {
			ek, err := ee.evaluateYamlNodeInternal(ctx, k)
			if err != nil {
				return nil, err
			}
			if ek != nil {
				if err := changed(); err != nil {
					return nil, err
				}
			} else {
				ek = k
			}
			if ret != nil {
				ret.Content = append(ret.Content, ek, ev)
			}
		}
	}
	return ret, nil
}

func (ee expressionEvaluator) evaluateSequenceYamlNode(ctx context.Context, node *yaml.Node) (*yaml.Node, error) {
	var ret *yaml.Node
	for i := 0; i < len(node.Content); i++ {
		v := node.Content[i]
		// Preserve nested sequences
		wasseq := v.Kind == yaml.SequenceNode
		ev, err := ee.evaluateYamlNodeInternal(ctx, v)
		if err != nil {
			return nil, err
		}
		if ev != nil {
			if ret == nil {
				ret = &yaml.Node{}
				if err := ret.Encode(node); err != nil {
					return nil, err
				}
				ret.Content = ret.Content[:i]
			}
			// GitHub has this undocumented feature to merge sequences / arrays
			// We have a nested sequence via evaluation, merge the arrays
			if ev.Kind == yaml.SequenceNode && !wasseq {
				ret.Content = append(ret.Content, ev.Content...)
			} else {
				ret.Content = append(ret.Content, ev)
			}
		} else if ret != nil {
			ret.Content = append(ret.Content, v)
		}
	}
	return ret, nil
}

func (ee expressionEvaluator) evaluateYamlNodeInternal(ctx context.Context, node *yaml.Node) (*yaml.Node, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return ee.evaluateScalarYamlNode(ctx, node)
	case yaml.MappingNode:
		return ee.evaluateMappingYamlNode(ctx, node)
	case yaml.SequenceNode:
		return ee.evaluateSequenceYamlNode(ctx, node)
	default:
		return nil, nil
	}
}

func (ee expressionEvaluator) EvaluateYamlNode(ctx context.Context, node *yaml.Node) error {
	ret, err := ee.evaluateYamlNodeInternal(ctx, node)
	if err != nil {
		return err
	}
	if ret != nil {
		return ret.Decode(node)
	}
	return nil
}

func (ee expressionEvaluator) Interpolate(ctx context.Context, fieldPath string, in string) (string, error) {
	if !strings.Contains(in, "${{") || !strings.Contains(in, "}}") {
		return in, nil
	}

	expr, err := rewriteSubExpression(ctx, in, true)
	if err != nil {
		return ee.handleInterpolationError(ctx, fieldPath, in, err)
	}

	evaluated, err := ee.evaluate(ctx, expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		// failures of nested sub expressions are attributed to the field that
		// contains them, while still keeping the original expression text
		return ee.handleInterpolationError(ctx, fieldPath, in, err)
	}

	value, ok := evaluated.(string)
	if !ok {
		err := &exprparser.Error{
			Stage:  exprparser.FailureStageEvaluate,
			Reason: exprparser.FailureReasonTypeMismatch,
			Message: fmt.Sprintf(
				"Expression %s did not evaluate to a string (expected string, got %s)",
				expr, actualTypeName(evaluated),
			),
		}
		return ee.handleInterpolationError(ctx, fieldPath, in, err)
	}

	return value, nil
}

// handleInterpolationError records the failure with its field location and
// either reports it as an aggregated error (strict mode) or returns an empty
// string without an error (default fault tolerant mode).
func (ee expressionEvaluator) handleInterpolationError(ctx context.Context, fieldPath string, in string, cause error) (string, error) {
	failure := ee.toInterpolationFailure(fieldPath, in, cause)
	reporter := ee.interpolationReporter()
	reporter.record(failure)

	common.Logger(ctx).
		WithField("file", failure.File).
		WithField("field", failure.FieldPath).
		WithField("stage", string(failure.Stage)).
		WithField("reason", string(failure.Reason)).
		Debugf("Expression interpolation failed for %q: %s", failure.Expr, failure.Message)

	if reporter.strict {
		return "", reporter.err()
	}

	return "", nil
}

func (ee expressionEvaluator) toInterpolationFailure(fieldPath string, in string, cause error) InterpolationFailure {
	failure := InterpolationFailure{
		FieldPath: ee.qualifiedFieldPath(fieldPath),
		Expr:      in,
	}

	if ee.rc != nil && ee.rc.Run != nil && ee.rc.Run.Workflow != nil {
		failure.File = ee.rc.Run.Workflow.File
	}

	var exprErr *exprparser.Error
	if errors.As(cause, &exprErr) {
		failure.Stage = exprErr.Stage
		failure.Reason = exprErr.Reason
		failure.Message = exprErr.Message
	} else {
		failure.Stage = exprparser.FailureStageEvaluate
		failure.Reason = exprparser.FailureReasonEvaluation
		failure.Message = cause.Error()
	}

	return failure
}

func (ee expressionEvaluator) qualifiedFieldPath(fieldPath string) string {
	if ee.rc != nil && ee.rc.Run != nil && ee.rc.Run.JobID != "" {
		if fieldPath != "" {
			return fmt.Sprintf("jobs.%s.%s", ee.rc.Run.JobID, fieldPath)
		}
		return fmt.Sprintf("jobs.%s", ee.rc.Run.JobID)
	}
	return fieldPath
}

func actualTypeName(value interface{}) string {
	if value == nil {
		return "null"
	}
	return reflect.TypeOf(value).String()
}

func (ee expressionEvaluator) interpolationReporter() *interpolationReporter {
	if ee.rc == nil || ee.rc.Config == nil {
		// evaluators are always created through a RunContext in production,
		// this is only a safeguard for unusual constructions
		return newInterpolationReporter(false)
	}
	return ee.rc.interpolationReporter()
}

// EvalBool evaluates an expression against given evaluator
func EvalBool(ctx context.Context, evaluator ExpressionEvaluator, expr string, defaultStatusCheck exprparser.DefaultStatusCheck) (bool, error) {
	nextExpr, err := rewriteSubExpression(ctx, expr, false)
	if err != nil {
		return false, err
	}

	evaluated, err := evaluator.evaluate(ctx, nextExpr, defaultStatusCheck)
	if err != nil {
		return false, err
	}

	return exprparser.IsTruthy(evaluated), nil
}

func escapeFormatString(in string) string {
	return strings.ReplaceAll(strings.ReplaceAll(in, "{", "{{"), "}", "}}")
}

func rewriteSubExpression(ctx context.Context, in string, forceFormat bool) (string, error) {
	if !strings.Contains(in, "${{") || !strings.Contains(in, "}}") {
		return in, nil
	}

	strPattern := regexp.MustCompile("(?:''|[^'])*'")
	pos := 0
	exprStart := -1
	strStart := -1
	var results []string
	formatOut := ""
	for pos < len(in) {
		if strStart > -1 {
			matches := strPattern.FindStringIndex(in[pos:])
			if matches == nil {
				return "", &exprparser.Error{
					Stage:   exprparser.FailureStageParse,
					Reason:  exprparser.FailureReasonSyntax,
					Message: "unclosed string.",
				}
			}

			strStart = -1
			pos += matches[1]
		} else if exprStart > -1 {
			exprEnd := strings.Index(in[pos:], "}}")
			strStart = strings.Index(in[pos:], "'")

			if exprEnd > -1 && strStart > -1 {
				if exprEnd < strStart {
					strStart = -1
				} else {
					exprEnd = -1
				}
			}

			if exprEnd > -1 {
				formatOut += fmt.Sprintf("{%d}", len(results))
				results = append(results, strings.TrimSpace(in[exprStart:pos+exprEnd]))
				pos += exprEnd + 2
				exprStart = -1
			} else if strStart > -1 {
				pos += strStart + 1
			} else {
				return "", &exprparser.Error{
					Stage:   exprparser.FailureStageParse,
					Reason:  exprparser.FailureReasonSyntax,
					Message: "unclosed expression.",
				}
			}
		} else {
			exprStart = strings.Index(in[pos:], "${{")
			if exprStart != -1 {
				formatOut += escapeFormatString(in[pos : pos+exprStart])
				exprStart = pos + exprStart + 3
				pos = exprStart
			} else {
				formatOut += escapeFormatString(in[pos:])
				pos = len(in)
			}
		}
	}

	if len(results) == 1 && formatOut == "{0}" && !forceFormat {
		return in, nil
	}

	out := fmt.Sprintf("format('%s', %s)", strings.ReplaceAll(formatOut, "'", "''"), strings.Join(results, ", "))
	if in != out {
		common.Logger(ctx).Debugf("expression '%s' rewritten to '%s'", in, out)
	}
	return out, nil
}

func getEvaluatorInputs(ctx context.Context, rc *RunContext, step step, ghc *model.GithubContext) map[string]interface{} {
	inputs := map[string]interface{}{}

	setupWorkflowInputs(ctx, &inputs, rc)

	var env map[string]string
	if step != nil {
		env = *step.getEnv()
	} else {
		env = rc.GetEnv()
	}

	for k, v := range env {
		if strings.HasPrefix(k, "INPUT_") {
			inputs[strings.ToLower(strings.TrimPrefix(k, "INPUT_"))] = v
		}
	}

	if rc.caller == nil && ghc.EventName == "workflow_dispatch" {
		config := rc.Run.Workflow.WorkflowDispatchConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					value = v.Default
				}
				if v.Type == "boolean" {
					inputs[k] = value == "true"
				} else {
					inputs[k] = value
				}
			}
		}
	}

	if ghc.EventName == "workflow_call" {
		config := rc.Run.Workflow.WorkflowCallConfig()
		if config != nil && config.Inputs != nil {
			for k, v := range config.Inputs {
				value := nestedMapLookup(ghc.Event, "inputs", k)
				if value == nil {
					if err := v.Default.Decode(&value); err != nil {
						common.Logger(ctx).Debugf("error decoding default value for %s: %v", k, err)
					}
				}
				if v.Type == "boolean" {
					inputs[k] = value == "true"
				} else {
					inputs[k] = value
				}
			}
		}
	}
	return inputs
}

func setupWorkflowInputs(ctx context.Context, inputs *map[string]interface{}, rc *RunContext) {
	if rc.caller != nil {
		config := rc.Run.Workflow.WorkflowCallConfig()

		for name, input := range config.Inputs {
			value := rc.caller.runContext.Run.Job().With[name]

			if value != nil {
				node := yaml.Node{}
				_ = node.Encode(value)
				if rc.caller.runContext.ExprEval != nil {
					// evaluate using the calling RunContext (outside)
					_ = rc.caller.runContext.ExprEval.EvaluateYamlNode(ctx, &node)
				}
				_ = node.Decode(&value)
			}

			if value == nil && config != nil && config.Inputs != nil {
				def := input.Default
				if rc.ExprEval != nil {
					// evaluate using the called RunContext (inside)
					_ = rc.ExprEval.EvaluateYamlNode(ctx, &def)
				}
				_ = def.Decode(&value)
			}

			(*inputs)[name] = value
		}
	}
}

func getWorkflowSecrets(ctx context.Context, rc *RunContext) map[string]string {
	if rc.caller != nil {
		job := rc.caller.runContext.Run.Job()
		secrets := job.Secrets()

		if secrets == nil && job.InheritSecrets() {
			secrets = rc.caller.runContext.Config.Secrets
		}

		if secrets == nil {
			secrets = map[string]string{}
		}

		for k, v := range secrets {
			// failures are recorded on the calling workflow run and handled
			// at its execution boundaries; secret evaluation cannot return an
			// error here as it happens while building the evaluator
			interpolated, _ := rc.caller.runContext.ExprEval.Interpolate(ctx, fmt.Sprintf("secrets.%s", k), v)
			secrets[k] = interpolated
		}

		return secrets
	}

	return rc.Config.Secrets
}

func getWorkflowVars(_ context.Context, rc *RunContext) map[string]string {
	return rc.Config.Vars
}
