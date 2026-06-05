package workflow

import "testing"

func TestConfigSampleParses(t *testing.T) {
	wf, err := Load("../../testdata/workflows/config.yml", true)
	if err != nil {
		t.Fatalf("parse config.yml: %v", err)
	}
	if _, ok := wf.Jobs["show"]; !ok {
		t.Fatalf("job 'show' missing; jobs=%v", wf.Jobs)
	}
}
