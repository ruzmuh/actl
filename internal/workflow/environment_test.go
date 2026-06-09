package workflow

import "testing"

func TestJobEnvironment(t *testing.T) {
	const wf = "../../testdata/workflows/environments.yml"
	tests := []struct {
		job  string
		want string
	}{
		{"deploy", "production"}, // scalar form: environment: production
		{"build", "staging"},     // object form: environment: { name: staging, url: … }
		{"nope", ""},             // missing job → ""
	}
	for _, tt := range tests {
		got, err := JobEnvironment(wf, tt.job)
		if err != nil {
			t.Fatalf("JobEnvironment(%q): %v", tt.job, err)
		}
		if got != tt.want {
			t.Errorf("JobEnvironment(%q) = %q, want %q", tt.job, got, tt.want)
		}
	}
}

func TestJobEnvironmentNone(t *testing.T) {
	// sample.yml's job declares no environment.
	got, err := JobEnvironment("../../testdata/workflows/sample.yml", "build")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (no environment declared)", got)
	}
}
