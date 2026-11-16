// Package lint holds repository-wide style checks that run with go test.
package lint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoEmDashes keeps the em-dash character (U+2014) out of source,
// UI strings and docs. Use a comma, colon, parentheses or a hyphen instead.
func TestNoEmDashes(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	skipDirs := map[string]bool{".git": true, "node_modules": true, "dist": true, "testdata": true}
	exts := map[string]bool{".go": true, ".ts": true, ".tsx": true, ".css": true, ".html": true,
		".md": true, ".yml": true, ".yaml": true, ".json": true, ".sh": true}
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[filepath.Ext(path)] || d.Name() == "package-lock.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.ContainsRune(line, '\u2014') {
				rel, _ := filepath.Rel(root, path)
				bad = append(bad, rel+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("em-dash found in %d places (first: %s)", len(bad), strings.Join(bad[:min(10, len(bad))], ", "))
	}
}
