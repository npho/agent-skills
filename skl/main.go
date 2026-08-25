// skl — a thin skill manager for ~/.agents.
//
// skl consumes (never writes) the .skill-lock.json produced by `npx skills`,
// and owns .sync-state.json, which pins upstream commits and records file
// hashes so drift can be detected.
//
// Commands: add, sync, check, update, promote.
// Global flag: -n / --dry-run (may appear anywhere on the command line).
package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	root         string
	skillsDir    string
	overridesDir string
	lockPath     string
	statePath    string
	piSkillsDir  string
	dryRun       bool
)

func initPaths() {
	root = os.Getenv("SKL_ROOT")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".agents")
	}
	skillsDir = filepath.Join(root, "skills")
	overridesDir = filepath.Join(root, "overrides")
	lockPath = filepath.Join(root, ".skill-lock.json")
	statePath = filepath.Join(root, ".sync-state.json")
	piSkillsDir = os.Getenv("SKL_PI_SKILLS")
	if piSkillsDir == "" {
		home, _ := os.UserHomeDir()
		piSkillsDir = filepath.Join(home, ".pi", "agent", "skills")
	}
}

// ---------- data types ----------

type LockSkill struct {
	Source          string `json:"source"`
	SourceType      string `json:"sourceType"`
	SourceURL       string `json:"sourceUrl"`
	SkillPath       string `json:"skillPath"`
	SkillFolderHash string `json:"skillFolderHash"`
	PluginName      string `json:"pluginName"`
	InstalledAt     string `json:"installedAt"`
	UpdatedAt       string `json:"updatedAt"`
}

type Lock struct {
	Version   int                  `json:"version"`
	Skills    map[string]LockSkill `json:"skills"`
	Dismissed map[string]bool      `json:"dismissed,omitempty"`
}

