package main

import "testing"

func TestRuntimeVersionFromImageTag(t *testing.T) {
	tests := []struct {
		name      string
		baked     string
		candidate string
		imageTag  string
		want      string
		applied   bool
	}{
		{name: "stable tag", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "v2.0.0", want: "v2.0.0", applied: true},
		{name: "exact built beta", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "v2.0.0-beta.4", want: "v2.0.0-beta.4", applied: true},
		{name: "invented beta", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "v2.0.0-beta.5", want: "v2.0.0"},
		{name: "candidate wrong base", baked: "v2.0.0", candidate: "v2.0.1-beta.1", imageTag: "v2.0.1-beta.1", want: "v2.0.0"},
		{name: "candidate is stable", baked: "v2.0.0", candidate: "v2.0.0", imageTag: "v2.0.0-beta.4", want: "v2.0.0"},
		{name: "latest alias is not identity", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "latest", want: "v2.0.0"},
		{name: "wrong base", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "v2.0.1-beta.1", want: "v2.0.0"},
		{name: "other prerelease", baked: "v2.0.0", candidate: "v2.0.0-beta.4", imageTag: "v2.0.0-rc.1", want: "v2.0.0"},
		{name: "dev stays dev", baked: "dev", candidate: "v2.0.0-beta.1", imageTag: "v2.0.0-beta.1", want: "dev"},
		{name: "empty", baked: "v2.0.0", candidate: "v2.0.0-beta.4", want: "v2.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, applied := runtimeVersionFromImageTag(tt.baked, tt.candidate, tt.imageTag)
			if got != tt.want || applied != tt.applied {
				t.Fatalf("runtimeVersionFromImageTag(%q, %q, %q) = (%q, %v), want (%q, %v)", tt.baked, tt.candidate, tt.imageTag, got, applied, tt.want, tt.applied)
			}
		})
	}
}
