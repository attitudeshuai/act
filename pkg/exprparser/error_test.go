package exprparser

import (
	"errors"
	"testing"

	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpressionErrorClassification(t *testing.T) {
	env := &EvaluationEnvironment{
		Github: &model.GithubContext{},
	}
	interpreter := NewInterpeter(env, Config{})

	// syntax errors are parse stage failures
	_, err := interpreter.Evaluate("1 + ", DefaultStatusCheckNone)
	require.Error(t, err)
	var parseErr *Error
	require.True(t, errors.As(err, &parseErr))
	assert.Equal(t, FailureStageParse, parseErr.Stage)
	assert.Equal(t, FailureReasonSyntax, parseErr.Reason)
	assert.Contains(t, parseErr.Error(), "Failed to parse")

	// unknown contexts are evaluation stage failures with a distinct reason
	_, err = interpreter.Evaluate("notacontext.value", DefaultStatusCheckNone)
	require.Error(t, err)
	var evalErr *Error
	require.True(t, errors.As(err, &evalErr))
	assert.Equal(t, FailureStageEvaluate, evalErr.Stage)
	assert.Equal(t, FailureReasonUnavailableContext, evalErr.Reason)
	assert.Equal(t, "Unavailable context: notacontext", evalErr.Error())

	// other runtime failures keep their message and are categorized as
	// evaluation failures
	_, err = interpreter.Evaluate("notafunction()", DefaultStatusCheckNone)
	require.Error(t, err)
	require.True(t, errors.As(err, &evalErr))
	assert.Equal(t, FailureStageEvaluate, evalErr.Stage)
	assert.Equal(t, FailureReasonEvaluation, evalErr.Reason)
	assert.Equal(t, "TODO: 'notafunction' not implemented", evalErr.Error())
}

func TestExpressionTypeMismatchCategory(t *testing.T) {
	env := &EvaluationEnvironment{
		Github: &model.GithubContext{},
	}
	_, err := NewInterpeter(env, Config{}).Evaluate("fromJSON(1)", DefaultStatusCheckNone)
	require.Error(t, err)
	var evalErr *Error
	require.True(t, errors.As(err, &evalErr))
	assert.Equal(t, FailureStageEvaluate, evalErr.Stage)
	assert.Equal(t, FailureReasonTypeMismatch, evalErr.Reason)
}
