package session

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// ** patterns (common LSP file watcher patterns)
		{"**/*.gradle.kts", "build.gradle.kts", true},
		{"**/*.gradle.kts", "app/build.gradle.kts", true},
		{"**/*.gradle.kts", "a/b/c/build.gradle.kts", true},
		{"**/*.gradle.kts", "build.gradle", false},
		{"**/*.gradle.kts", "foo.kt", false},

		{"**/build.gradle", "build.gradle", true},
		{"**/build.gradle", "app/build.gradle", true},
		{"**/build.gradle", "a/b/build.gradle", true},
		{"**/build.gradle", "build.gradle.kts", false},

		// {a,b} alternatives
		{"*.{gradle,kts}", "build.gradle", true},
		{"*.{gradle,kts}", "app.kts", true},
		{"*.{gradle,kts}", "app.kt", false},
		{"**/*.{gradle,gradle.kts}", "build.gradle", true},
		{"**/*.{gradle,gradle.kts}", "app/build.gradle.kts", true},

		// Simple * wildcard
		{"*.go", "main.go", true},
		{"*.go", "test.go", true},
		{"*.go", "main.kt", false},
		{"*.go", "dir/main.go", false},

		// ? wildcard
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},

		// [abc] character class
		{"[abc].go", "a.go", true},
		{"[abc].go", "d.go", false},

		// Mixed ** with directory structure
		{"**/src/**/*.kt", "src/main.kt", true},
		{"**/src/**/*.kt", "project/src/main/App.kt", true},
		{"**/src/**/*.kt", "project/lib/main.kt", false},

		// Trailing **
		{"src/**", "src/main.go", true},
		{"src/**", "src/a/b/c.go", true},

		// Exact match (no wildcards)
		{"build.gradle", "build.gradle", true},
		{"build.gradle", "other.gradle", false},

		// Leading path with **
		{"gradle/**", "gradle/wrapper/gradle-wrapper.properties", true},
		{"gradle/**", "other/file", false},
	}

	for _, tt := range tests {
		got := MatchGlob(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestExpandBraces(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"*.{go,kt}", []string{"*.go", "*.kt"}},
		{"no-braces", []string{"no-braces"}},
		{"{a,b,c}", []string{"a", "b", "c"}},
		{"pre-{a,b}-suf", []string{"pre-a-suf", "pre-b-suf"}},
	}

	for _, tt := range tests {
		got := expandBraces(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("expandBraces(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("expandBraces(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
