package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Profile struct{ Extends, Skills []string }
type ProjectManifest struct {
	Version          int
	Profiles, Skills []string
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

func parseSimpleTOML(path string, allowed map[string]bool) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]any{}
	scan := bufio.NewScanner(f)
	line := 0
	for scan.Scan() {
		line++
		text := strings.TrimSpace(stripTOMLComment(scan.Text()))
		if text == "" {
			continue
		}
		parts := strings.SplitN(text, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, line)
		}
		key := strings.TrimSpace(parts[0])
		if !allowed[key] {
			return nil, fmt.Errorf("%s:%d: unknown key %q", path, line, key)
		}
		if _, ok := out[key]; ok {
			return nil, fmt.Errorf("%s:%d: duplicate key %q", path, line, key)
		}
		raw := strings.TrimSpace(parts[1])
		if key == "version" {
			v, e := strconv.Atoi(raw)
			if e != nil {
				return nil, fmt.Errorf("%s:%d: invalid version", path, line)
			}
			out[key] = v
		} else {
			v, e := parseStringArray(raw)
			if e != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, line, e)
			}
			out[key] = v
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func stripTOMLComment(s string) string {
	quoted := false
	escaped := false
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
		}
		if r == '#' && !quoted {
			return s[:i]
		}
	}
	return s
}
func parseStringArray(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("expected an array of strings")
	}
	raw = strings.TrimSpace(raw[1 : len(raw)-1])
	if raw == "" {
		return []string{}, nil
	}
	var result []string
	for len(raw) > 0 {
		raw = strings.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '"' {
			return nil, fmt.Errorf("array entries must be quoted strings")
		}
		end := -1
		escaped := false
		for i := 1; i < len(raw); i++ {
			if escaped {
				escaped = false
				continue
			}
			if raw[i] == '\\' {
				escaped = true
				continue
			}
			if raw[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unterminated string")
		}
		var value string
		if err := json.Unmarshal([]byte(raw[:end+1]), &value); err != nil {
			return nil, err
		}
		result = append(result, value)
		raw = strings.TrimSpace(raw[end+1:])
		if raw == "" {
			break
		}
		if raw[0] != ',' {
			return nil, fmt.Errorf("expected comma")
		}
		raw = raw[1:]
	}
	return result, nil
}
func tomlArray(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		b, _ := json.Marshal(v)
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
func saveManifest(path string, m ProjectManifest) error {
	data := fmt.Sprintf("version = %d\nprofiles = %s\nskills = %s\n", m.Version, tomlArray(m.Profiles), tomlArray(m.Skills))
	if dryRun {
		fmt.Printf("  [dry-run] write %s\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0644)
}
func loadManifest(path string) (ProjectManifest, error) {
	m := ProjectManifest{Version: projectVersion}
	v, err := parseSimpleTOML(path, map[string]bool{"version": true, "profiles": true, "skills": true})
	if err != nil {
		return m, err
	}
	if x, ok := v["version"].(int); ok {
		m.Version = x
	}
	if m.Version != projectVersion {
		return m, fmt.Errorf("unsupported project manifest version %d", m.Version)
	}
	if x, ok := v["profiles"].([]string); ok {
		m.Profiles = x
	}
	if x, ok := v["skills"].([]string); ok {
		m.Skills = x
	}
	return m, nil
}
func loadProfile(name string) (Profile, error) {
	if err := validatePart("profile name", name); err != nil {
		return Profile{}, err
	}
	path := filepath.Join(cfg.profiles, name+".toml")
	v, err := parseSimpleTOML(path, map[string]bool{"extends": true, "skills": true})
	if err != nil {
		if os.IsNotExist(err) {
			return Profile{}, fmt.Errorf("profile %q does not exist (%s)", name, path)
		}
		return Profile{}, err
	}
	p := Profile{}
	if x, ok := v["extends"].([]string); ok {
		p.Extends = x
	}
	if x, ok := v["skills"].([]string); ok {
		p.Skills = x
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
func loadProjectLock(path string) (*ProjectLock, error) {
	var l ProjectLock
	if err := readJSON(path, &l); err != nil {
		return nil, err
	}
	if l.Version != projectVersion {
		return nil, fmt.Errorf("unsupported project lock version %d", l.Version)
	}
	if l.Skills == nil {
		l.Skills = map[string]ProjectLockSkill{}
	}
	return &l, nil
}
func lockFromState(s SkillState, files map[string]string) ProjectLockSkill {
	return ProjectLockSkill{Name: s.Name, Namespace: s.Namespace, LibraryPath: s.Path, Origin: s.Origin, SourceURL: s.SourceURL, Owner: s.Owner, Repo: s.Repo, SkillPath: s.SkillPath, LocalDir: s.LocalDir, Revision: s.PinnedRef, InstalledAt: time.Now().UTC().Format(time.RFC3339), Files: files}
}
func stateFromLock(s ProjectLockSkill) SkillState {
	return SkillState{Name: s.Name, Namespace: s.Namespace, Path: s.LibraryPath, Origin: s.Origin, SourceURL: s.SourceURL, Owner: s.Owner, Repo: s.Repo, SkillPath: s.SkillPath, LocalDir: s.LocalDir, PinnedRef: s.Revision, Files: s.Files}
}
func copyFresh(src, dest string) error {
	if dryRun {
		fmt.Printf("  [dry-run] copy %s -> %s\n", src, dest)
		return nil
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return copyDir(src, dest)
}
func seedProjectSkill(s SkillState, dest string) error {
	lib := canonicalPath(s)
	if dirExists(lib) {
		drift, err := hasDrift(s, lib)
		if err != nil {
			return err
		}
		if drift {
			return fmt.Errorf("library copy %s is drifted; run `skl check`", canonicalID(s.Namespace, s.Name))
		}
		return copyFresh(lib, dest)
	}
	if dryRun {
		fmt.Printf("  [dry-run] fetch %s for project\n", canonicalID(s.Namespace, s.Name))
		return nil
	}
	payload, cleanup, err := sourceToStage(s, s.PinnedRef)
	if err != nil {
		return err
	}
	defer cleanup()
	return copyFresh(payload, dest)
}

func reconcileProject(root string, m ProjectManifest, force bool) error {
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
	old, _ := loadProjectLock(lockPath)
	lock := &ProjectLock{Version: projectVersion, Skills: map[string]ProjectLockSkill{}}
	for _, id := range ids {
		s := st.Skills[id]
		dest := filepath.Join(skillsDir, s.Name)
		if info, statErr := os.Lstat(dest); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("project skill %s is a symlink; project skills must be real directories", s.Name)
			}
			oldSkill, managed := ProjectLockSkill{}, false
			if old != nil {
				oldSkill, managed = old.Skills[s.Name]
			}
			if !managed && !force {
				return fmt.Errorf("project path %s already exists but is not in the lock (use --force to replace)", dest)
			}
			if managed {
				drift, driftErr := hasProjectDrift(oldSkill, dest)
				if driftErr != nil {
					return driftErr
				}
				if drift && !force {
					return fmt.Errorf("project skill %s is locally modified; refusing refresh (use --force)", s.Name)
				}
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := seedProjectSkill(s, dest); err != nil {
			return err
		}
		files := s.Files
		if !dryRun {
			files, err = hashFolder(dest)
			if err != nil {
				return err
			}
		}
		lock.Skills[s.Name] = lockFromState(s, files)
	}
	// Remove deselected managed copies, but never an unknown directory.
	if old != nil {
		for name := range old.Skills {
			if _, ok := lock.Skills[name]; !ok {
				dest := filepath.Join(skillsDir, name)
				drift, e := hasProjectDrift(old.Skills[name], dest)
				if e == nil && drift && !force {
					return fmt.Errorf("deselected project skill %s is locally modified; use --force", name)
				}
				if dryRun {
					fmt.Printf("  [dry-run] remove %s\n", dest)
				} else if err := os.RemoveAll(dest); err != nil {
					return err
				}
			}
		}
	}
	return writeJSON(lockPath, lock)
}

func hasProjectDrift(s ProjectLockSkill, path string) (bool, error) {
	if !dirExists(path) {
		return false, nil
	}
	cur, err := hashFolder(path)
	if err != nil {
		return false, err
	}
	return !equalHashes(cur, s.Files), nil
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
	manifestPath, lockPath, skillsDir := projectPaths(root)
	switch sub {
	case "init":
		if fileExists(manifestPath) {
			return fmt.Errorf("project already initialized at %s", manifestPath)
		}
		m := ProjectManifest{Version: projectVersion, Profiles: f.profiles, Skills: f.args}
		if err := reconcileProject(root, m, f.force); err != nil {
			return err
		}
		return saveManifest(manifestPath, m)
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
		if err := reconcileProject(root, m, f.force); err != nil {
			return err
		}
		return saveManifest(manifestPath, m)
	case "refresh":
		m, err := loadManifest(manifestPath)
		if err != nil {
			return err
		}
		return reconcileProject(root, m, f.force)
	case "sync":
		lock, err := loadProjectLock(lockPath)
		if err != nil {
			return err
		}
		for name, s := range lock.Skills {
			dest := filepath.Join(skillsDir, name)
			if dirExists(dest) {
				drift, e := hasProjectDrift(s, dest)
				if e != nil {
					return e
				}
				if drift && !f.force {
					return fmt.Errorf("project skill %s is locally modified; refusing sync (use --force)", name)
				}
				if !drift {
					continue
				}
			}
			src := stateFromLock(s)
			if dryRun {
				fmt.Printf("  [dry-run] restore project skill %s from recorded source\n", name)
				continue
			}
			payload, cleanup, e := sourceToStage(src, s.Revision)
			if e != nil {
				return e
			}
			files, e := hashFolder(payload)
			if e != nil {
				cleanup()
				return e
			}
			if !equalHashes(files, s.Files) {
				cleanup()
				return fmt.Errorf("recorded source for %s no longer reproduces lock hashes", name)
			}
			e = copyFresh(payload, dest)
			cleanup()
			if e != nil {
				return e
			}
		}
		return nil
	case "update":
		lock, err := loadProjectLock(lockPath)
		if err != nil {
			return err
		}
		selected := map[string]bool{}
		if len(f.args) > 0 {
			for _, x := range f.args {
				matched := false
				for name, ls := range lock.Skills {
					if x == name || x == canonicalID(ls.Namespace, name) {
						matched = true
						selected[name] = true
					}
				}
				if !matched {
					return fmt.Errorf("project lock has no skill %q", x)
				}
			}
		}
		for name, ls := range lock.Skills {
			if len(selected) > 0 && !selected[name] {
				continue
			}
			dest := filepath.Join(skillsDir, name)
			drift, e := hasProjectDrift(ls, dest)
			if e != nil {
				return e
			}
			if drift && !f.force {
				return fmt.Errorf("project skill %s is locally modified; refusing update (use --force)", name)
			}
			src := stateFromLock(ls)
			ref := ""
			if src.LocalDir == "" {
				ref = resolveLatest(src.Owner, src.Repo)
			}
			if dryRun {
				fmt.Printf("  [dry-run] update project skill %s from original source\n", name)
				continue
			}
			payload, cleanup, e := sourceToStage(src, ref)
			if e != nil {
				return e
			}
			e = copyFresh(payload, dest)
			cleanup()
			if e != nil {
				return e
			}
			files, e := hashFolder(dest)
			if e != nil {
				return e
			}
			ls.Files = files
			ls.InstalledAt = time.Now().UTC().Format(time.RFC3339)
			if ref != "" {
				ls.Revision = ref
			}
			lock.Skills[name] = ls
		}
		return writeJSON(lockPath, lock)
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

var _ = sort.Strings
