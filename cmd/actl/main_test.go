package main

import "testing"

func TestParseNeeds(t *testing.T) {
	in := []string{
		"build.outputs.image=repo/app:abc",
		"build.outputs.sha=abc123",
		"build.result=failure",
		"test.outputs.coverage=92",
	}
	got, err := parseNeeds(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["build"].Result != "failure" {
		t.Errorf("build.result = %q, want failure", got["build"].Result)
	}
	if got["build"].Outputs["image"] != "repo/app:abc" || got["build"].Outputs["sha"] != "abc123" {
		t.Errorf("build.outputs = %v", got["build"].Outputs)
	}
	if got["test"].Outputs["coverage"] != "92" {
		t.Errorf("test.outputs.coverage = %q, want 92", got["test"].Outputs["coverage"])
	}
}

func TestParseNeedsErrors(t *testing.T) {
	bad := []string{
		"noequalsign",
		"build.bogus=1",    // neither result nor outputs
		"build.outputs=1",  // outputs without a name
		"build.result.x=1", // result takes no sub-path
	}
	for _, e := range bad {
		if _, err := parseNeeds([]string{e}); err == nil {
			t.Errorf("parseNeeds(%q): want error, got nil", e)
		}
	}
}
