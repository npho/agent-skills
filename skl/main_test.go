package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("SKL_ROOT", root)
	initPaths()
	dryRun = false
	t.Cleanup(func() { dryRun = false })
	return root
}
func writeSkill(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
func localState(t *testing.T, root string, pairs ...string) *State {
	t.Helper()
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for i := 0; i < len(pairs); i += 2 {
		ns, name := pairs[i], pairs[i+1]
		path := filepath.Join(root, "lib", ns, name)
		writeSkill(t, path, ns+name)
		files, err := hashFolder(path)
		if err != nil {
			t.Fatal(err)
		}
		id := canonicalID(ns, name)
		st.Skills[id] = SkillState{Name: name, Namespace: ns, Path: canonicalRel(ns, name), Origin: "local", LocalDir: path, Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCanonicalLayoutAndSelectorAmbiguity(t *testing.T) {
	root := testRoot(t)
	st := localState(t, root, "one", "scan", "two", "scan")
	if got := canonicalPath(st.Skills["one/scan"]); got != filepath.Join(root, "lib", "one", "scan") {
		t.Fatalf("path=%s", got)
	}
	_, err := resolveSelectors(st, []string{"scan"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	ids, err := resolveSelectors(st, []string{"two/scan"})
	if err != nil || len(ids) != 1 || ids[0] != "two/scan" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestProfileResolutionCycleMissingAndCollision(t *testing.T) {
	root := testRoot(t)
	st := localState(t, root, "one", "a", "two", "b", "three", "a")
	if err := os.MkdirAll(cfg.profiles, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.profiles, "base.toml"), []byte("skills = [\"one/a\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.profiles, "dev.toml"), []byte("extends = [\"base\"]\nskills = [\"two/b\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ids, err := resolveProfiles(st, []string{"dev"}, nil)
	if err != nil || strings.Join(ids, ",") != "one/a,two/b" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	os.WriteFile(filepath.Join(cfg.profiles, "x.toml"), []byte("extends = [\"y\"]\n"), 0644)
	os.WriteFile(filepath.Join(cfg.profiles, "y.toml"), []byte("extends = [\"x\"]\n"), 0644)
	if _, err := resolveProfiles(st, []string{"x"}, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle err=%v", err)
	}
	if _, err := resolveProfiles(st, []string{"missing"}, nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing err=%v", err)
	}
	if _, err := resolveProfiles(st, nil, []string{"one/a", "three/a"}); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("collision err=%v", err)
	}
}

func TestGlobalRelativeSymlinkEnableDisable(t *testing.T) {
	root := testRoot(t)
	st := localState(t, root, "owner", "thing")
	if err := enableGlobal(st, "owner/thing"); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "skills", "thing"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) || target != filepath.Join("..", "lib", "owner", "thing") {
		t.Fatalf("target=%q", target)
	}
	if err := disableGlobal(st, "owner/thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "skills", "thing")); !os.IsNotExist(err) {
		t.Fatalf("link remains: %v", err)
	}
}

func TestProjectManifestLockAndRealCopy(t *testing.T) {
	root := testRoot(t)
	lib := filepath.Join(root, "source")
	writeSkill(t, lib, "project payload")
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, canonical, "project payload")
	files, _ := hashFolder(canonical)
	st.Skills["local/demo"] = SkillState{Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: lib, Files: files}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := cmdProject([]string{"init", "--project", project, "local/demo"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifest(filepath.Join(project, ".agents", "skills.toml"))
	if err != nil || len(manifest.Skills) != 1 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	lock, err := loadProjectLock(filepath.Join(project, ".agents", "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Skills["demo"].LocalDir != lib || len(lock.Skills["demo"].Files) == 0 {
		t.Fatalf("bad lock: %+v", lock.Skills["demo"])
	}
	info, err := os.Lstat(filepath.Join(project, ".agents", "skills", "demo"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("project copy must be real directory: %v %v", info, err)
	}
}

func TestSyncProtectsDriftAndDryRunDoesNotMutate(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	writeSkill(t, source, "pristine")
	st := localState(t, root, "local", "demo")
	s := st.Skills["local/demo"]
	s.LocalDir = source
	st.Skills["local/demo"] = s
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	dest := canonicalPath(s)
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte("edited"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdSync(nil); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected drift refusal, got %v", err)
	}
	dryRun = true
	if err := cmdSync([]string{"--force"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if string(data) != "edited" {
		t.Fatalf("dry-run mutated payload: %q", data)
	}
}

func TestSafeLegacyMigration(t *testing.T) {
	root := testRoot(t)
	legacyDir := filepath.Join(root, "skills", "old")
	writeSkill(t, legacyDir, "exact")
	files, _ := hashFolder(legacyDir)
	legacy := map[string]any{"version": 1, "skills": map[string]any{"old": map[string]any{"origin": "lock", "sourceUrl": "https://github.com/acme/repo.git", "owner": "acme", "repo": "repo", "skillPath": "skills/old/SKILL.md", "pinnedRef": "abc", "installedAt": "2024-01-01T00:00:00Z", "files": files}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, ".sync-state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(root, "pi")
	if err := os.MkdirAll(pi, 0755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(pi, legacyDir)
	if err := os.Symlink(rel, filepath.Join(pi, "old")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(pi, "real"), "keep")
	if err := cmdMigrate([]string{"--pi-skills", pi}); err != nil {
		t.Fatal(err)
	}
	if !dirExists(filepath.Join(root, "lib", "acme", "old")) || dirExists(legacyDir) {
		t.Fatal("skill was not moved")
	}
	if _, err := os.Lstat(filepath.Join(pi, "old")); !os.IsNotExist(err) {
		t.Fatal("managed pi link remains")
	}
	if !dirExists(filepath.Join(pi, "real")) {
		t.Fatal("real Pi directory removed")
	}
	st, wasLegacy, err := loadState()
	if err != nil || wasLegacy || st.Skills["acme/old"].PinnedRef != "abc" {
		t.Fatalf("state migration failed: legacy=%v err=%v state=%+v", wasLegacy, err, st)
	}
	if err := cmdMigrate([]string{"--pi-skills", pi}); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
}
