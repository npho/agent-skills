package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTOMLMultilineCommentsAndEmptySelection(t *testing.T) {
	root := testRoot(t)
	st := localState(t, root, "one", "a")
	if err := os.MkdirAll(cfg.profiles, 0755); err != nil {
		t.Fatal(err)
	}
	text := "# valid TOML\nextends = [\n]\nskills = [\n  \"one/a\", # comment\n]\n"
	if err := os.WriteFile(filepath.Join(cfg.profiles, "multi.toml"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	ids, err := resolveProfiles(st, []string{"multi"}, nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	ids, err = resolveProfiles(st, nil, nil)
	if err != nil || len(ids) != 0 {
		t.Fatalf("empty selection resolved %v, err=%v", ids, err)
	}
}

func TestProjectLockRejectsTraversalAndMismatch(t *testing.T) {
	root := testRoot(t)
	project := t.TempDir()
	_, lockPath, _ := projectPaths(project)
	bad := `{"version":1,"skills":{"../escape":{"name":"../escape","namespace":"x","libraryPath":"lib/x/../escape","origin":"local","localDir":"/tmp","installedAt":"x","files":{"SKILL.md":"x"}}}}`
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(bad), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectLock(lockPath); err == nil || !strings.Contains(err.Error(), "invalid project lock key") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
	bad = `{"version":1,"skills":{"safe":{"name":"other","namespace":"x","libraryPath":"lib/x/other","origin":"local","localDir":"/tmp","installedAt":"x","files":{"SKILL.md":"x"}}}}`
	os.WriteFile(lockPath, []byte(bad), 0644)
	if _, err := loadProjectLock(lockPath); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected mismatch rejection, got %v", err)
	}
	_ = root
}

func TestProjectSyncRejectsExtraAndNonDirectory(t *testing.T) {
	root := testRoot(t)
	source := filepath.Join(root, "source")
	writeSkill(t, source, "payload")
	canonical := filepath.Join(root, "lib", "local", "demo")
	writeSkill(t, canonical, "payload")
	files, _ := hashFolder(canonical)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"local/demo": {Name: "demo", Namespace: "local", Path: canonicalRel("local", "demo"), Origin: "local", LocalDir: source, Files: files}}}
	saveState(st)
	project := t.TempDir()
	if err := cmdProject([]string{"init", "--project", project, "local/demo"}); err != nil {
		t.Fatal(err)
	}
	_, _, skills := projectPaths(project)
	writeSkill(t, filepath.Join(skills, "extra"), "extra")
	if err := projectSync(project, false); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected unmanaged refusal, got %v", err)
	}
	os.RemoveAll(filepath.Join(skills, "extra"))
	os.RemoveAll(filepath.Join(skills, "demo"))
	os.WriteFile(filepath.Join(skills, "demo"), []byte("file"), 0644)
	if err := projectSync(project, false); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("expected non-directory refusal, got %v", err)
	}
}

func TestProjectUpdateStagesAllBeforeCommit(t *testing.T) {
	root := testRoot(t)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for _, name := range []string{"a", "b"} {
		src := filepath.Join(root, "sources", name)
		lib := filepath.Join(root, "lib", "local", name)
		writeSkill(t, src, "old-"+name)
		writeSkill(t, lib, "old-"+name)
		files, _ := hashFolder(lib)
		st.Skills["local/"+name] = SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: src, Files: files}
	}
	saveState(st)
	project := t.TempDir()
	if err := cmdProject([]string{"init", "--project", project, "local/a", "local/b"}); err != nil {
		t.Fatal(err)
	}
	_, lockPath, skills := projectPaths(project)
	beforeLock, _ := os.ReadFile(lockPath)
	os.WriteFile(filepath.Join(root, "sources", "a", "SKILL.md"), []byte("new-a"), 0644)
	os.Remove(filepath.Join(root, "sources", "b", "SKILL.md"))
	if err := projectUpdate(project, false, nil); err == nil {
		t.Fatal("expected staged source failure")
	}
	got, _ := os.ReadFile(filepath.Join(skills, "a", "SKILL.md"))
	if string(got) != "old-a" {
		t.Fatalf("partial payload commit: %q", got)
	}
	afterLock, _ := os.ReadFile(lockPath)
	if !bytes.Equal(beforeLock, afterLock) {
		t.Fatal("lock changed after failed update")
	}
}

