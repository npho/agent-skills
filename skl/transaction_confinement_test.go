package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLibraryTransactionRejectsNamespaceSwapBeforeBackupRename(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, source, "new")
	writeSkill(t, canonical, "old")
	files, err := hashFolder(canonical)
	if err != nil {
		t.Fatal(err)
	}
	state := &State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: source, Files: files}}}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(cfg.state)
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalSkill := filepath.Join(external, "demo")
	writeSkill(t, externalSkill, "external")
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
	err = cmdUpdate([]string{"local/demo"})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected namespace confinement failure, got %v", err)
	}
	if err := os.Remove(filepath.Join(cfg.lib, "local")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, filepath.Join(cfg.lib, "local")); err != nil {
		t.Fatal(err)
	}
	assertRejectedNamespaceSwap(t, canonical, externalSkill, beforeState)
}

func TestLibraryTransactionRejectsRealNamespaceReplacement(t *testing.T) {
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
	externalSkill := filepath.Join(external, "demo")
	writeSkill(t, externalSkill, "external")
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
		return os.Rename(external, filepath.Join(cfg.lib, namespace))
	}
	t.Cleanup(func() { beforeNamespaceRename = oldHook })
	if err := cmdUpdate([]string{"local/demo"}); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("expected real-directory replacement failure, got %v", err)
	}
	if err := os.Rename(filepath.Join(cfg.lib, "local"), external); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, filepath.Join(cfg.lib, "local")); err != nil {
		t.Fatal(err)
	}
	assertRejectedNamespaceSwap(t, canonical, externalSkill, beforeState)
}

func assertRejectedNamespaceSwap(t *testing.T, canonical, externalSkill string, beforeState []byte) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(canonical, "SKILL.md"))
	if err != nil || string(payload) != "old" {
		t.Fatalf("canonical payload changed: %q, %v", payload, err)
	}
	externalPayload, err := os.ReadFile(filepath.Join(externalSkill, "SKILL.md"))
	if err != nil || string(externalPayload) != "external" {
		t.Fatalf("external payload changed: %q, %v", externalPayload, err)
	}
	afterState, err := os.ReadFile(cfg.state)
	if err != nil || string(afterState) != string(beforeState) {
		t.Fatalf("state changed after rejected namespace replacement: %v", err)
	}
}
