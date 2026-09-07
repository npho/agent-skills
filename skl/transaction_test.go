package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLibraryUpdateStagesBeforeReplacingAnything(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"a", "b"} {
		src := filepath.Join(root, "src", name)
		lib := filepath.Join(root, "lib", "local", name)
		writeSkill(t, src, "old-"+name)
		writeSkill(t, lib, "old-"+name)
		files, _ := hashFolder(lib)
		st.Skills["local/"+name] = SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: src, Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "src", "a", "SKILL.md"), []byte("new-a"), 0644)
	os.Remove(filepath.Join(root, "src", "b", "SKILL.md"))
	before, _ := os.ReadFile(cfg.state)
	if err := cmdUpdate(nil); err == nil {
		t.Fatal("expected later source failure")
	}
	got, _ := os.ReadFile(filepath.Join(root, "lib", "local", "a", "SKILL.md"))
	if string(got) != "old-a" {
		t.Fatalf("first payload changed after later failure: %q", got)
	}
	after, _ := os.ReadFile(cfg.state)
	if string(before) != string(after) {
		t.Fatal("state changed after failed update")
	}
}

func TestLibrarySyncStagesEveryMissingSkillBeforeCommit(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"a", "b"} {
		src := filepath.Join(root, "src", name)
		writeSkill(t, src, "payload-"+name)
		files, err := hashFolder(src)
		if err != nil {
			t.Fatal(err)
		}
		st.Skills["local/"+name] = SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: src, Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src", "b", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	if err := cmdSync(nil); err == nil || !strings.Contains(err.Error(), "has no SKILL.md") {
		t.Fatalf("expected later source failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(root, "lib", "local", name)); !os.IsNotExist(err) {
			t.Fatalf("skill %s was partially restored: %v", name, err)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after failed sync staging")
	}
}

func TestLibrarySyncRollsBackPayloadsWhenCommitFails(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"a", "b"} {
		src := filepath.Join(root, "src", name)
		writeSkill(t, src, "payload-"+name)
		files, _ := hashFolder(src)
		st.Skills["local/"+name] = SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: src, Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	oldRename := renamePath
	renamePath = func(from, to string) error {
		if to == cfg.state && strings.Contains(filepath.Base(from), ".state-") {
			return errors.New("injected sync commit failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renamePath = oldRename })
	if err := cmdSync(nil); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected sync commit failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(root, "lib", "local", name)); !os.IsNotExist(err) {
			t.Fatalf("skill %s survived sync rollback: %v", name, err)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after failed sync commit")
	}
}

func TestCanonicalConfinementRejectsSymlinkedLibAndNamespaces(t *testing.T) {
	t.Run("local add rejects symlinked lib root", func(t *testing.T) {
		root := testRoot(t)
		source := filepath.Join(root, "source")
		writeSkill(t, source, "new")
		external := t.TempDir()
		if err := os.Symlink(external, cfg.lib); err != nil {
			t.Fatal(err)
		}
		if err := cmdAdd([]string{source}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("expected confined add failure, got %v", err)
		}
		entries, _ := os.ReadDir(external)
		if len(entries) != 0 || fileExists(cfg.state) {
			t.Fatalf("local add escaped canonical root: entries=%v state=%v", entries, fileExists(cfg.state))
		}
	})

	t.Run("npx add rejects symlinked owner namespace", func(t *testing.T) {
		testRoot(t)
		external := t.TempDir()
		if err := os.MkdirAll(cfg.lib, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(cfg.lib, "acme")); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{}}); err != nil {
			t.Fatal(err)
		}
		oldRun := runCommand
		t.Cleanup(func() { runCommand = oldRun })
		runCommand = func(string, ...string) error {
			if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{"a": {SourceURL: "https://github.com/acme/repo.git", SkillPath: "skills/a/SKILL.md"}}}); err != nil {
				return err
			}
			writeSkill(t, filepath.Join(cfg.global, "a"), "temporary")
			return nil
		}
		if err := cmdAdd([]string{"acme/repo"}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("expected confined npx add failure, got %v", err)
		}
		entries, _ := os.ReadDir(external)
		if len(entries) != 0 || fileExists(cfg.state) {
			t.Fatalf("npx add escaped owner namespace: entries=%v state=%v", entries, fileExists(cfg.state))
		}
		globalEntries, _ := os.ReadDir(cfg.global)
		if len(globalEntries) != 0 {
			t.Fatalf("npx output remained exposed: %v", globalEntries)
		}
	})

	t.Run("update rejects symlinked owner namespace", func(t *testing.T) {
		root := testRoot(t)
		external := t.TempDir()
		externalSkill := filepath.Join(external, "a")
		writeSkill(t, externalSkill, "external-old")
		files, _ := hashFolder(externalSkill)
		if err := os.MkdirAll(cfg.lib, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(cfg.lib, "acme")); err != nil {
			t.Fatal(err)
		}
		st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/a": {Name: "a", Namespace: "acme", Path: canonicalRel("acme", "a"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/a/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files}}}
		if err := saveState(st); err != nil {
			t.Fatal(err)
		}
		beforeState, _ := os.ReadFile(cfg.state)
		if err := cmdUpdate([]string{"acme/a"}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("expected confined update failure, got %v", err)
		}
		payload, _ := os.ReadFile(filepath.Join(externalSkill, "SKILL.md"))
		afterState, _ := os.ReadFile(cfg.state)
		if string(payload) != "external-old" || string(afterState) != string(beforeState) {
			t.Fatal("update changed external payload or state")
		}
		_ = root
	})

	t.Run("sync rejects symlinked owner namespace", func(t *testing.T) {
		root := testRoot(t)
		source := filepath.Join(root, "source")
		writeSkill(t, source, "locked")
		files, _ := hashFolder(source)
		external := t.TempDir()
		if err := os.MkdirAll(cfg.lib, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(cfg.lib, "local")); err != nil {
			t.Fatal(err)
		}
		st := &State{Version: stateVersion, Skills: map[string]SkillState{"local/a": {Name: "a", Namespace: "local", Path: canonicalRel("local", "a"), Origin: "local", LocalDir: source, Files: files}}}
		if err := saveState(st); err != nil {
			t.Fatal(err)
		}
		beforeState, _ := os.ReadFile(cfg.state)
		if err := cmdSync(nil); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("expected confined sync failure, got %v", err)
		}
		entries, _ := os.ReadDir(external)
		afterState, _ := os.ReadFile(cfg.state)
		if len(entries) != 0 || string(afterState) != string(beforeState) {
			t.Fatal("sync changed external namespace or state")
		}
	})

	t.Run("migration rejects symlinked owner namespace", func(t *testing.T) {
		root := testRoot(t)
		legacy := filepath.Join(cfg.global, "a")
		writeSkill(t, legacy, "legacy")
		files, _ := hashFolder(legacy)
		external := t.TempDir()
		if err := os.MkdirAll(cfg.lib, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(cfg.lib, "acme")); err != nil {
			t.Fatal(err)
		}
		st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/a": {Name: "a", Namespace: "acme", Path: canonicalRel("acme", "a"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/a/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files}}}
		if err := saveState(st); err != nil {
			t.Fatal(err)
		}
		beforeState, _ := os.ReadFile(cfg.state)
		if err := cmdMigrate([]string{"--pi-skills", filepath.Join(root, "pi")}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("expected confined migration failure, got %v", err)
		}
		payload, _ := os.ReadFile(filepath.Join(legacy, "SKILL.md"))
		entries, _ := os.ReadDir(external)
		afterState, _ := os.ReadFile(cfg.state)
		if string(payload) != "legacy" || len(entries) != 0 || string(afterState) != string(beforeState) {
			t.Fatal("migration changed legacy payload, external namespace, or state")
		}
	})
}

