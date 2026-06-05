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

func TestGCPAuthSampleParses(t *testing.T) {
	wf, err := Load("../../testdata/workflows/gcp-auth.yml", true)
	if err != nil {
		t.Fatalf("parse gcp-auth.yml: %v", err)
	}
	job, ok := wf.Jobs["deploy"]
	if !ok {
		t.Fatalf("job 'deploy' missing; jobs=%v", wf.Jobs)
	}
	var hasAuth bool
	for _, st := range job.Steps {
		if st.Uses == "google-github-actions/auth@v2" {
			hasAuth = true
		}
	}
	if !hasAuth {
		t.Error("sample should contain a google-github-actions/auth step")
	}
}
