//go:build windows

package winsys

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProcessNamesIncludesThisProgram(t *testing.T) {
	names, err := ProcessNames()
	if err != nil {
		t.Fatal(err)
	}
	self := strings.ToLower(filepath.Base(os.Args[0]))
	seen := map[string]int{}
	for _, n := range names {
		seen[strings.ToLower(n)]++
	}
	if seen[self] != 1 {
		t.Fatalf("%s appears %d times among %d names", self, seen[self], len(names))
	}
}

func TestRunningMatchesFullPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := RunningMatches([]string{exe})
	if err != nil || len(got) != 1 || !strings.EqualFold(got[0], filepath.Base(exe)) {
		t.Fatalf("matching %q yielded %v, error %v", exe, got, err)
	}
	got, err = RunningMatches([]string{filepath.Join(t.TempDir(), filepath.Base(exe))})
	if err != nil || len(got) != 0 {
		t.Fatalf("matched an unrelated path: %v, error %v", got, err)
	}
}

func TestRunningMatchesFolderAndDescendants(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(filepath.Dir(exe))
	isSelf := func(name string) bool { return strings.EqualFold(name, filepath.Base(exe)) }
	got, err := RunningMatches([]string{filepath.Join(strings.ToUpper(folder), "*")})
	if err != nil || !slices.ContainsFunc(got, isSelf) {
		t.Fatalf("folder %q did not match its descendant: %v, error %v", folder, got, err)
	}
	got, err = RunningMatches([]string{filepath.Join(folder+"-other", "*")})
	if err != nil || slices.ContainsFunc(got, isSelf) {
		t.Fatalf("matched outside the folder: %v, error %v", got, err)
	}
}