func tarGz(t *testing.T, entries []struct {
	name string
	kind byte
	link string
}) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0644, Size: 1, Linkname: e.link}
		if e.kind != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func TestExtractTarRejectsTraversalAndLinks(t *testing.T) {
	cases := [][]struct {
		name string
		kind byte
		link string
	}{{{"repo/../../escape", tar.TypeReg, ""}}, {{"/absolute", tar.TypeReg, ""}}, {{"repo/link", tar.TypeSymlink, "/etc/passwd"}}, {{"repo/hard", tar.TypeLink, "repo/file"}}}
	for i, c := range cases {
		dest := t.TempDir()
		err := extractTarGz(bytes.NewReader(tarGz(t, c)), dest)
		if i < 2 {
			if err == nil {
				t.Fatalf("case %d accepted", i)
			}
		} else {
			if err != nil {
				t.Fatalf("case %d rejected: %v", i, err)
			}
		}
	}
}

func TestUpdateFailsClosedOnRevisionResolution(t *testing.T) {
	root := testRoot(t)
	path := filepath.Join(root, "lib", "acme", "demo")
	writeSkill(t, path, "x")
	files, _ := hashFolder(path)
	st := &State{Version: stateVersion, Skills: map[string]SkillState{"acme/demo": {Name: "demo", Namespace: "acme", Path: canonicalRel("acme", "demo"), Origin: "github", Owner: "acme", Repo: "repo", SkillPath: "skills/demo/SKILL.md", PinnedRef: strings.Repeat("a", 40), Files: files}}}
	saveState(st)
	old := latestCommitFunc
	latestCommitFunc = func(string, string) (string, error) { return "", errors.New("offline") }
	t.Cleanup(func() { latestCommitFunc = old })
	if err := cmdUpdate([]string{"acme/demo"}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("expected closed failure, got %v", err)
	}
	saved, _, _ := loadState()
	if saved.Skills["acme/demo"].PinnedRef != strings.Repeat("a", 40) {
		t.Fatal("pin mutated on resolution failure")
	}
}

func TestAddUsesNpxLockDiffAndCleansTemporaryOutput(t *testing.T) {
	testRoot(t)
	stale := LockSkill{SourceURL: "https://github.com/old/repo.git", SkillPath: "skills/stale/SKILL.md"}
	writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{"stale": stale}})
	oldRun, oldLatest, oldSource := runCommand, latestCommitFunc, sourceToStage
	t.Cleanup(func() { runCommand = oldRun; latestCommitFunc = oldLatest; sourceToStage = oldSource })
	runCommand = func(string, ...string) error {
		newSkill := LockSkill{SourceURL: "https://github.com/new/repo.git", SkillPath: "skills/fresh/SKILL.md"}
		if err := writeJSON(cfg.npxLock, &NpxLock{Version: 1, Skills: map[string]LockSkill{"stale": stale, "fresh": newSkill}}); err != nil {
			return err
		}
		writeSkill(t, filepath.Join(cfg.global, "fresh"), "temporary")
		return nil
	}
	latestCommitFunc = func(string, string) (string, error) { return strings.Repeat("b", 40), nil }
	sourceToStage = func(s SkillState, ref string) (string, func(), error) {
		d := t.TempDir()
		p := filepath.Join(d, "payload")
		writeSkill(t, p, s.Name)
		return p, func() {}, nil
	}
	if err := cmdAdd([]string{"--global", "new/repo"}); err != nil {
		t.Fatal(err)
	}
	state, _, _ := loadState()
	if _, ok := state.Skills["new/fresh"]; !ok {
		t.Fatal("new lock entry not adopted")
	}
	if _, ok := state.Skills["old/stale"]; ok {
		t.Fatal("stale lock entry adopted")
	}
	entries, _ := os.ReadDir(cfg.global)
	if len(entries) != 1 || entries[0].Name() != "fresh" {
		t.Fatalf("temporary output was not replaced by one exposure: %v", entries)
	}
	link := filepath.Join(cfg.global, "fresh")
	target, err := os.Readlink(link)
	if err != nil || filepath.IsAbs(target) {
		t.Fatalf("global exposure is not a relative symlink: %q %v", target, err)
	}
	if !state.Skills["new/fresh"].Global {
		t.Fatal("global exposure not recorded")
	}
}
