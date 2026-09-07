package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real-directory replacement is external data, unlike a symlink node which
// can safely be unlinked. npx recovery must fail closed without enumerating or
// restoring links in that replacement.
func TestNpxFailureDoesNotTouchRealDirectoryGlobalReplacement(t *testing.T) {
	root := testRoot(t)
	canonical := filepath.Join(root, "lib", "acme", "chosen")
	writeSkill(t, canonical, "canonical")
	files, err := hashFolder(canonical)
	if err != nil {
		t.Fatal(err)
	}
	chosen := SkillState{Name: "chosen", Namespace: "acme", Path: canonicalRel("acme", "chosen"), Origin: "npx-lock", SourceURL: "https://github.com/acme/repo.git", Owner: "acme", Repo: "repo", SkillPath: "skills/chosen/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files, Global: true}
	if err := saveState(&State{Version: stateVersion, Skills: map[string]SkillState{"acme/chosen": chosen}}); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(cfg.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.global, 0755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(cfg.global, canonicalPath(chosen))
	if err := os.Symlink(rel, filepath.Join(cfg.global, "chosen")); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	writeSkill(t, filepath.Join(external, "chosen"), "external chosen")
	writeSkill(t, filepath.Join(external, "accidental"), "external accidental")
	parked := filepath.Join(root, "parked-global")
	oldRun := runCommand
	t.Cleanup(func() { runCommand = oldRun })
	runCommand = func(string, ...string) error {
		if err := os.Rename(cfg.global, parked); err != nil {
			return err
		}
		if err := os.Rename(external, cfg.global); err != nil {
			return err
		}
		return errors.New("injected real-root npx failure")
	}
	err = cmdAdd([]string{"--force", "acme/repo"})
	if err == nil || !strings.Contains(err.Error(), "injected real-root") || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("expected joined npx/root-replacement failure, got %v", err)
	}
	for name, want := range map[string]string{"chosen": "external chosen", "accidental": "external accidental"} {
		got, readErr := os.ReadFile(filepath.Join(cfg.global, name, "SKILL.md"))
		if readErr != nil || string(got) != want {
			t.Fatalf("external %s was modified: %q, %v", name, got, readErr)
		}
	}
	afterState, err := os.ReadFile(cfg.state)
	if err != nil || string(afterState) != string(beforeState) {
		t.Fatalf("state changed after real-root replacement: %v", err)
	}
	if err := os.Rename(cfg.global, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, cfg.global); err != nil {
		t.Fatal(err)
	}
	if !linkPointsTo(filepath.Join(cfg.global, "chosen"), canonicalPath(chosen)) {
		t.Fatal("legitimate global link changed")
	}
}
