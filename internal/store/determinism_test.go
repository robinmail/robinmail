package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSQLIsDeterministic: every node applies the store's batches and must
// get the same result, so no statement may ask SQLite for the time or for
// chance; the store passes such values as parameters (:now, random ids
// made in Go). Nor may anything run ANALYZE or PRAGMA optimize, whose
// statistics could give one node another query plan, nor VACUUM, which may
// renumber rowids. This reads the store's source for them.
func TestSQLIsDeterministic(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(random|randomblob|unixepoch|julianday|datetime|date|time|strftime|current_timestamp|current_date|current_time|total_changes|last_insert_rowid|changes|sqlite_version)\(|\b(current_timestamp|current_date|current_time)\b|'now'|\b(analyze|vacuum)\b|pragma\s+optimize`)
	// SQL lives in backquoted strings.
	sqlString := regexp.MustCompile("`[^`]*`")
	isSQL := regexp.MustCompile(`(?i)\b(select|insert|update|delete|create)\b`)
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sqlString.FindAllString(string(src), -1) {
			if !isSQL.MatchString(s) {
				continue
			}
			n++
			if m := forbidden.FindString(s); m != "" {
				t.Errorf("%s: %q in %s", f, m, s)
			}
		}
	}
	if n < 100 {
		t.Errorf("found only %d SQL strings: is the pattern still right?", n)
	}
}
