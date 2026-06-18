// Package debugger: see session.go for the package doc.
package debugger

import (
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
)

// When marks which side of a step's main executor a pause occurred on. It mirrors
// the fork's runner.BarrierWhen but keeps act's type out of frontend code.
type When int

const (
	Before When = iota // before the step's main executor ran
	After              // after the step's main executor returned
)

func (w When) String() string {
	if w == After {
		return "after"
	}
	return "before"
}

// PauseEvent is emitted when the run halts at a step boundary.
type PauseEvent struct {
	When  When        // before or after the step's main executor
	Index int         // zero-based step index within the job
	Step  *model.Step // the step at this boundary
	Err   error       // for When==After: the step's error, or nil
}

// ProgressEvent is emitted as the run passes a step's "before" boundary without
// halting (Continue mode, no breakpoint), so a frontend can follow execution — the
// step-list highlight tracks the running step even when no pause fires. It's purely
// advisory: the send is non-blocking and may be dropped under load, and the
// authoritative state is still PauseEvent/Done. Emitted only for the debugged job.
type ProgressEvent struct {
	Index int         // zero-based step index now starting
	Step  *model.Step // the step at this boundary
}

func toWhen(w runner.BarrierWhen) When {
	if w == runner.BarrierAfter {
		return After
	}
	return Before
}