type SkillState struct {
	Origin      string            `json:"origin"` // "lock" | "external"
	SourceURL   string            `json:"sourceUrl,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	SkillPath   string            `json:"skillPath,omitempty"`
	LocalDir    string            `json:"localDir,omitempty"`
	PinnedRef   string            `json:"pinnedRef,omitempty"`
	Override    bool              `json:"override,omitempty"`
	InstalledAt string            `json:"installedAt"`
	Files       map[string]string `json:"files"`
}

type State struct {
	Version int                   `json:"version"`
	Skills  map[string]SkillState `json:"skills"`
}

// ---------- io helpers ----------

func loadLock() (*Lock, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read lock file %s: %w (have you run `npx skills add` yet?)", lockPath, err)
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", lockPath, err)
	}
	return &l, nil
}

func loadState() *State {
	st := &State{Version: 1, Skills: map[string]SkillState{}}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, st)
	if st.Skills == nil {
		st.Skills = map[string]SkillState{}
	}
	return st
}

func (s *State) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, append(data, '\n'), 0644)
}

// do runs fn, or prints the action if --dry-run is set.
func do(desc string, fn func() error) error {
	if dryRun {
		fmt.Printf("  [dry-run] %s\n", desc)
		return nil
	}
	return fn()
}

// runCmd runs an external command with inherited stdio (or just prints it under dry-run).
func runCmd(name string, args ...string) error {
	if dryRun {
		fmt.Printf("  [dry-run] exec: %s %s\n", name, strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(name, args...)
	home, _ := os.UserHomeDir()
	cmd.Dir = home // keep `npx skills` from seeing ~/.agents (a git repo) as a "project"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// resolveRef returns a pinned ref for owner/repo: the state pin if present,
// otherwise the current HEAD commit SHA (falling back to "HEAD" on API failure).
func resolveRef(st *State, name, owner, repo string) string {
	if s, ok := st.Skills[name]; ok && s.PinnedRef != "" {
		return s.PinnedRef
	}
	return resolveLatest(owner, repo)
}

func resolveLatest(owner, repo string) string {
	if owner == "" || repo == "" {
		return ""
	}
	if dryRun {
		return "HEAD (would resolve latest commit)"
	}
	sha, err := latestCommit(owner, repo)
	if err != nil {
		fmt.Printf("  warning: could not resolve latest commit for %s/%s (%v); using unpinned HEAD\n", owner, repo, err)
		return "HEAD"
	}
	return sha
}

func skillDest(name string) string { return filepath.Join(skillsDir, name) }

// installFromGitHub fetches owner/repo@ref and copies the skill folder to skillsDir/name.
func installFromGitHub(name, owner, repo, ref, skillPath string) error {
	tmp, err := os.MkdirTemp("", "skl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	top, err := downloadTarball(owner, repo, ref, tmp)
	if err != nil {
		return err
	}
	src := filepath.Join(top, filepath.FromSlash(folderOfSkillPath(skillPath)))
	if !dirExists(src) {
		return fmt.Errorf("skill folder %q not found in %s/%s@%s", folderOfSkillPath(skillPath), owner, repo, ref)
	}
	dest := skillDest(name)
	if err := removeDir(dest); err != nil {
		return err
	}
	return copyDir(src, dest)
}

func applyOverride(name string) (bool, error) {
	src := filepath.Join(overridesDir, name)
	if !dirExists(src) {
		return false, nil
	}
	return true, copyDir(src, skillDest(name))
}

func recordState(st *State, name string, mutate func(*SkillState)) error {
	if dryRun {
		return nil
	}
	s, exists := st.Skills[name]
	files, err := hashFolder(skillDest(name))
	if err != nil {
		return fmt.Errorf("hashing %s: %w", name, err)
	}
	// installedAt records when the installed content changed, not when sync ran.
	// Preserve it when the complete path-to-hash set is unchanged.
	if !exists || !maps.Equal(s.Files, files) {
		s.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	}
	s.Files = files
	if mutate != nil {
		mutate(&s)
	}
	st.Skills[name] = s
	return nil
}

func refreshSymlinks() error {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		rel, err := filepath.Rel(piSkillsDir, skillDest(name))
		if err != nil {
			return err
		}
		link := filepath.Join(piSkillsDir, name)
		if err := do(fmt.Sprintf("symlink %s -> %s", link, rel), func() error {
			status, err := ensureSymlink(link, rel)
			if err != nil {
				return err
			}
			if status != "ok" {
				fmt.Printf("  symlink %s: %s\n", filepath.Base(link), status)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func commitReminder(files ...string) {
	if dryRun {
		return
	}
	fmt.Printf("\nNext step (manual, skl never commits for you):\n  git -C %s add %s && git commit\n", root, strings.Join(files, " "))
}

// ---------- commands ----------

func cmdSync() error {
	lock, err := loadLock()
	if err != nil {
		return err
	}
	st := loadState()
	fmt.Printf("root: %s\n", root)

	names := make([]string, 0, len(lock.Skills))
	for name := range lock.Skills {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ls := lock.Skills[name]
		owner, repo, isGitHub := parseGitHubURL(ls.SourceURL)
		if !isGitHub {
			fmt.Printf("  %s: skipped (non-GitHub source %q is not supported by sync yet)\n", name, ls.SourceURL)
			continue
		}
		ref := resolveRef(st, name, owner, repo)
		if !dirExists(skillDest(name)) {
			if err := do(fmt.Sprintf("install %s from %s/%s@%s (path %s)", name, owner, repo, ref, ls.SkillPath), func() error {
				return installFromGitHub(name, owner, repo, ref, ls.SkillPath)
			}); err != nil {
				return err
			}
		} else {
			fmt.Printf("  %s: present, adopting/refreshing state\n", name)
		}
		if err := do(fmt.Sprintf("apply override for %s", name), func() error {
			_, e := applyOverride(name)
			return e
		}); err != nil {
			return err
		}
		if err := recordState(st, name, func(s *SkillState) {
			s.Origin = "lock"
			s.SourceURL = ls.SourceURL
			s.Owner = owner
			s.Repo = repo
			s.SkillPath = ls.SkillPath
			if s.PinnedRef == "" {
				s.PinnedRef = ref
			}
			s.Override = dirExists(filepath.Join(overridesDir, name))
		}); err != nil {
			return err
		}
	}

	// external (non-lock) skills recorded in state
	for _, name := range sortedStateNames(st) {
		s := st.Skills[name]
		if s.Origin != "external" || dirExists(skillDest(name)) {
			continue
		}
		if s.LocalDir != "" {
			if err := do(fmt.Sprintf("install %s from local dir %s", name, s.LocalDir), func() error {
				if !fileExists(filepath.Join(s.LocalDir, "SKILL.md")) {
					return fmt.Errorf("%s: no SKILL.md in local source %s", name, s.LocalDir)
				}
				if err := removeDir(skillDest(name)); err != nil {
					return err
				}
				return copyDir(s.LocalDir, skillDest(name))
			}); err != nil {
				return err
			}
			if err := recordState(st, name, nil); err != nil {
				return err
			}
			continue
		}
		ref := s.PinnedRef
		if ref == "" {
			ref = resolveLatest(s.Owner, s.Repo)
		}
		if err := do(fmt.Sprintf("install %s from %s/%s@%s (path %s)", name, s.Owner, s.Repo, ref, s.SkillPath), func() error {
			return installFromGitHub(name, s.Owner, s.Repo, ref, s.SkillPath)
		}); err != nil {
			return err
		}
		if err := recordState(st, name, func(s *SkillState) {
			if s.PinnedRef == "" {
				s.PinnedRef = ref
			}
		}); err != nil {
			return err
		}
	}

	if err := refreshSymlinks(); err != nil {
		return err
	}
	if dryRun {
		fmt.Println("\n[dry-run] no changes written")
		return nil
	}
	if err := st.save(); err != nil {
		return err
	}
	fmt.Printf("\nstate written to %s\n", statePath)
	return nil
}

func cmdCheck() error {
	st := loadState()
	lock, lockErr := loadLock()
	problems := 0

	// guard: nothing under skills/ may be git-tracked
	if out, err := exec.Command("git", "-C", root, "ls-files", "skills/").CombinedOutput(); err == nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			fmt.Printf("GUARD ERROR: the following paths under skills/ are git-tracked (must stay untracked):\n  %s\n", strings.Join(strings.Split(trimmed, "\n"), "\n  "))
			problems++
		}
	} else {
		fmt.Printf("warning: %s is not a git repository (guard skipped)\n", root)
	}

	if lockErr != nil {
		fmt.Printf("lock file missing: %v\n", lockErr)
		problems++
	}

	checked := map[string]bool{}
	for _, name := range sortedStateNames(st) {
		s := st.Skills[name]
		checked[name] = true
		dest := skillDest(name)
		if !dirExists(dest) {
			fmt.Printf("MISSING   %s (recorded in state but not on disk — run skl sync)\n", name)
			problems++
			continue
		}
		cur, err := hashFolder(dest)
		if err != nil {
			fmt.Printf("ERROR     %s: %v\n", name, err)
			problems++
			continue
		}
		var modified, added, removed []string
		for p, h := range cur {
			old, ok := s.Files[p]
			if !ok {
				added = append(added, p)
			} else if old != h {
				modified = append(modified, p)
			}
		}
		for p := range s.Files {
			if _, ok := cur[p]; !ok {
				removed = append(removed, p)
			}
		}
		if len(modified)+len(added)+len(removed) == 0 {
			ovr := ""
			if s.Override {
				ovr = " [override applied]"
			}
			pin := shortRef(s.PinnedRef)
			fmt.Printf("OK        %s  (pin %s)%s\n", name, pin, ovr)
			continue
		}
		problems++
		cause := "modified locally (not yet promoted?)"
		if s.Override {
			cause = "changed since override was applied"
		}
		fmt.Printf("DRIFTED   %s  (%s)\n", name, cause)
		printDriftList("modified", modified)
		printDriftList("added", added)
		printDriftList("removed", removed)
	}

	if lock != nil {
		for _, name := range sortedLockNames(lock) {
			if !checked[name] {
				fmt.Printf("UNTRACKED %s  (in lock but not in state — run skl sync)\n", name)
				problems++
			}
		}
	}

	// overrides without a state record
	if e, err := os.ReadDir(overridesDir); err == nil {
		for _, entry := range e {
			if !entry.IsDir() {
				continue
			}
			s, ok := st.Skills[entry.Name()]
			if !ok || !s.Override {
				fmt.Printf("ORPHANED  override %s/ (not promoted in state)\n", entry.Name())
				problems++
			}
		}
	}

	// symlinks
	if e, err := os.ReadDir(skillsDir); err == nil {
		for _, entry := range e {
			if !entry.IsDir() {
				continue
			}
			link := filepath.Join(piSkillsDir, entry.Name())
			if _, err := os.Lstat(link); err != nil {
				fmt.Printf("NOSYMLINK %s -> %s missing (run skl sync)\n", link, skillDest(entry.Name()))
				problems++
			} else if tgt, err := os.Readlink(link); err != nil {
				fmt.Printf("BADLINK   %s is not a symlink (%s)\n", link, tgt)
				problems++
			}
		}
	}

	if problems > 0 {
		fmt.Printf("\n%d problem(s) found\n", problems)
		os.Exit(1)
	}
	fmt.Println("\nall clean")
	return nil
}

func printDriftList(kind string, files []string) {
	if len(files) == 0 {
		return
	}
	sort.Strings(files)
	for _, f := range files {
		fmt.Printf("    %-8s %s\n", kind+":", f)
	}
}

func cmdAdd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: skl add <owner/repo | github-url | /local/dir>")
	}
	arg := args[0]
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return addLocal(arg)
	}
	fmt.Printf("Running: npx skills add %s\n", arg)
	if err := runCmd("npx", "skills", "add", arg); err != nil {
		return err
	}
	return adoptLockSkills("add")
}

// addLocal installs a local directory as an "external" skill (not recorded in the npx lock).
func addLocal(dir string) error {
	name := filepath.Base(filepath.Clean(dir))
	if !fileExists(filepath.Join(dir, "SKILL.md")) {
		return fmt.Errorf("%s: no SKILL.md found at top level", dir)
	}
	if err := do(fmt.Sprintf("install %s from local dir %s", name, dir), func() error {
		if err := removeDir(skillDest(name)); err != nil {
			return err
		}
		return copyDir(dir, skillDest(name))
	}); err != nil {
		return err
	}
	if err := do(fmt.Sprintf("apply override for %s", name), func() error {
		_, e := applyOverride(name)
		return e
	}); err != nil {
		return err
	}
	if err := do(fmt.Sprintf("symlink %s into %s", name, piSkillsDir), func() error {
		rel, err := filepath.Rel(piSkillsDir, skillDest(name))
		if err != nil {
			return err
		}
		_, e := ensureSymlink(filepath.Join(piSkillsDir, name), rel)
		return e
	}); err != nil {
		return err
	}
	st := loadState()
	if err := recordState(st, name, func(s *SkillState) {
		s.Origin = "external"
		s.LocalDir = dir
		s.Override = dirExists(filepath.Join(overridesDir, name))
	}); err != nil {
		return err
	}
	if !dryRun {
		if err := st.save(); err != nil {
			return err
		}
	}
	fmt.Printf("\n%s installed from local directory (recorded in %s, not in the npx lock)\n", name, filepath.Base(statePath))
	commitReminder(".sync-state.json")
	return nil
}

// adoptLockSkills (re)records state for every skill in the lock.
func adoptLockSkills(verb string) error {
	lock, err := loadLock()
	if err != nil {
		return err
	}
	st := loadState()
	for _, name := range sortedLockNames(lock) {
		ls := lock.Skills[name]
		if !dirExists(skillDest(name)) {
			fmt.Printf("  warning: %s is in the lock but not on disk; run `skl sync` to install it\n", name)
			continue
		}
		owner, repo, _ := parseGitHubURL(ls.SourceURL)
		ref := resolveRef(st, name, owner, repo)
		if err := do(fmt.Sprintf("apply override for %s", name), func() error {
			_, e := applyOverride(name)
			return e
		}); err != nil {
			return err
		}
		changed := false
		if _, ok := st.Skills[name]; !ok {
			changed = true
		}
		if err := recordState(st, name, func(s *SkillState) {
			s.Origin = "lock"
			s.SourceURL = ls.SourceURL
			s.Owner = owner
			s.Repo = repo
			s.SkillPath = ls.SkillPath
			if s.PinnedRef == "" {
				s.PinnedRef = ref
			}
			s.Override = dirExists(filepath.Join(overridesDir, name))
		}); err != nil {
			return err
		}
		if changed && !dryRun {
			fmt.Printf("  %s: now tracked in state\n", name)
		}
	}
	if err := refreshSymlinks(); err != nil {
		return err
	}
	if !dryRun {
		if err := st.save(); err != nil {
			return err
		}
	}
	_ = verb
	commitReminder(".skill-lock.json", ".sync-state.json")
	return nil
}

func cmdPromote(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: skl promote <skill>")
	}
	name := args[0]
	st := loadState()
	s, ok := st.Skills[name]
	if !ok {
		return fmt.Errorf("%s: not in state (run `skl sync` first)", name)
	}
	dest := skillDest(name)
	if !dirExists(dest) {
		return fmt.Errorf("%s: not on disk", name)
	}

	if err := do(fmt.Sprintf("save modified skill to %s", filepath.Join(overridesDir, name)), func() error {
		if err := removeDir(filepath.Join(overridesDir, name)); err != nil {
			return err
		}
		return copyDir(dest, filepath.Join(overridesDir, name))
	}); err != nil {
		return err
	}

	// restore pristine upstream, then re-apply the override
	if s.Owner != "" && s.Repo != "" {
		ref := s.PinnedRef
		if ref == "" {
			ref = resolveLatest(s.Owner, s.Repo)
		}
		if err := do(fmt.Sprintf("reinstall pristine %s from %s/%s@%s", name, s.Owner, s.Repo, ref), func() error {
			return installFromGitHub(name, s.Owner, s.Repo, ref, s.SkillPath)
		}); err != nil {
			return err
		}
	} else if s.LocalDir != "" {
		if err := do(fmt.Sprintf("reinstall pristine %s from %s", name, s.LocalDir), func() error {
			if err := removeDir(dest); err != nil {
				return err
			}
			return copyDir(s.LocalDir, dest)
		}); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("%s: no known pristine source", name)
	}

	if err := do(fmt.Sprintf("apply override for %s", name), func() error {
		_, e := applyOverride(name)
		return e
	}); err != nil {
		return err
	}
	if err := recordState(st, name, func(s *SkillState) {
		s.Override = true
	}); err != nil {
		return err
	}
	if !dryRun {
		if err := st.save(); err != nil {
			return err
		}
	}
	fmt.Printf("\n%s promoted: your edits now live in overrides/%s and are applied on top of pristine upstream.\n", name, name)
	commitReminder("overrides/"+name, ".sync-state.json")
	return nil
}

func cmdUpdate(args []string) error {
	st := loadState()
	lock, lockErr := loadLock()
	var lockNames, extGitHub []string
	for _, name := range sortedStateNames(st) {
		s := st.Skills[name]
		if len(args) > 0 && !contains(args, name) {
			continue
		}
		if s.Origin == "lock" {
			lockNames = append(lockNames, name)
		} else if s.Owner != "" && s.Repo != "" {
			extGitHub = append(extGitHub, name)
		} else if s.LocalDir != "" {
			fmt.Printf("  %s: local source, skipped\n", name)
		}
	}

	if lockErr == nil && len(lockNames) > 0 {
		args := []string{"skills", "update", "-y", "-g"}
		args = append(args, lockNames...)
		if err := runCmd("npx", args...); err != nil {
			return err
		}
		for _, name := range lockNames {
			ls := lock.Skills[name]
			owner, repo, _ := parseGitHubURL(ls.SourceURL)
			ref := resolveLatest(owner, repo)
			if err := do(fmt.Sprintf("apply override for %s", name), func() error {
				_, e := applyOverride(name)
				return e
			}); err != nil {
				return err
			}
			if err := recordState(st, name, func(s *SkillState) {
				s.PinnedRef = ref
			}); err != nil {
				return err
			}
		}
	}

	for _, name := range extGitHub {
		s := st.Skills[name]
		ref := resolveLatest(s.Owner, s.Repo)
		if err := do(fmt.Sprintf("update %s to %s/%s@%s", name, s.Owner, s.Repo, ref), func() error {
			return installFromGitHub(name, s.Owner, s.Repo, ref, s.SkillPath)
		}); err != nil {
			return err
		}
		if err := do(fmt.Sprintf("apply override for %s", name), func() error {
			_, e := applyOverride(name)
			return e
		}); err != nil {
			return err
		}
		if err := recordState(st, name, func(s *SkillState) {
			s.PinnedRef = ref
		}); err != nil {
			return err
		}
	}

	if !dryRun {
		if err := st.save(); err != nil {
			return err
		}
	}
	commitReminder(".sync-state.json")
	return nil
}

// ---------- small utils ----------

func sortedLockNames(l *Lock) []string {
	names := make([]string, 0, len(l.Skills))
	for n := range l.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedStateNames(s *State) []string {
	names := make([]string, 0, len(s.Skills))
	for n := range s.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

func usage() {
	fmt.Print(`skl — skill manager for ~/.agents

Usage:
  skl add <owner/repo | github-url | /local/dir>   install via ` + "`npx skills add`" + ` (or a local dir), then track it
  skl sync                                         install missing skills, apply overrides, fix pi symlinks, refresh state
  skl check                                        verify: drift, missing skills, orphaned overrides, git guard, symlinks
  skl update [skills...]                           update via ` + "`npx skills update`" + ` (or re-fetch external), re-pin, re-hash
  skl promote <skill>                              move local edits to overrides/, restore pristine, re-apply

Global flag:
  -n, --dry-run        show what would be done without changing anything

Files:
  .skill-lock.json     owned by ` + "`npx skills`" + ` (skl reads it, never writes it)
  .sync-state.json     owned by skl (commit pins, file hashes, override flags)
  overrides/<skill>/   your promoted modifications, applied on top of pristine upstream
  skills/<skill>/      disposable cache: pristine upstream + overrides (git-ignored)
`)
}

func main() {
	initPaths()
	args := os.Args[1:]
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-n" || a == "--dry-run" {
			dryRun = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := rest[0], rest[1:]
	var err error
	switch cmd {
	case "sync":
		err = cmdSync()
	case "check":
		err = cmdCheck()
	case "add":
		err = cmdAdd(rest)
	case "update":
		err = cmdUpdate(rest)
	case "promote":
		err = cmdPromote(rest)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
