// Package debugger: see session.go for the package doc.
package debugger

import (
	"github.com/ruzmuh/actl/internal/workflow"
)

// Listing is a Docker-free inventory of a workflow's jobs and steps, produced by List
// for `actl -list` so the user can see what they'd debug (jobs, their environment and
// matrix combinations, and each job's steps) without starting a container.
type Listing struct {
	WorkflowPath string
	Event        string
	Jobs         []JobListing
}

// JobListing is one job in a Listing.
type JobListing struct {
	ID          string
	Name        string
	Environment string   // deployment environment (`environment:`), "" if none
	Matrix      []string // combination labels ("k=v, …") when the job has more than one; nil otherwise
	Steps       []StepListing
}

// StepListing is one step in a JobListing.
type StepListing struct {
	Index int
	Label string // step display label (name / uses / run)
	Kind  string // run / docker / local / remote / reusable
}

// List parses and plans the workflow and returns its jobs and steps without touching
// Docker — the read-only counterpart to New, sharing the same parsePlan prefix. It does
// not select a single job (it lists them all), so a multi-job or matrix workflow is
// reported in full rather than prompting.
func List(opts Options) (*Listing, error) {
	plan, err := parsePlan(&opts)
	if err != nil {
		return nil, err
	}
	listing := &Listing{WorkflowPath: opts.WorkflowPath, Event: opts.EventName}
	for _, stage := range plan.Stages {
		for _, run := range stage.Runs {
			job := run.Job()
			jl := JobListing{ID: run.JobID, Name: job.Name}
			jl.Environment, _ = workflow.JobEnvironment(opts.WorkflowPath, run.JobID)
			if combos, err := job.GetMatrixes(); err == nil && len(combos) > 1 {
				jl.Matrix = comboLabels(combos)
			}
			for i, st := range job.Steps {
				jl.Steps = append(jl.Steps, StepListing{
					Index: i,
					Label: st.String(),
					Kind:  string(workflow.KindOf(st)),
				})
			}
			listing.Jobs = append(listing.Jobs, jl)
		}
	}
	return listing, nil
}
