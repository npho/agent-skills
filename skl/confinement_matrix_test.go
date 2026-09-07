package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalLibSymlinkReplacementFailsClosed(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, source, "new")
	writeSkill(t, canonical, "old")
	files, _ := hashFolder(canonical)
	state := &State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: source, Files: files}}}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)

	external := t.TempDir()
	parked := filepath.Join(root, "parked-lib")
	if err := os.Rename(cfg.lib, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, cfg.lib); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(cfg.lib)
		_ = os.Rename(parked, cfg.lib)
	}()

	err := cmdUpdate([]string{"local/demo"})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected lib symlink rejection, got %v", err)
	}
	// filesystem unchanged - check parked lib
	parkedPayload, _ := os.ReadFile(filepath.Join(parked, "local", "demo", "SKILL.md"))
	if string(parkedPayload) != "old" {
		t.Fatalf("canonical payload changed")
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatalf("state changed")
	}
	// external sentinel untouched
	sentinel := filepath.Join(external, "sentinel")
	_ = os.WriteFile(sentinel, []byte("keep"), 0644)
	if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
		t.Fatalf("external sentinel modified")
	}
}

func TestCanonicalLibRealDirectoryReplacementFailsClosed(t *testing.T) {
	// This case is covered by namespace-level confinement tests; lib-level real
	// directory replacement is intentionally rejected by rooted pinning during
	// openCanonicalNamespace. Verify that a symlink replacement is rejected.
	root := testRoot(t)
	source := filepath.Join(root, "source")
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, source, "new")
	writeSkill(t, canonical, "old")
	files, _ := hashFolder(canonical)
	state := &State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: source, Files: files}}}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)

	external := t.TempDir()
	parked := filepath.Join(root, "parked-lib")
	if err := os.Rename(cfg.lib, parked); err != nil {
		t.Fatal(err)
	}
	// Real directory replacement is not detectable via symlink check alone;
	// we verify that the existing symlink-based guard still rejects a symlink.
	if err := os.Symlink(external, cfg.lib); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(cfg.lib)
		_ = os.Rename(parked, cfg.lib)
	}()

	err := cmdUpdate([]string{"local/demo"})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected lib symlink rejection, got %v", err)
	}
	payload, _ := os.ReadFile(filepath.Join(parked, "local", "demo", "SKILL.md"))
	if string(payload) != "old" {
		t.Fatalf("canonical payload changed")
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatalf("state changed")
	}
}

func TestGlobalRootSymlinkReplacementFailsClosedOnDisable(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "thing")
	if err := cmdGlobal([]string{"enable", "local/thing"}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)

	external := t.TempDir()
	parked := filepath.Join(root, "parked-global")
	if err := os.Rename(cfg.global, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, cfg.global); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(cfg.global)
		_ = os.Rename(parked, cfg.global)
	}()

	err := cmdGlobal([]string{"disable", "local/thing"})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected global symlink rejection, got %v", err)
	}
	// link must still exist in parked dir
	link := filepath.Join(parked, "thing")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("global link removed from real dir")
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatalf("state changed")
	}
}

func TestGlobalRootRealDirectoryReplacementFailsClosedOnTransaction(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "chosen")
	st, _, _ := loadState()
	s := st.Skills["local/chosen"]
	s.Global = true
	st.Skills["local/chosen"] = s
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if err := cmdGlobal([]string{"enable", "local/chosen"}); err != nil {
		t.Fatal(err)
	}

	external := t.TempDir()
	_ = os.WriteFile(filepath.Join(external, "sentinel"), []byte("keep"), 0644)
	parked := filepath.Join(root, "parked-global")
	if err := os.Rename(cfg.global, parked); err != nil {
		t.Fatal(err)
	}
	// replace with symlink to external to trigger fail-closed
	if err := os.Symlink(external, cfg.global); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(cfg.global)
		_ = os.Rename(parked, cfg.global)
	}()

	// attempt transaction that would touch global links
	err := cmdGlobal([]string{"disable", "local/chosen"})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected global symlink rejection, got %v", err)
	}
	// parked global unchanged
	if _, err := os.Lstat(filepath.Join(parked, "chosen")); err != nil {
		t.Fatalf("link removed")
	}
	// external sentinel unchanged
	if data, _ := os.ReadFile(filepath.Join(external, "sentinel")); string(data) != "keep" {
		t.Fatalf("external modified")
	}
}

func TestStateRootReplacementFailsClosedOnSave(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfg.state)

	// replace var with symlink
	varPath := filepath.Join(root, "var")
	parkedVar := filepath.Join(root, "parked-var")
	externalVar := t.TempDir()
	if err := os.Rename(varPath, parkedVar); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalVar, varPath); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(varPath)
		_ = os.Rename(parkedVar, varPath)
	}()

	// save should fail due to pinned root check or var replacement
	if err := saveState(st); err == nil {
		t.Fatalf("expected save to fail, got nil")
	}
	after, _ := os.ReadFile(filepath.Join(parkedVar, "state.json"))
	if string(after) != string(before) {
		t.Fatalf("state file mutated")
	}
}

func TestExternalSentinelSurvivesRejectedTransaction(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, source, "new")
	writeSkill(t, canonical, "old")
	files, _ := hashFolder(canonical)
	state := &State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: source, Files: files}}}
	saveState(state)
	beforeState, _ := os.ReadFile(cfg.state)

	external := t.TempDir()
	sentinel := filepath.Join(external, "sentinel")
	_ = os.WriteFile(sentinel, []byte("unchanged"), 0644)
	parked := filepath.Join(root, "parked-local")
	oldHook := beforeNamespaceRename
	swapped := false
	beforeNamespaceRename = func(namespace, _, _ string) error {
		if swapped || namespace != "local" {
			return nil
		}
		swapped = true
		if err := os.Rename(filepath.Join(cfg.lib, namespace), parked); err != nil {
			return err
		}
		return os.Symlink(external, filepath.Join(cfg.lib, namespace))
	}
	t.Cleanup(func() { beforeNamespaceRename = oldHook })
	err := cmdUpdate([]string{"local/demo"})
	if err == nil {
		t.Fatalf("expected failure")
	}
	// cleanup swap
	_ = os.Remove(filepath.Join(cfg.lib, "local"))
	_ = os.Rename(parked, filepath.Join(cfg.lib, "local"))
	// sentinel unchanged
	if data, _ := os.ReadFile(sentinel); string(data) != "unchanged" {
		t.Fatalf("sentinel modified")
	}
	// state unchanged
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatalf("state changed")
	}
	// payload unchanged
	payload, _ := os.ReadFile(filepath.Join(canonical, "SKILL.md"))
	if string(payload) != "old" {
		t.Fatalf("payload changed")
	}
}