func TestGlobalMultiEnableCollisionHasNoPartialLink(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, ns := range []string{"one", "two"} {
		path := filepath.Join(root, "lib", ns, "a")
		writeSkill(t, path, ns)
		files, _ := hashFolder(path)
		st.Skills[ns+"/a"] = SkillState{Name: "a", Namespace: ns, Path: canonicalRel(ns, "a"), Origin: "github", SourceURL: "https://github.com/" + ns + "/repo.git", Owner: ns, Repo: "repo", SkillPath: "skills/a/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if err := cmdGlobal([]string{"enable", "one/a", "two/a"}); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("expected collision, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "skills", "a")); !os.IsNotExist(err) {
		t.Fatalf("partial global link remains: %v", err)
	}
	saved, _, _ := loadState()
	if saved.Skills["one/a"].Global || saved.Skills["two/a"].Global {
		t.Fatal("partial global state persisted")
	}
}

func TestForceAddUsesOnlyCurrentNpxOutput(t *testing.T) {
	root := testRoot(t)
	same := func(name string) LockSkill {
		return LockSkill{SourceURL: "https://github.com/acme/repo.git", SkillPath: "skills/" + name + "/SKILL.md"}
	}
	lock := &NpxLock{Version: 1, Skills: map[string]LockSkill{"chosen": same("chosen"), "other": same("other")}}
	if err := writeJSON(cfg.npxLock, lock); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(root, "lib", "acme", "chosen")
	writeSkill(t, lib, "old")
	files, _ := hashFolder(lib)
	chosen := SkillState{Name: "chosen", Namespace: "acme", Path: canonicalRel("acme", "chosen"), Origin: "npx-lock", SourceURL: same("chosen").SourceURL, Owner: "acme", Repo: "repo", SkillPath: same("chosen").SkillPath, PinnedRef: strings.Repeat("a", 40), Files: files, Global: true}
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/chosen": chosen}}
	saveState(st)
	if err := os.MkdirAll(cfg.global, 0755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(cfg.global, canonicalPath(chosen))
	if err := os.Symlink(rel, filepath.Join(cfg.global, "chosen")); err != nil {
		t.Fatal(err)
	}
	oldRun, oldLatest, oldSource := runCommand, latestCommitFunc, sourceToStage
	t.Cleanup(func() { runCommand = oldRun; latestCommitFunc = oldLatest; sourceToStage = oldSource })
	runCommand = func(string, ...string) error {
		if err := os.Remove(filepath.Join(cfg.global, "chosen")); err != nil {
			return err
		}
		writeSkill(t, filepath.Join(cfg.global, "chosen"), "npx-selected")
		return nil
	}
	latestCommitFunc = func(string, string) (string, error) { return strings.Repeat("b", 40), nil }
	sourceToStage = func(s SkillState, _ string) (string, func(), error) {
		d := t.TempDir()
		p := filepath.Join(d, "payload")
		writeSkill(t, p, "new-"+s.Name)
		return p, func() {}, nil
	}
	if err := cmdAdd([]string{"--force", "acme/repo"}); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := loadState()
	if _, ok := saved.Skills["acme/other"]; ok {
		t.Fatal("unselected same-repository lock entry adopted")
	}
	got, _ := os.ReadFile(filepath.Join(root, "lib", "acme", "chosen", "SKILL.md"))
	if string(got) != "new-chosen" {
		t.Fatalf("chosen skill not forced: %q", got)
	}
	if !saved.Skills["acme/chosen"].Global || !linkPointsTo(filepath.Join(cfg.global, "chosen"), filepath.Join(root, "lib", "acme", "chosen")) {
		t.Fatal("existing explicit global exposure was not preserved")
	}
	if _, err := os.Lstat(filepath.Join(cfg.global, "other")); !os.IsNotExist(err) {
		t.Fatalf("unselected skill was accidentally exposed: %v", err)
	}
}

func TestNpxCommandFailureRestoresManagedLinksAndRemovesOutput(t *testing.T) {
	root := testRoot(t)
	lib := filepath.Join(root, "lib", "acme", "chosen")
	writeSkill(t, lib, "canonical")
	files, err := hashFolder(lib)
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
	oldRun := runCommand
	t.Cleanup(func() { runCommand = oldRun })
	runCommand = func(string, ...string) error {
		if err := os.Remove(filepath.Join(cfg.global, "chosen")); err != nil {
			return err
		}
		writeSkill(t, filepath.Join(cfg.global, "chosen"), "temporary replacement")
		writeSkill(t, filepath.Join(cfg.global, "accidental"), "temporary output")
		return errors.New("injected npx failure")
	}
	if err := cmdAdd([]string{"--force", "acme/repo"}); err == nil || !strings.Contains(err.Error(), "injected npx failure") {
		t.Fatalf("expected npx failure, got %v", err)
	}
	if !linkPointsTo(filepath.Join(cfg.global, "chosen"), canonicalPath(chosen)) {
		t.Fatal("managed global link was not restored")
	}
	if _, err := os.Lstat(filepath.Join(cfg.global, "accidental")); !os.IsNotExist(err) {
		t.Fatalf("accidental npx output remains exposed: %v", err)
	}
	entries, err := os.ReadDir(cfg.global)
	if err != nil || len(entries) != 1 || entries[0].Name() != "chosen" {
		t.Fatalf("global filesystem was not restored: %v %v", entries, err)
	}
	afterState, err := os.ReadFile(cfg.state)
	if err != nil || string(afterState) != string(beforeState) {
		t.Fatalf("state changed after failed npx command: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(lib, "SKILL.md"))
	if err != nil || string(payload) != "canonical" {
		t.Fatalf("canonical payload changed: %q %v", payload, err)
	}
}

func TestNpxFailureRecoversRedirectedGlobalRoot(t *testing.T) {
	root := testRoot(t)
	lib := filepath.Join(root, "lib", "acme", "chosen")
	writeSkill(t, lib, "canonical")
	files, _ := hashFolder(lib)
	chosen := SkillState{Name: "chosen", Namespace: "acme", Path: canonicalRel("acme", "chosen"), Origin: "npx-lock", SourceURL: "https://github.com/acme/repo.git", Owner: "acme", Repo: "repo", SkillPath: "skills/chosen/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files, Global: true}
	if err := saveState(&State{Version: stateVersion, Skills: map[string]SkillState{"acme/chosen": chosen}}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	if err := os.MkdirAll(cfg.global, 0755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(cfg.global, canonicalPath(chosen))
	if err := os.Symlink(rel, filepath.Join(cfg.global, "chosen")); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "sentinel"), []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	oldRun := runCommand
	t.Cleanup(func() { runCommand = oldRun })
	runCommand = func(string, ...string) error {
		writeSkill(t, filepath.Join(cfg.global, "accidental"), "temporary")
		if err := os.RemoveAll(cfg.global); err != nil {
			return err
		}
		if err := os.Symlink(external, cfg.global); err != nil {
			return err
		}
		return errors.New("injected redirected-root npx failure")
	}
	if err := cmdAdd([]string{"--force", "acme/repo"}); err == nil || !strings.Contains(err.Error(), "injected redirected-root") {
		t.Fatalf("expected npx failure, got %v", err)
	}
	info, err := os.Lstat(cfg.global)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("global root was not safely recovered: %v %v", info, err)
	}
	if !linkPointsTo(filepath.Join(cfg.global, "chosen"), canonicalPath(chosen)) {
		t.Fatal("managed link was not restored after root recovery")
	}
	if _, err := os.Lstat(filepath.Join(cfg.global, "accidental")); !os.IsNotExist(err) {
		t.Fatalf("accidental output remains: %v", err)
	}
	sentinel, _ := os.ReadFile(filepath.Join(external, "sentinel"))
	externalEntries, _ := os.ReadDir(external)
	if string(sentinel) != "untouched" || len(externalEntries) != 1 {
		t.Fatalf("external target was mutated: %q %v", sentinel, externalEntries)
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after redirected-root npx failure")
	}
}

func TestForceAddDetectsByteIdenticalRewriteWithoutWidening(t *testing.T) {
	root := testRoot(t)
	lockSkill := func(name string) LockSkill {
		return LockSkill{SourceURL: "https://github.com/acme/repo.git", SkillPath: "skills/" + name + "/SKILL.md"}
	}
	lock := &NpxLock{Version: 1, Skills: map[string]LockSkill{"chosen": lockSkill("chosen"), "other": lockSkill("other")}}
	if err := writeJSON(cfg.npxLock, lock); err != nil {
		t.Fatal(err)
	}
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"chosen", "other"} {
		lib := filepath.Join(root, "lib", "acme", name)
		writeSkill(t, lib, "old-"+name)
		files, _ := hashFolder(lib)
		st.Skills["acme/"+name] = SkillState{Name: name, Namespace: "acme", Path: canonicalRel("acme", name), Origin: "npx-lock", SourceURL: lockSkill(name).SourceURL, Owner: "acme", Repo: "repo", SkillPath: lockSkill(name).SkillPath, PinnedRef: strings.Repeat("a", 40), Files: files}
		writeSkill(t, filepath.Join(cfg.global, name), "byte-identical-"+name)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	beforeChosen := pathSignature(filepath.Join(cfg.global, "chosen"))
	oldRun, oldLatest, oldSource := runCommand, latestCommitFunc, sourceToStage
	t.Cleanup(func() { runCommand, latestCommitFunc, sourceToStage = oldRun, oldLatest, oldSource })
	runCommand = func(string, ...string) error {
		chosenOutput := filepath.Join(cfg.global, "chosen")
		if err := os.RemoveAll(chosenOutput); err != nil {
			return err
		}
		writeSkill(t, chosenOutput, "byte-identical-chosen")
		if pathSignature(chosenOutput) == beforeChosen {
			return errors.New("test setup did not change output identity")
		}
		return nil
	}
	latestCommitFunc = func(string, string) (string, error) { return strings.Repeat("b", 40), nil }
	stagedNames := []string{}
	sourceToStage = func(s SkillState, _ string) (string, func(), error) {
		stagedNames = append(stagedNames, s.Name)
		d := t.TempDir()
		payload := filepath.Join(d, "payload")
		writeSkill(t, payload, "new-"+s.Name)
		return payload, func() {}, nil
	}
	if err := cmdAdd([]string{"--force", "acme/repo"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stagedNames, ",") != "chosen" {
		t.Fatalf("force selection widened beyond rewritten output: %v", stagedNames)
	}
	chosenPayload, _ := os.ReadFile(filepath.Join(root, "lib", "acme", "chosen", "SKILL.md"))
	otherPayload, _ := os.ReadFile(filepath.Join(root, "lib", "acme", "other", "SKILL.md"))
	if string(chosenPayload) != "new-chosen" || string(otherPayload) != "old-other" {
		t.Fatalf("wrong force adoption: chosen=%q other=%q", chosenPayload, otherPayload)
	}
	saved, _, _ := loadState()
	if saved.Skills["acme/other"].PinnedRef != strings.Repeat("a", 40) {
		t.Fatal("unselected same-repository state changed")
	}
}

func TestSymbolicRepairVerifiesCohortCandidate(t *testing.T) {
	root := testRoot(t)
	aRef := strings.Repeat("a", 40)
	bRef := strings.Repeat("b", 40)
	aDir := filepath.Join(root, "lib", "acme", "a")
	bDir := filepath.Join(root, "lib", "acme", "b")
	writeSkill(t, aDir, "A")
	writeSkill(t, bDir, "B")
	aFiles, _ := hashFolder(aDir)
	bFiles, _ := hashFolder(bDir)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/a": {Name: "a", Namespace: "acme", Path: canonicalRel("acme", "a"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/a/SKILL.md", PinnedRef: aRef, InstalledAt: "same", Files: aFiles}, "acme/b": {Name: "b", Namespace: "acme", Path: canonicalRel("acme", "b"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/b/SKILL.md", PinnedRef: "HEAD", InstalledAt: "same", Files: bFiles}}}
	oldDownload, oldLatest := downloadTarballFunc, latestCommitFunc
	t.Cleanup(func() { downloadTarballFunc = oldDownload; latestCommitFunc = oldLatest })
	downloadTarballFunc = func(_, _, ref, dest string) (string, error) {
		writeSkill(t, filepath.Join(dest, "skills", "a"), "A")
		content := "wrong"
		if ref == bRef {
			content = "B"
		}
		writeSkill(t, filepath.Join(dest, "skills", "b"), content)
		return dest, nil
	}
	latestCommitFunc = func(string, string) (string, error) { return bRef, nil }
	n, err := repairSymbolicPins(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || st.Skills["acme/b"].PinnedRef != bRef {
		t.Fatalf("unverified cohort used: n=%d pin=%s", n, st.Skills["acme/b"].PinnedRef)
	}
}

func TestLibraryTransactionRecoversRedirectedGlobalRootAndRollsBack(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	lib := filepath.Join(root, "lib", "local", "a")
	writeSkill(t, source, "new")
	writeSkill(t, lib, "old")
	files, _ := hashFolder(lib)
	skill := SkillState{Name: "a", Namespace: "local", Path: canonicalRel("local", "a"), Origin: "local", LocalDir: source, Files: files, Global: true}
	if err := saveState(&State{Version: stateVersion, Skills: map[string]SkillState{"local/a": skill}}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	if err := os.MkdirAll(cfg.global, 0755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(cfg.global, lib)
	if err := os.Symlink(rel, filepath.Join(cfg.global, "a")); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "sentinel"), []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	oldHook := beforeNamespaceRename
	redirected := false
	beforeNamespaceRename = func(_, _, _ string) error {
		if !redirected {
			redirected = true
			if err := os.RemoveAll(cfg.global); err != nil {
				return err
			}
			if err := os.Symlink(external, cfg.global); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { beforeNamespaceRename = oldHook })
	if err := cmdUpdate([]string{"local/a"}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected redirected global-root failure, got %v", err)
	}
	payload, _ := os.ReadFile(filepath.Join(lib, "SKILL.md"))
	if string(payload) != "old" {
		t.Fatalf("canonical payload was not rolled back: %q", payload)
	}
	info, err := os.Lstat(cfg.global)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("global root was not recovered: %v %v", info, err)
	}
	if !linkPointsTo(filepath.Join(cfg.global, "a"), lib) {
		t.Fatal("managed global link was not restored")
	}
	sentinel, _ := os.ReadFile(filepath.Join(external, "sentinel"))
	externalEntries, _ := os.ReadDir(external)
	if string(sentinel) != "untouched" || len(externalEntries) != 1 {
		t.Fatalf("external target was mutated: %q %v", sentinel, externalEntries)
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after redirected-root rollback")
	}
}

func TestLibraryUpdateRollsBackPayloadsWhenStateCommitFails(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"a", "b"} {
		src := filepath.Join(root, "src", name)
		lib := filepath.Join(root, "lib", "local", name)
		writeSkill(t, src, "new-"+name)
		writeSkill(t, lib, "old-"+name)
		files, _ := hashFolder(lib)
		st.Skills["local/"+name] = SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: src, Files: files}
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	oldRename := renamePath
	renamePath = func(from, to string) error {
		if to == cfg.state && strings.Contains(filepath.Base(from), ".state-") {
			return errors.New("injected state commit failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renamePath = oldRename })
	if err := cmdUpdate(nil); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected state commit failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		got, _ := os.ReadFile(filepath.Join(root, "lib", "local", name, "SKILL.md"))
		if string(got) != "old-"+name {
			t.Fatalf("%s payload was not rolled back: %q", name, got)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after failed commit")
	}
}

func TestNpxAddStagesEveryCandidateBeforeCommit(t *testing.T) {
	root := testRoot(t)
	if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{}}); err != nil {
		t.Fatal(err)
	}
	oldRun, oldLatest, oldSource := runCommand, latestCommitFunc, sourceToStage
	t.Cleanup(func() { runCommand = oldRun; latestCommitFunc = oldLatest; sourceToStage = oldSource })
	lockSkill := func(name string) LockSkill {
		return LockSkill{SourceURL: "https://github.com/acme/repo.git", SkillPath: "skills/" + name + "/SKILL.md"}
	}
	runCommand = func(string, ...string) error {
		if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{"a": lockSkill("a"), "b": lockSkill("b")}}); err != nil {
			return err
		}
		writeSkill(t, filepath.Join(cfg.global, "a"), "temporary-a")
		writeSkill(t, filepath.Join(cfg.global, "b"), "temporary-b")
		return nil
	}
	latestCommitFunc = func(string, string) (string, error) { return strings.Repeat("c", 40), nil }
	sourceToStage = func(s SkillState, _ string) (string, func(), error) {
		if s.Name == "b" {
			return "", func() {}, errors.New("later candidate failed")
		}
		d := t.TempDir()
		payload := filepath.Join(d, "payload")
		writeSkill(t, payload, "new-a")
		return payload, func() {}, nil
	}
	beforeState, _ := os.ReadFile(cfg.state)
	if err := cmdAdd([]string{"acme/repo"}); err == nil || !strings.Contains(err.Error(), "later candidate") {
		t.Fatalf("expected later candidate failure, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "lib", "acme", "a")); !os.IsNotExist(err) {
		t.Fatalf("first candidate was partially installed: %v", err)
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after candidate staging failure")
	}
	entries, err := os.ReadDir(cfg.global)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("npx output became accidental global exposure: %v", entries)
	}
}

func TestNpxAddRollsBackAllCandidatesWhenStateCommitFails(t *testing.T) {
	root := testRoot(t)
	if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{}}); err != nil {
		t.Fatal(err)
	}
	oldRun, oldLatest, oldSource, oldRename := runCommand, latestCommitFunc, sourceToStage, renamePath
	t.Cleanup(func() {
		runCommand, latestCommitFunc, sourceToStage, renamePath = oldRun, oldLatest, oldSource, oldRename
	})
	lockSkill := func(name string) LockSkill {
		return LockSkill{SourceURL: "https://github.com/acme/repo.git", SkillPath: "skills/" + name + "/SKILL.md"}
	}
	runCommand = func(string, ...string) error {
		return writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{"a": lockSkill("a"), "b": lockSkill("b")}})
	}
	latestCommitFunc = func(string, string) (string, error) { return strings.Repeat("d", 40), nil }
	sourceToStage = func(s SkillState, _ string) (string, func(), error) {
		d := t.TempDir()
		payload := filepath.Join(d, "payload")
		writeSkill(t, payload, "new-"+s.Name)
		return payload, func() {}, nil
	}
	renamePath = func(from, to string) error {
		if to == cfg.state && strings.Contains(filepath.Base(from), ".state-") {
			return errors.New("injected add state failure")
		}
		return os.Rename(from, to)
	}
	if err := cmdAdd([]string{"acme/repo"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected commit failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(root, "lib", "acme", name)); !os.IsNotExist(err) {
			t.Fatalf("candidate %s survived rollback: %v", name, err)
		}
	}
	saved, _, err := loadState()
	if err != nil || len(saved.Skills) != 0 {
		t.Fatalf("state survived failed add: %+v, %v", saved, err)
	}
}

func TestGlobalEnableRollsBackLinksAndStateOnLaterLinkFailure(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	beforeState, _ := os.ReadFile(cfg.state)
	oldSymlink := symlinkPath
	symlinkPath = func(target, link string) error {
		if filepath.Base(link) == "b" {
			return errors.New("injected later link failure")
		}
		return os.Symlink(target, link)
	}
	t.Cleanup(func() { symlinkPath = oldSymlink })
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected later link failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(cfg.global, name)); !os.IsNotExist(err) {
			t.Fatalf("partial link %s remains: %v", name, err)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("global state changed after link failure")
	}
}

func TestGlobalDisablePreflightsEveryOwnedLink(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	bad := filepath.Join(cfg.global, "b")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("unmanaged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdGlobal([]string{"disable", "local/a", "local/b"}); err == nil {
		t.Fatal("expected unmanaged later destination failure")
	}
	if !linkPointsTo(filepath.Join(cfg.global, "a"), filepath.Join(root, "lib", "local", "a")) {
		t.Fatal("first link was removed before later preflight failure")
	}
	got, _ := os.ReadFile(bad)
	if string(got) != "unmanaged" {
		t.Fatal("unmanaged path was changed")
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("global state changed after disable preflight failure")
	}
}

func TestGlobalEnableRollsBackLinksWhenStateCommitFails(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	beforeState, _ := os.ReadFile(cfg.state)
	oldRename := renamePath
	renamePath = func(from, to string) error {
		if to == cfg.state && strings.Contains(filepath.Base(from), ".state-") {
			return errors.New("injected global state failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renamePath = oldRename })
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected state failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Lstat(filepath.Join(cfg.global, name)); !os.IsNotExist(err) {
			t.Fatalf("link %s survived state rollback: %v", name, err)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after global commit failure")
	}
}

func TestGlobalDisableRollsBackAfterLaterRemoveFailure(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	oldRemove := removeLinkPath
	removeLinkPath = func(path string) error {
		if filepath.Base(path) == "b" {
			return errors.New("injected later remove failure")
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeLinkPath = oldRemove })
	if err := cmdGlobal([]string{"disable", "local/a", "local/b"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected later remove failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if !linkPointsTo(filepath.Join(cfg.global, name), filepath.Join(root, "lib", "local", name)) {
			t.Fatalf("link %s was not restored", name)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after remove rollback")
	}
}

func TestGlobalDisableRestoresLinksWhenStateCommitFails(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	oldRename := renamePath
	renamePath = func(from, to string) error {
		if to == cfg.state && strings.Contains(filepath.Base(from), ".state-") {
			return errors.New("injected disable state failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renamePath = oldRename })
	if err := cmdGlobal([]string{"disable", "local/a", "local/b"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected state failure, got %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if !linkPointsTo(filepath.Join(cfg.global, name), filepath.Join(root, "lib", "local", name)) {
			t.Fatalf("link %s was not restored", name)
		}
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after disable rollback")
	}
}

func TestGlobalEnableRejectsMissingCanonicalSourceBeforeLinking(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a", "local", "b")
	if err := os.RemoveAll(filepath.Join(root, "lib", "local", "b")); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(cfg.state)
	if err := cmdGlobal([]string{"enable", "local/a", "local/b"}); err == nil || !strings.Contains(err.Error(), "global source") {
		t.Fatalf("expected source preflight failure, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.global, "a")); !os.IsNotExist(err) {
		t.Fatalf("first link was created before source preflight: %v", err)
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed after source preflight failure")
	}
}

func TestGlobalEnableRejectsSymlinkedGlobalRootWithoutMutation(t *testing.T) {
	root := testRoot(t)
	localState(t, root, "local", "a")
	beforeState, _ := os.ReadFile(cfg.state)
	external := t.TempDir()
	if err := os.Symlink(external, cfg.global); err != nil {
		t.Fatal(err)
	}
	if err := cmdGlobal([]string{"enable", "local/a"}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected symlinked global root rejection, got %v", err)
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("external directory was mutated: %v", entries)
	}
	afterState, _ := os.ReadFile(cfg.state)
	if string(afterState) != string(beforeState) {
		t.Fatal("state changed despite unsafe global root")
	}
	info, err := os.Lstat(cfg.global)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("global root itself was changed: %v %v", info, err)
	}
}

func TestMigrationRejectsSymlinkedGlobalRootWithoutMutation(t *testing.T) {
	root := testRoot(t)
	external := t.TempDir()
	externalSkill := filepath.Join(external, "a")
	writeSkill(t, externalSkill, "external-payload")
	files, err := hashFolder(externalSkill)
	if err != nil {
		t.Fatal(err)
	}
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/a": {
		Name: "a", Namespace: "acme", Path: canonicalRel("acme", "a"), Origin: "github",
		SourceURL: "https://github.com/acme/repo.git", Owner: "acme", Repo: "repo",
		SkillPath: "skills/a/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files,
	}}}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(cfg.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, cfg.global); err != nil {
		t.Fatal(err)
	}
	if err := cmdMigrate([]string{"--pi-skills", filepath.Join(root, "pi")}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected symlinked migration root rejection, got %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(externalSkill, "SKILL.md"))
	if err != nil || string(payload) != "external-payload" {
		t.Fatalf("external payload was changed: %q %v", payload, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "lib", "acme", "a")); !os.IsNotExist(err) {
		t.Fatalf("external payload was moved into the library: %v", err)
	}
	afterState, err := os.ReadFile(cfg.state)
	if err != nil || string(afterState) != string(beforeState) {
		t.Fatalf("state changed despite unsafe migration root: %v", err)
	}
	info, err := os.Lstat(cfg.global)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("global root itself was changed: %v %v", info, err)
	}
}

func TestSymbolicRepairRejectsEmptyHashesWithoutVerification(t *testing.T) {
	testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/a": {
		Name: "a", Namespace: "acme", Path: canonicalRel("acme", "a"), Origin: "github",
		Owner: "acme", Repo: "repo", SkillPath: "skills/a/SKILL.md", PinnedRef: "HEAD", Files: map[string]string{},
	}}}
	oldDownload, oldLatest := downloadTarballFunc, latestCommitFunc
	t.Cleanup(func() { downloadTarballFunc, latestCommitFunc = oldDownload, oldLatest })
	downloadTarballFunc = func(_, _, _, _ string) (string, error) {
		t.Fatal("repair attempted verification without payload hashes")
		return "", nil
	}
	latestCommitFunc = func(string, string) (string, error) {
		t.Fatal("repair resolved a commit without payload hashes")
		return "", nil
	}
	if _, err := repairSymbolicPins(st); err == nil || !strings.Contains(err.Error(), "without recorded payload hashes") {
		t.Fatalf("expected closed verification failure, got %v", err)
	}
}

func TestSymbolicRepairContinuesWhenCandidateLacksSkill(t *testing.T) {
	root := testRoot(t)
	oldRef, latestRef := strings.Repeat("a", 40), strings.Repeat("b", 40)
	symbolicDir := filepath.Join(root, "lib", "acme", "new-skill")
	writeSkill(t, symbolicDir, "wanted")
	files, _ := hashFolder(symbolicDir)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{
		"acme/old":       {Name: "old", Namespace: "acme", Path: canonicalRel("acme", "old"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/old/SKILL.md", PinnedRef: oldRef, InstalledAt: "same", Files: files},
		"acme/new-skill": {Name: "new-skill", Namespace: "acme", Path: canonicalRel("acme", "new-skill"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/new-skill/SKILL.md", PinnedRef: "main", InstalledAt: "same", Files: files},
	}}
	oldDownload, oldLatest := downloadTarballFunc, latestCommitFunc
	t.Cleanup(func() { downloadTarballFunc, latestCommitFunc = oldDownload, oldLatest })
	latestResolved := false
	downloadTarballFunc = func(_, _, ref, dest string) (string, error) {
		writeSkill(t, filepath.Join(dest, "skills", "old"), "old")
		if ref == latestRef {
			writeSkill(t, filepath.Join(dest, "skills", "new-skill"), "wanted")
		}
		return dest, nil
	}
	latestCommitFunc = func(string, string) (string, error) {
		latestResolved = true
		return latestRef, nil
	}
	n, err := repairSymbolicPins(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !latestResolved || st.Skills["acme/new-skill"].PinnedRef != latestRef {
		t.Fatalf("missing candidate blocked valid repair: n=%d latest=%v pin=%s", n, latestResolved, st.Skills["acme/new-skill"].PinnedRef)
	}
}

func TestSymbolicRepairDoesNotTrustEmptyTimestampCohort(t *testing.T) {
	root := testRoot(t)
	oldRef, latestRef := strings.Repeat("a", 40), strings.Repeat("b", 40)
	symbolicDir := filepath.Join(root, "lib", "acme", "symbolic")
	writeSkill(t, symbolicDir, "wanted")
	files, _ := hashFolder(symbolicDir)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{
		"acme/old":      {Name: "old", Namespace: "acme", Path: canonicalRel("acme", "old"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/old/SKILL.md", PinnedRef: oldRef, InstalledAt: "", Files: files},
		"acme/symbolic": {Name: "symbolic", Namespace: "acme", Path: canonicalRel("acme", "symbolic"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/symbolic/SKILL.md", PinnedRef: "main", InstalledAt: "", Files: files},
	}}
	oldDownload, oldLatest := downloadTarballFunc, latestCommitFunc
	t.Cleanup(func() { downloadTarballFunc, latestCommitFunc = oldDownload, oldLatest })
	checkedOld := false
	downloadTarballFunc = func(_, _, ref, dest string) (string, error) {
		content := "wrong"
		if ref == oldRef {
			checkedOld = true
		}
		if ref == latestRef {
			content = "wanted"
		}
		writeSkill(t, filepath.Join(dest, "skills", "symbolic"), content)
		writeSkill(t, filepath.Join(dest, "skills", "old"), "wanted")
		return dest, nil
	}
	latestCommitFunc = func(string, string) (string, error) { return latestRef, nil }
	if n, err := repairSymbolicPins(st); err != nil || n != 1 {
		t.Fatalf("repair failed: n=%d err=%v", n, err)
	}
	if !checkedOld || st.Skills["acme/symbolic"].PinnedRef != latestRef {
		t.Fatalf("empty timestamp candidate was trusted without matching hashes: checked=%v pin=%s", checkedOld, st.Skills["acme/symbolic"].PinnedRef)
	}
}

func TestProjectCommitRollbackOnRenameFailure(t *testing.T) {
	root := testRoot(t)
	src := filepath.Join(root, "source")
	lib := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, src, "old")
	writeSkill(t, lib, "old")
	files, _ := hashFolder(lib)
	saveState(&State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: src, Files: files}}})
	project := t.TempDir()
	if err := cmdProject([]string{"init", "--project", project, "local/demo"}); err != nil {
		t.Fatal(err)
	}
	_, lockPath, skills := projectPaths(project)
	beforeLock, _ := os.ReadFile(lockPath)
	os.WriteFile(filepath.Join(skills, "demo", "SKILL.md"), []byte("drift"), 0644)
	oldRename := renamePath
	calls := 0
	renamePath = func(a, b string) error {
		calls++
		if calls == 4 {
			return errors.New("injected rename failure")
		}
		return os.Rename(a, b)
	}
	t.Cleanup(func() { renamePath = oldRename })
	if err := projectSync(project, true); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected injected failure, got %v (calls %d)", err, calls)
	}
	got, _ := os.ReadFile(filepath.Join(skills, "demo", "SKILL.md"))
	if string(got) != "drift" {
		t.Fatalf("payload rollback failed: %q", got)
	}
	afterLock, _ := os.ReadFile(lockPath)
	if string(beforeLock) != string(afterLock) {
		t.Fatal("lock rollback failed")
	}
}
