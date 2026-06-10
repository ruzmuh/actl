// Package debugger: see session.go for the package doc.
package debugger

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nektos/act/pkg/model"
)

// MultipleJobsError is returned by New when the workflow has more than one job
// and Options.JobID did not pick one. It lists the available job ids so a
// frontend can prompt for a choice.
type MultipleJobsError struct{ Jobs []string }

func (e *MultipleJobsError) Error() string {
	return fmt.Sprintf("workflow has %d jobs; select one with -job: %s", len(e.Jobs), strings.Join(e.Jobs, ", "))
}

// MultipleMatrixError is returned by New when the selected job is a matrix job and
// the supplied -matrix selection does not pin it to a single combination (debugging
// one job means debugging one combination). It lists the candidates that remain so a
// frontend can prompt; the same shape as MultipleJobsError.
type MultipleMatrixError struct {
	Job    string
	Combos []string // candidate combinations as "k=v, k2=v2" labels, sorted
}

func (e *MultipleMatrixError) Error() string {
	return fmt.Sprintf("job %q has %d matrix combinations; narrow to one with -matrix KEY=VALUE: %s",
		e.Job, len(e.Combos), strings.Join(e.Combos, " | "))
}

// NeedsInput seeds an upstream job's contribution to the needs context when a
// downstream job is debugged in isolation (the upstream job is not run). Outputs
// holds only the keys the user provided; Result defaults to "success" if empty.
type NeedsInput struct {
	Outputs map[string]string
	Result  string
}

// NeedsSummary describes how one of the selected job's needs was satisfied
// locally, for a transparency line. With Live (the --with-deps mode) the upstream
// job runs for real and the seeded fields are unused; otherwise Result is the
// effective value (Assumed when defaulted) and Outputs holds the seeded keys.
type NeedsSummary struct {
	Job     string
	Live    bool
	Result  string
	Assumed bool
	Outputs map[string]string
}

// selectRun picks the single job to debug. With jobID set it returns that job
// (or an error naming the available ones); without it, it returns the sole job,
// or a MultipleJobsError so a frontend can prompt.
func selectRun(plan *model.Plan, jobID string) (*model.Run, error) {
	var runs []*model.Run
	for _, stage := range plan.Stages {
		runs = append(runs, stage.Runs...)
	}
	if len(runs) == 0 {
		return nil, errors.New("debugger: no jobs to run for this event")
	}
	if jobID != "" {
		for _, r := range runs {
			if r.JobID == jobID {
				return r, nil
			}
		}
		return nil, fmt.Errorf("debugger: job %q not found; available jobs: %s", jobID, strings.Join(jobIDsOf(runs), ", "))
	}
	if len(runs) == 1 {
		return runs[0], nil
	}
	return nil, &MultipleJobsError{Jobs: jobIDsOf(runs)}
}

func jobIDsOf(runs []*model.Run) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.JobID)
	}
	sort.Strings(ids)
	return ids
}

// selectMatrix narrows the job's matrix to the single combination to debug. The job
// expands into one act run per combination at run time; debugging one job means
// debugging one combination, so we must pin Config.Matrix to exactly one. With no
// matrix (one implicit combination) the selection is irrelevant and returned as-is —
// act ignores a stray -matrix on a matrix-less job, matching upstream. Otherwise sel
// must filter the cross product down to exactly one combo, else an error: a
// MultipleMatrixError (under-specified) or a plain error (no combination matches).
func selectMatrix(run *model.Run, sel map[string]map[string]bool) (map[string]map[string]bool, error) {
	combos, err := run.Job().GetMatrixes() // cross product with includes/excludes applied
	if err != nil {
		return nil, fmt.Errorf("debugger: matrix: %w", err)
	}
	if len(combos) <= 1 {
		return sel, nil // no matrix (or a single combination) — nothing to pick
	}
	matched := make([]map[string]interface{}, 0, len(combos))
	for _, c := range combos {
		if matchMatrix(c, sel) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 1:
		return sel, nil
	case 0:
		return nil, fmt.Errorf("debugger: no matrix combination matches -matrix; the job's combinations are: %s",
			strings.Join(comboLabels(combos), " | "))
	default:
		return nil, &MultipleMatrixError{Job: run.JobID, Combos: comboLabels(matched)}
	}
}

// matchMatrix reports whether a combination satisfies the selection. For each key the
// user constrained, the combination's value (stringified the same way act compares in
// selectMatrixes) must be in the allowed set; keys the user didn't mention are free.
func matchMatrix(combo map[string]interface{}, sel map[string]map[string]bool) bool {
	for key, allowed := range sel {
		v, ok := combo[key]
		if !ok {
			return false
		}
		if !allowed[fmt.Sprintf("%v", v)] {
			return false
		}
	}
	return true
}

// comboLabels renders matrix combinations as sorted "k=v, k2=v2" strings for listing
// in a prompt/error.
func comboLabels(combos []map[string]interface{}) []string {
	labels := make([]string, 0, len(combos))
	for _, c := range combos {
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, c[k]))
		}
		labels = append(labels, strings.Join(parts, ", "))
	}
	sort.Strings(labels)
	return labels
}

// liveNeeds lists the selected job's needs for a transparency line in --with-deps
// mode, where the upstream jobs run for real (no seeding).
func liveNeeds(run *model.Run) []NeedsSummary {
	job := run.Job()
	if job == nil {
		return nil
	}
	out := make([]NeedsSummary, 0, len(job.Needs()))
	for _, name := range job.Needs() {
		out = append(out, NeedsSummary{Job: name, Live: true})
	}
	return out
}

// seedNeeds writes the selected job's upstream needs into the workflow model so
// act resolves needs.<job>.* locally (it reads them straight from there). It
// replaces each upstream job's Outputs with only the user-supplied keys — so an
// unseeded output is absent and resolves to empty, exactly like a non-existent
// one in GitHub — and defaults the result to success unless overridden. Returns
// a summary for a transparency line.
func seedNeeds(run *model.Run, seeded map[string]NeedsInput) []NeedsSummary {
	job := run.Job()
	if job == nil {
		return nil
	}
	var out []NeedsSummary
	for _, name := range job.Needs() {
		upstream := run.Workflow.GetJob(name)
		if upstream == nil {
			continue
		}
		in := seeded[name]
		result := in.Result
		assumed := result == ""
		if assumed {
			result = "success"
		}
		outputs := make(map[string]string, len(in.Outputs))
		for k, v := range in.Outputs {
			outputs[k] = v
		}
		upstream.Outputs = outputs
		upstream.Result = result
		out = append(out, NeedsSummary{Job: name, Result: result, Assumed: assumed, Outputs: outputs})
	}
	return out
}
