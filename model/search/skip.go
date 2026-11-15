package search

import "strings"

// skippedDirNames are the folders indexed without their content (ADR 008 §4):
// they hold many generated files that nobody searches for.
var skippedDirNames = map[string]bool{
	"node_modules": true,
	".git":         true,
	".venv":        true,
	"__pycache__":  true,
	"vendor":       true,
	".next":        true,
}

// inSkippedTree returns true if the folder at dirPath is a skipped folder or
// is under one, ie its content must not be indexed.
func inSkippedTree(dirPath string) bool {
	for _, name := range strings.Split(dirPath, "/") {
		if skippedDirNames[name] {
			return true
		}
	}
	return false
}
