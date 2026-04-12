package session

import (
	"path/filepath"
	"strings"
)

// MatchGlob tests whether a file path matches a glob pattern as defined by the
// LSP specification. Supported syntax:
//   - *     matches any sequence of characters except path separator
//   - **    matches any sequence of path segments (including zero)
//   - ?     matches any single character except path separator
//   - [abc] matches any character in the set
//   - {a,b} matches any of the comma-separated alternatives
//
// The pattern is matched against the full path using forward slashes as
// separators, consistent with LSP URI semantics.
func MatchGlob(pattern, path string) bool {
	// Normalize to forward slashes for consistent matching.
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)

	// Expand {a,b,c} alternatives and try each.
	for _, pat := range expandBraces(pattern) {
		if doMatch(pat, path) {
			return true
		}
	}
	return false
}

// doMatch performs recursive glob matching. ** matches zero or more path
// segments (including separators), * matches within a single segment.
func doMatch(pattern, name string) bool {
	for len(pattern) > 0 {
		if len(name) == 0 {
			// Name exhausted — pattern must be only * or ** or empty.
			if pattern == "**" || pattern == "**/" {
				return true
			}
			// Check if remaining pattern can match empty string.
			if pattern[0] == '*' && !strings.HasPrefix(pattern, "**") {
				pattern = pattern[1:]
				continue
			}
			return false
		}

		switch pattern[0] {
		case '*':
			if strings.HasPrefix(pattern, "**/") || pattern == "**" {
				// ** matches zero or more path segments.
				rest := pattern
				if strings.HasPrefix(pattern, "**/") {
					rest = pattern[3:]
				} else {
					rest = "" // trailing ** matches everything
				}

				// Try matching rest against name at every segment boundary.
				// Zero segments: try matching rest here.
				if doMatch(rest, name) {
					return true
				}
				// One or more segments: skip to each / and try.
				for i := 0; i < len(name); i++ {
					if name[i] == '/' {
						if doMatch(rest, name[i+1:]) {
							return true
						}
					}
				}
				// Try consuming entire name.
				if rest == "" {
					return true
				}
				return doMatch(rest, "")
			}

			// Single * — matches anything except /.
			// Try consuming 0..n non-/ characters.
			rest := pattern[1:]
			if doMatch(rest, name) {
				return true
			}
			for i := 0; i < len(name) && name[i] != '/'; i++ {
				if doMatch(rest, name[i+1:]) {
					return true
				}
			}
			return false

		case '?':
			if name[0] == '/' {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]

		case '[':
			// Character class — find the closing ].
			end := strings.IndexByte(pattern, ']')
			if end < 0 {
				return false // malformed
			}
			class := pattern[1:end]
			if name[0] == '/' {
				return false
			}
			if !matchCharClass(class, name[0]) {
				return false
			}
			pattern = pattern[end+1:]
			name = name[1:]

		default:
			if pattern[0] != name[0] {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}

	return len(name) == 0
}

// matchCharClass checks if a byte matches a bracket expression like "abc" or "a-z".
func matchCharClass(class string, char byte) bool {
	negate := false
	i := 0
	if i < len(class) && (class[i] == '!' || class[i] == '^') {
		negate = true
		i++
	}

	matched := false
	for i < len(class) {
		if i+2 < len(class) && class[i+1] == '-' {
			// Range.
			if char >= class[i] && char <= class[i+2] {
				matched = true
			}
			i += 3
		} else {
			if char == class[i] {
				matched = true
			}
			i++
		}
	}

	if negate {
		return !matched
	}
	return matched
}

// expandBraces expands the first top-level {a,b,...} group in pattern into
// multiple patterns. Nested braces are not supported.
func expandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}

	// Find the matching closing brace.
	depth := 0
	end := -1
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
				goto found
			}
		}
	}
	// No matching closing brace — treat literally.
	return []string{pattern}

found:
	prefix := pattern[:start]
	suffix := pattern[end+1:]
	alternatives := splitBraceAlternatives(pattern[start+1 : end])

	var results []string
	for _, alt := range alternatives {
		// Recursively expand remaining braces.
		results = append(results, expandBraces(prefix+alt+suffix)...)
	}
	return results
}

// splitBraceAlternatives splits a brace body by commas, respecting nested braces.
func splitBraceAlternatives(body string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])
	return parts
}
