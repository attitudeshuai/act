package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/exprparser"
)

// InterpolationFailure describes a single failed expression interpolation.
// It records where the expression was used and why it failed.
type InterpolationFailure struct {
	// File is the workflow file the failed expression belongs to.
	File string
	// FieldPath is the path of the field within the workflow,
	// e.g. jobs.build.services.db.image
	FieldPath string
	// Expr is the original text of the field as it was written in the
	// workflow (including the ${{ }} markers).
	Expr string
	// Stage is the stage the failure occurred at (parse or evaluate).
	Stage exprparser.FailureStage
	// Reason is the category of the failure.
	Reason exprparser.FailureReason
	// Message is the underlying error message.
	Message string

	// count is the number of times the same failure was recorded.
	count int
}

func (f InterpolationFailure) key() string {
	return strings.Join([]string{
		f.File,
		f.FieldPath,
		f.Expr,
		string(f.Stage),
		string(f.Reason),
		f.Message,
	}, "\x00")
}

// interpolationReporter collects interpolation failures for a single run.
// It is safe for concurrent use: fields are evaluated independently, a
// failure of one field only records its own failure and never affects the
// evaluation result of any other field.
type interpolationReporter struct {
	mu       sync.Mutex
	strict   bool
	keys     []string
	failures map[string]*InterpolationFailure
}

func newInterpolationReporter(strict bool) *interpolationReporter {
	return &interpolationReporter{
		strict:   strict,
		failures: map[string]*InterpolationFailure{},
	}
}

// record stores the failure. Failures with the same file, field path,
// expression, stage, reason and message are counted as one.
func (r *interpolationReporter) record(failure InterpolationFailure) InterpolationFailure {
	key := failure.key()

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.failures[key]; ok {
		existing.count++
		return *existing
	}

	failure.count = 1
	r.failures[key] = &failure
	r.keys = append(r.keys, key)
	return failure
}

// snapshot returns a copy of all unique failures recorded so far,
// in the order they were first seen.
func (r *interpolationReporter) snapshot() []InterpolationFailure {
	r.mu.Lock()
	defer r.mu.Unlock()

	failures := make([]InterpolationFailure, 0, len(r.keys))
	for _, key := range r.keys {
		failures = append(failures, *r.failures[key])
	}
	return failures
}

// err returns an error listing every failure point discovered so far.
// It returns nil outside of strict mode or when no failure was recorded.
func (r *interpolationReporter) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.strict || len(r.keys) == 0 {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "expression interpolation failed in strict mode with %d failure point(s):", len(r.keys))
	for _, key := range r.keys {
		f := r.failures[key]
		fmt.Fprintf(&sb, "\n  - file: %s", dashUnknown(f.File))
		fmt.Fprintf(&sb, "\n    field: %s", dashUnknown(f.FieldPath))
		fmt.Fprintf(&sb, "\n    expression: %q", f.Expr)
		fmt.Fprintf(&sb, "\n    stage: %s", f.Stage)
		fmt.Fprintf(&sb, "\n    reason: %s", f.Reason)
		if f.count > 1 {
			fmt.Fprintf(&sb, "\n    occurrences: %d", f.count)
		}
		fmt.Fprintf(&sb, "\n    cause: %s", f.Message)
	}
	return fmt.Errorf("%s", sb.String())
}

// flush reports all collected failures once and resets the reporter.
// In the default (fault tolerant) mode every unique failure is logged
// once with the number of occurrences. In strict mode the failures are
// surfaced via the returned run error, so nothing is logged here.
func (r *interpolationReporter) flush(ctx context.Context) {
	r.mu.Lock()
	failures := make([]InterpolationFailure, 0, len(r.keys))
	for _, key := range r.keys {
		failures = append(failures, *r.failures[key])
	}
	r.keys = nil
	r.failures = map[string]*InterpolationFailure{}
	strict := r.strict
	r.mu.Unlock()

	if strict {
		return
	}

	logger := common.Logger(ctx)
	for _, f := range failures {
		message := fmt.Sprintf(
			"Expression interpolation failed (file: %s, field: %s, expression: %q, stage: %s, reason: %s): %s",
			dashUnknown(f.File),
			dashUnknown(f.FieldPath),
			f.Expr,
			f.Stage,
			f.Reason,
			f.Message,
		)
		if f.count > 1 {
			message += fmt.Sprintf(" (occurred %d times)", f.count)
		}
		logger.Errorf("%s", message)
	}
}

func dashUnknown(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// interpolationReporter returns the reporter shared by every evaluator of
// the run. The reporter is lazily created so that configs constructed
// without going through New keep working (e.g. in tests). Lazy creation is
// atomic because evaluators of parallel jobs can run concurrently.
func (c *Config) interpolationReporter() *interpolationReporter {
	pointer := (*unsafe.Pointer)(unsafe.Pointer(&c.interpolationFailures))
	if reporter := (*interpolationReporter)(atomic.LoadPointer(pointer)); reporter != nil {
		return reporter
	}

	reporter := newInterpolationReporter(c.StrictExpressions)
	if atomic.CompareAndSwapPointer(pointer, nil, unsafe.Pointer(reporter)) {
		return reporter
	}
	return (*interpolationReporter)(atomic.LoadPointer(pointer))
}

// interpolationReporter returns the run wide reporter of the run context.
func (rc *RunContext) interpolationReporter() *interpolationReporter {
	return rc.Config.interpolationReporter()
}
