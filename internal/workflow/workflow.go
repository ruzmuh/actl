// Package workflow is a thin, intentionally boring wrapper around act's
// pkg/model. It exists so the rest of actl depends on our types and helpers,
// not directly on act's parsing surface — making the act version we stand on a
// single, swappable seam.
package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nektos/act/pkg/model"
)

// Discover returns the workflow files under dir/.github/workflows, sorted. It
// matches GitHub's own convention (*.yml and *.yaml in that directory). A
// missing .github/workflows directory is not an error — it yields an empty
// slice, so the caller can give a friendly "no workflow found" message.
func Discover(dir string) ([]string, error) {
	wfDir := filepath.Join(dir, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", wfDir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yml", ".yaml":
			paths = append(paths, filepath.Join(wfDir, e.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// Load reads and parses a single workflow file using act's parser.
//
// strict mirrors act's own flag: when true the file is validated against the
// GitHub Actions schema and rejected on unknown keys.
func Load(path string, strict bool) (*model.Workflow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open workflow: %w", err)
	}
	defer f.Close()

	wf, err := model.ReadWorkflow(f, strict)
	if err != nil {
		return nil, fmt.Errorf("parse workflow %q: %w", path, err)
	}
	wf.File = path
	return wf, nil
}

// StepKind is a human-facing label for how a step will execute. It collapses
// act's finer StepType enum into the v0.1 vocabulary we show in the TUI.
type StepKind string

const (
	KindRun      StepKind = "run"      // a shell script
	KindDocker   StepKind = "docker"   // uses: docker://...
	KindLocal    StepKind = "local"    // uses: ./path (composite or node, in-repo)
	KindRemote   StepKind = "remote"   // uses: owner/repo@ref (composite/node/docker)
	KindReusable StepKind = "reusable" // uses: a reusable workflow (out of v0.1 scope)
	KindInvalid  StepKind = "invalid"  // neither run nor uses, or both
)

// KindOf classifies a step for display.
func KindOf(s *model.Step) StepKind {
	switch s.Type() {
	case model.StepTypeRun:
		return KindRun
	case model.StepTypeUsesDockerURL:
		return KindDocker
	case model.StepTypeUsesActionLocal:
		return KindLocal
	case model.StepTypeUsesActionRemote:
		return KindRemote
	case model.StepTypeReusableWorkflowLocal, model.StepTypeReusableWorkflowRemote:
		return KindReusable
	default:
		return KindInvalid
	}
}
