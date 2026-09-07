package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Profile struct {
	Extends []string `toml:"extends"`
	Skills  []string `toml:"skills"`
}
type ProjectManifest struct {
	Version  int      `toml:"version"`
	Profiles []string `toml:"profiles"`
	Skills   []string `toml:"skills"`
}
type ProjectLockSkill struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	LibraryPath string            `json:"libraryPath"`
	Origin      string            `json:"origin"`
	SourceURL   string            `json:"sourceUrl,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	SkillPath   string            `json:"skillPath,omitempty"`
	LocalDir    string            `json:"localDir,omitempty"`
	Revision    string            `json:"revision,omitempty"`
	InstalledAt string            `json:"installedAt"`
	Files       map[string]string `json:"files"`
}
type ProjectLock struct {
	Version int                         `json:"version"`
	Skills  map[string]ProjectLockSkill `json:"skills"`
}

func rejectUnknownTOML(path string, keys []toml.Key) error {
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.String()
	}
	return fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(names, ", "))
}
func encodeManifest(m ProjectManifest) ([]byte, error) {
	var b bytes.Buffer
	err := toml.NewEncoder(&b).Encode(m)
	return b.Bytes(), err
}
func saveManifest(path string, m ProjectManifest) error {
	data, err := encodeManifest(m)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("  [dry-run] write %s\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
func loadManifest(path string) (ProjectManifest, error) {
	m := ProjectManifest{Version: projectVersion}
	if info, err := os.Lstat(path); err != nil {
		return m, err
	} else if !info.Mode().IsRegular() {
		return m, fmt.Errorf("%s is not a regular manifest file", path)
	}
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return m, err
	}
	if err := rejectUnknownTOML(path, md.Undecoded()); err != nil {
		return m, err
	}
	if m.Version != projectVersion {
		return m, fmt.Errorf("unsupported project manifest version %d", m.Version)
	}
	return m, nil
}
func loadProfile(name string) (Profile, error) {
	if err := validatePart("profile name", name); err != nil {
		return Profile{}, err
	}
	path := filepath.Join(cfg.profiles, name+".toml")
	var p Profile
	md, err := toml.DecodeFile(path, &p)
	if err != nil {
		if os.IsNotExist(err) {
			return p, fmt.Errorf("profile %q does not exist (%s)", name, path)
		}
		return p, err
	}
	if err := rejectUnknownTOML(path, md.Undecoded()); err != nil {
		return p, err
	}
	return p, nil
}

func resolveProfiles(st *State, names, explicit []string) ([]string, error) {
	var selectors []string
	visiting := map[string]bool{}
	done := map[string]bool{}
	var stack []string
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("profile cycle: %s -> %s", strings.Join(stack, " -> "), name)
		}
		if done[name] {
			return nil
		}
		p, err := loadProfile(name)
		if err != nil {
			return err
		}
		visiting[name] = true
		stack = append(stack, name)
		for _, base := range p.Extends {
			if err := visit(base); err != nil {
				return err
			}
		}
		selectors = append(selectors, p.Skills...)
		stack = stack[:len(stack)-1]
		visiting[name] = false
		done[name] = true
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	selectors = append(selectors, explicit...)
	if len(selectors) == 0 {
		return []string{}, nil
	}
	ids, err := resolveSelectors(st, selectors)
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, id := range ids {
		s := st.Skills[id]
		if old, ok := byName[s.Name]; ok && old != id {
			return nil, fmt.Errorf("skill-name collision: %s and %s both install as .agents/skills/%s", old, id, s.Name)
		}
		byName[s.Name] = id
	}
	return ids, nil
}
func cmdProfile(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: skl profile list|show <name>")
	}
	switch args[0] {
	case "list":
		entries, err := os.ReadDir(cfg.profiles)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
				fmt.Println(strings.TrimSuffix(e.Name(), ".toml"))
			}
		}
		return nil
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: skl profile show <name>")
		}
		st, _, err := loadState()
		if err != nil {
			return err
		}
		ids, err := resolveProfiles(st, []string{args[1]}, nil)
		if err != nil {
			return err
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		return nil
	default:
		return fmt.Errorf("unknown profile command %q", args[0])
	}
}

func projectPaths(root string) (string, string, string) {
	base := filepath.Join(root, ".agents")
	return filepath.Join(base, "skills.toml"), filepath.Join(base, "skills.lock.json"), filepath.Join(base, "skills")
}
func projectRoot(f commonFlags) string {
	if f.project != "" {
		return f.project
	}
	cwd, _ := os.Getwd()
	return cwd
}
func safeProjectDest(skillsDir, name string) (string, error) {
	if err := validatePart("project skill name", name); err != nil {
		return "", err
	}
	dest := filepath.Join(skillsDir, name)
	rel, err := filepath.Rel(skillsDir, dest)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project skill %q escapes %s", name, skillsDir)
	}
	return dest, nil
}
func validateProjectLock(l *ProjectLock) error {
	if l.Version != projectVersion {
		return fmt.Errorf("unsupported project lock version %d", l.Version)
	}
	if l.Skills == nil {
		l.Skills = map[string]ProjectLockSkill{}
	}
	for key, s := range l.Skills {
		if err := validatePart("project lock key", key); err != nil {
			return err
		}
		if s.Name != key {
			return fmt.Errorf("project lock key %q does not match record name %q", key, s.Name)
		}
		if err := validatePart("namespace", s.Namespace); err != nil {
			return fmt.Errorf("project lock %s: %w", key, err)
		}
		if s.LibraryPath != canonicalRel(s.Namespace, s.Name) {
			return fmt.Errorf("project lock %s has incoherent libraryPath %q", key, s.LibraryPath)
		}
		if s.Origin == "local" {
			if s.LocalDir == "" || s.Namespace != "local" {
				return fmt.Errorf("project lock %s has incoherent local provenance", key)
			}
		} else {
			if validatePart("owner", s.Owner) != nil || validatePart("repo", s.Repo) != nil || s.SkillPath == "" || s.Namespace != s.Owner {
				return fmt.Errorf("project lock %s has incomplete or incoherent GitHub provenance", key)
			}
			folder := filepath.Clean(filepath.FromSlash(folderOfSkillPath(s.SkillPath)))
			if folder == "." || folder == ".." || filepath.IsAbs(folder) || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
				return fmt.Errorf("project lock %s has unsafe skillPath %q", key, s.SkillPath)
			}
			if s.SourceURL != "" {
				owner, repo, ok := parseGitHubURL(s.SourceURL)
				if !ok || owner != s.Owner || repo != s.Repo {
					return fmt.Errorf("project lock %s sourceUrl disagrees with owner/repo", key)
				}
			}
			if !isCommitSHA(s.Revision) {
				return fmt.Errorf("project lock %s has non-immutable revision %q", key, s.Revision)
			}
		}
		if len(s.Files) == 0 {
			return fmt.Errorf("project lock %s has no content hashes", key)
		}
	}
	return nil
}
func loadProjectLock(path string) (*ProjectLock, error) {
	var l ProjectLock
	if info, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular lock file", path)
	}
	if err := readJSON(path, &l); err != nil {
		return nil, err
	}
	if err := validateProjectLock(&l); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &l, nil
}
func lockFromState(s SkillState, files map[string]string) ProjectLockSkill {
	return ProjectLockSkill{Name: s.Name, Namespace: s.Namespace, LibraryPath: s.Path, Origin: s.Origin, SourceURL: s.SourceURL, Owner: s.Owner, Repo: s.Repo, SkillPath: s.SkillPath, LocalDir: s.LocalDir, Revision: s.PinnedRef, InstalledAt: time.Now().UTC().Format(time.RFC3339), Files: files}
}
func stateFromLock(s ProjectLockSkill) SkillState {
	return SkillState{Name: s.Name, Namespace: s.Namespace, Path: s.LibraryPath, Origin: s.Origin, SourceURL: s.SourceURL, Owner: s.Owner, Repo: s.Repo, SkillPath: s.SkillPath, LocalDir: s.LocalDir, PinnedRef: s.Revision, Files: s.Files}
}
func hasProjectDrift(s ProjectLockSkill, path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	cur, err := hashFolder(path)
	if err != nil {
		return true, err
	}
	return !equalHashes(cur, s.Files), nil
}

func inspectProjectTree(skillsDir string, lock *ProjectLock, force bool) error {
	entries, err := os.ReadDir(skillsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := safeProjectDest(skillsDir, e.Name()); err != nil {
			return err
		}
		s, managed := lock.Skills[e.Name()]
		if !managed {
			if !force {
				return fmt.Errorf("unmanaged project skill entry %s; use --force to reconcile it", e.Name())
			}
			continue
		}
		drift, err := hasProjectDrift(s, filepath.Join(skillsDir, e.Name()))
		if err != nil {
			return fmt.Errorf("inspect project skill %s: %w", e.Name(), err)
		}
		if drift && !force {
			return fmt.Errorf("project skill %s is locally modified or not a real directory; use --force", e.Name())
		}
	}
	return nil
}
func copyIntoStage(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return copyDir(src, dest)
}
func sourceIntoStage(s SkillState, ref, dest string, expected map[string]string) error {
	payload, cleanup, err := sourceToStage(s, ref)
	if err != nil {
		return err
	}
	defer cleanup()
	files, err := hashFolder(payload)
	if err != nil {
		return err
	}
	if expected != nil && !equalHashes(files, expected) {
		return fmt.Errorf("recorded source for %s does not reproduce lock hashes", s.Name)
	}
	return copyIntoStage(payload, dest)
}

// commitProject swaps the complete skills tree and metadata, rolling back every
// rename if a later rename fails. All fetching and validation happen beforehand.
func commitProject(root, stageSkills string, lock *ProjectLock, manifest *ProjectManifest) error {
	manifestPath, lockPath, skillsDir := projectPaths(root)
	if dryRun {
		fmt.Printf("  [dry-run] atomically replace %s and %s\n", skillsDir, lockPath)
		if manifest != nil {
			fmt.Printf("  [dry-run] write %s\n", manifestPath)
		}
		return nil
	}
	base := filepath.Dir(skillsDir)
	if err := os.MkdirAll(base, 0755); err != nil {
		return err
	}
	token := fmt.Sprintf(".skl-backup-%d", time.Now().UnixNano())
	type move struct {
		live, backup string
		had          bool
	}
	moves := []move{{skillsDir, filepath.Join(base, token+"-skills"), false}, {lockPath, filepath.Join(base, token+"-lock"), false}}
	if manifest != nil {
		moves = append(moves, move{manifestPath, filepath.Join(base, token+"-manifest"), false})
	}
	stageLock := filepath.Join(filepath.Dir(stageSkills), "skills.lock.json")
	if err := writeJSONDirect(stageLock, lock); err != nil {
		return err
	}
	stageManifest := ""
	if manifest != nil {
		data, err := encodeManifest(*manifest)
		if err != nil {
			return err
		}
		stageManifest = filepath.Join(filepath.Dir(stageSkills), "skills.toml")
		if err := os.WriteFile(stageManifest, data, 0644); err != nil {
			return err
		}
	}
	for i := range moves {
		if _, err := os.Lstat(moves[i].live); err == nil {
			if err := projectRename(moves[i].live, moves[i].backup); err != nil {
				for j := i - 1; j >= 0; j-- {
					if moves[j].had {
						_ = os.Rename(moves[j].backup, moves[j].live)
					}
				}
				return err
			}
			moves[i].had = true
		} else if !os.IsNotExist(err) {
			for j := i - 1; j >= 0; j-- {
				if moves[j].had {
					_ = os.Rename(moves[j].backup, moves[j].live)
				}
			}
			return err
		}
	}
	staged := []string{stageSkills, stageLock}
	if manifest != nil {
		staged = append(staged, stageManifest)
	}
	installed := 0
	for i, m := range moves {
		if err := projectRename(staged[i], m.live); err != nil {
			for j := installed - 1; j >= 0; j-- {
				_ = os.RemoveAll(moves[j].live)
			}
			for j := len(moves) - 1; j >= 0; j-- {
				if moves[j].had {
					_ = os.Rename(moves[j].backup, moves[j].live)
				}
			}
			return err
		}
		installed++
	}
	for _, m := range moves {
		if m.had {
			_ = os.RemoveAll(m.backup)
		}
	}
	return nil
}
func writeJSONDirect(path string, v any) error {
	old := dryRun
	dryRun = false
	err := writeJSON(path, v)
	dryRun = old
	return err
}
func makeProjectStage(root string) (string, string, func(), error) {
	base := filepath.Join(root, ".agents")
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", "", func() {}, err
	}
	dir, err := os.MkdirTemp(base, ".skl-project-")
	if err != nil {
		return "", "", func() {}, err
	}
	skills := filepath.Join(dir, "skills")
	if err := os.Mkdir(skills, 0755); err != nil {
		os.RemoveAll(dir)
		return "", "", func() {}, err
	}
	return dir, skills, func() { _ = os.RemoveAll(dir) }, nil
}

func reconcileProject(root string, m ProjectManifest, force bool, writeManifest bool) error {
	st, migrated, err := loadState()
	if err != nil {
		return err
	}
	if migrated {
		return fmt.Errorf("run `skl migrate` first")
	}
	ids, err := resolveProfiles(st, m.Profiles, m.Skills)
	if err != nil {
		return err
	}
	_, lockPath, skillsDir := projectPaths(root)
	old := &ProjectLock{Version: projectVersion, Skills: map[string]ProjectLockSkill{}}
	if _, statErr := os.Lstat(lockPath); statErr == nil {
		old, err = loadProjectLock(lockPath)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := inspectProjectTree(skillsDir, old, force); err != nil {
		return err
	}
	lock := &ProjectLock{Version: projectVersion, Skills: map[string]ProjectLockSkill{}}
	if dryRun {
		for _, id := range ids {
			fmt.Printf("  [dry-run] stage %s for project\n", id)
		}
		var mp *ProjectManifest
		if writeManifest {
			mp = &m
		}
		return commitProject(root, "", lock, mp)
	}
	_, stageSkills, cleanup, err := makeProjectStage(root)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, id := range ids {
		s := st.Skills[id]
		dest, _ := safeProjectDest(stageSkills, s.Name)
		lib := canonicalPath(s)
		if info, e := os.Lstat(lib); e == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			drift, e := hasDrift(s, lib)
			if e != nil {
				return e
			}
			if drift {
				return fmt.Errorf("library copy %s is drifted", id)
			}
			if err := copyIntoStage(lib, dest); err != nil {
				return err
			}
		} else {
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if err := sourceIntoStage(s, s.PinnedRef, dest, s.Files); err != nil {
				return err
			}
		}
		files, err := hashFolder(dest)
		if err != nil {
			return err
		}
		lock.Skills[s.Name] = lockFromState(s, files)
	}
	if err := validateProjectLock(lock); err != nil {
		return err
	}
	var mp *ProjectManifest
	if writeManifest {
		mp = &m
	}
	return commitProject(root, stageSkills, lock, mp)
}

func projectSync(root string, force bool) error {
	_, lockPath, skillsDir := projectPaths(root)
	lock, err := loadProjectLock(lockPath)
	if err != nil {
		return err
	}
	if err := inspectProjectTree(skillsDir, lock, force); err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("  [dry-run] reproduce %d locked project skills atomically\n", len(lock.Skills))
		return nil
	}
	_, stageSkills, cleanup, err := makeProjectStage(root)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, name := range sortedProjectNames(lock) {
		s := lock.Skills[name]
		dest, _ := safeProjectDest(stageSkills, name)
		live, _ := safeProjectDest(skillsDir, name)
		drift, e := hasProjectDrift(s, live)
		if e != nil {
			return e
		}
		if !drift {
			if err := copyIntoStage(live, dest); err != nil {
				return err
			}
			continue
		}
		if err := sourceIntoStage(stateFromLock(s), s.Revision, dest, s.Files); err != nil {
			return err
		}
	}
	return commitProject(root, stageSkills, lock, nil)
}
func projectUpdate(root string, force bool, selectors []string) error {
	_, lockPath, skillsDir := projectPaths(root)
	lock, err := loadProjectLock(lockPath)
	if err != nil {
		return err
	}
	if err := inspectProjectTree(skillsDir, lock, force); err != nil {
		return err
	}
	selected := map[string]bool{}
	if len(selectors) > 0 {
		for _, x := range selectors {
			matched := ""
			for name, s := range lock.Skills {
				if x == name || x == canonicalID(s.Namespace, name) {
					if matched != "" && matched != name {
						return fmt.Errorf("project selector %q is ambiguous", x)
					}
					matched = name
				}
			}
			if matched == "" {
				return fmt.Errorf("project lock has no skill %q", x)
			}
			selected[matched] = true
		}
	}
	if dryRun {
		fmt.Printf("  [dry-run] update project skills atomically\n")
		return nil
	}
	_, stageSkills, cleanup, err := makeProjectStage(root)
	if err != nil {
		return err
	}
	defer cleanup()
	cache := map[string]string{}
	for _, name := range sortedProjectNames(lock) {
		ls := lock.Skills[name]
		dest, _ := safeProjectDest(stageSkills, name)
		live, _ := safeProjectDest(skillsDir, name)
		doUpdate := len(selected) == 0 || selected[name]
		if !doUpdate {
			drift, e := hasProjectDrift(ls, live)
			if e != nil {
				return e
			}
			if !drift {
				if err := copyIntoStage(live, dest); err != nil {
					return err
				}
			} else if err := sourceIntoStage(stateFromLock(ls), ls.Revision, dest, ls.Files); err != nil {
				return err
			}
			continue
		}
		src := stateFromLock(ls)
		ref := ""
		if src.LocalDir == "" {
			ref, err = resolveLatestCached(src.Owner, src.Repo, cache)
			if err != nil {
				return err
			}
		}
		if err := sourceIntoStage(src, ref, dest, nil); err != nil {
			return err
		}
		files, err := hashFolder(dest)
		if err != nil {
			return err
		}
		ls.Files = files
		ls.InstalledAt = time.Now().UTC().Format(time.RFC3339)
		if ref != "" {
			ls.Revision = ref
		}
		lock.Skills[name] = ls
	}
	if err := validateProjectLock(lock); err != nil {
		return err
	}
	return commitProject(root, stageSkills, lock, nil)
}
func sortedProjectNames(l *ProjectLock) []string {
	r := make([]string, 0, len(l.Skills))
	for n := range l.Skills {
		r = append(r, n)
	}
	sort.Strings(r)
	return r
}

func cmdProject(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: skl project init|add|refresh|sync|update")
	}
	sub := args[0]
	f, err := parseProjectFlags(args[1:])
	if err != nil {
		return err
	}
	root := projectRoot(f.commonFlags)
	manifestPath, _, _ := projectPaths(root)
	switch sub {
	case "init":
		if _, err := os.Lstat(manifestPath); err == nil {
			return fmt.Errorf("project already initialized at %s", manifestPath)
		}
		m := ProjectManifest{Version: projectVersion, Profiles: f.profiles, Skills: f.args}
		return reconcileProject(root, m, f.force, true)
	case "add":
		if len(f.args) == 0 {
			return fmt.Errorf("usage: skl project add [--force] <skills...>")
		}
		m, err := loadManifest(manifestPath)
		if err != nil {
			return err
		}
		for _, s := range f.args {
			if !containsString(m.Skills, s) {
				m.Skills = append(m.Skills, s)
			}
		}
		return reconcileProject(root, m, f.force, true)
	case "refresh":
		m, err := loadManifest(manifestPath)
		if err != nil {
			return err
		}
		return reconcileProject(root, m, f.force, false)
	case "sync":
		return projectSync(root, f.force)
	case "update":
		return projectUpdate(root, f.force, f.args)
	default:
		return fmt.Errorf("unknown project command %q", sub)
	}
}

type projectFlags struct {
	commonFlags
	profiles []string
}

func parseProjectFlags(args []string) (projectFlags, error) {
	var f projectFlags
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			f.force = true
		case "--project":
			if i+1 == len(args) {
				return f, fmt.Errorf("--project requires a directory")
			}
			i++
			f.project = args[i]
		case "--profile":
			if i+1 == len(args) {
				return f, fmt.Errorf("--profile requires a name")
			}
			i++
			f.profiles = append(f.profiles, args[i])
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, fmt.Errorf("unknown option %s", args[i])
			}
			f.args = append(f.args, args[i])
		}
	}
	return f, nil
}
func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
