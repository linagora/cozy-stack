package search

import (
	"path"
	"strings"
)

// skippedDirNames are the folders indexed without their content (ADR 008 §4):
// they hold many generated files that nobody searches for.
var skippedDirNames = map[string]bool{
	"node_modules": true,
	".git":         true,
	".venv":        true,
	"__pycache__":  true,
	".next":        true,
}

// contentSkipped returns true if the content of the folder at dirPath is not
// indexed: the folder is a skipped folder or is under one.
func contentSkipped(dirPath string) bool {
	for _, name := range strings.Split(dirPath, "/") {
		if skippedDirNames[name] {
			return true
		}
	}
	return false
}

// folderSkipped returns true if the folder at dirPath is not indexed: it is
// under a skipped folder. A skipped folder itself is indexed.
func folderSkipped(dirPath string) bool {
	return contentSkipped(path.Dir(dirPath))
}
