package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type commonFlags struct {
	force, global bool
	project       string
	args          []string
}

func parseFlags(args []string) (commonFlags, error) {
	var f commonFlags
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			f.force = true
		case "--global":
			f.global = true
		case "--project":
			if i+1 == len(args) {
				return f, fmt.Errorf("--project requires a directory")
			}
			i++
			f.project = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, fmt.Errorf("unknown option %s", args[i])
			}
			f.args = append(f.args, args[i])
		}
	}
	return f, nil
}

func run(name string, args ...string) error {
	if dryRun {
		fmt.Printf("  [dry-run] exec %s %s\n", name, strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(name, args...)
	home, _ := os.UserHomeDir()
	cmd.Dir = home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func sourceToStage(s SkillState, ref string) (string, func(), error) {
	if dryRun {
		return "", func() {}, nil
	}
	stage, err := os.MkdirTemp(cfg.root, ".skl-stage-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	payload := filepath.Join(stage, "payload")
	if s.LocalDir != "" {
		if !fileExists(filepath.Join(s.LocalDir, "SKILL.md")) {
			cleanup()
			return "", func() {}, fmt.Errorf("local source %s has no SKILL.md", s.LocalDir)
		}
		err = copyDir(s.LocalDir, payload)
	} else {
		var top string
		top, err = downloadTarball(s.Owner, s.Repo, ref, stage)
		if err == nil {
			src := filepath.Join(top, filepath.FromSlash(folderOfSkillPath(s.SkillPath)))
			if !fileExists(filepath.Join(src, "SKILL.md")) {
				err = fmt.Errorf("skill path %q not found in %s/%s@%s", s.SkillPath, s.Owner, s.Repo, ref)
			} else {
				err = copyDir(src, payload)
			}
		}
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return payload, cleanup, nil
}

func installStaged(payload, dest string) error {
	backup := dest + ".skl-backup"
	_ = os.RemoveAll(backup)
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Rename(dest, backup); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	if err := os.Rename(payload, dest); err != nil {
		_ = os.Rename(backup, dest)
		return err
	}
	return os.RemoveAll(backup)
}

func replaceFromSource(s SkillState, ref string) error {
	dest := canonicalPath(s)
	if dryRun {
		fmt.Printf("  [dry-run] install %s at %s\n", canonicalID(s.Namespace, s.Name), dest)
		return nil
	}
	if err := os.MkdirAll(cfg.root, 0755); err != nil {
		return err
	}
	payload, cleanup, err := sourceToStage(s, ref)
	if err != nil {
		return err
	}
	defer cleanup()
	return installStaged(payload, dest)
}

// restoreFromSource verifies the staged source before replacing anything. Sync
// reproduces recorded state; it never silently turns changed source into a new lock.
func restoreFromSource(s SkillState, ref string, expected map[string]string) error {
	if dryRun {
		fmt.Printf("  [dry-run] restore %s from recorded source\n", canonicalID(s.Namespace, s.Name))
		return nil
	}
	payload, cleanup, err := sourceToStage(s, ref)
	if err != nil {
		return err
	}
	defer cleanup()
	files, err := hashFolder(payload)
	if err != nil {
		return err
	}
	if !equalHashes(files, expected) {
		return fmt.Errorf("recorded source for %s does not reproduce its locked hashes", canonicalID(s.Namespace, s.Name))
	}
	return installStaged(payload, canonicalPath(s))
}

func hasDrift(s SkillState, path string) (bool, error) {
	cur, err := hashFolder(path)
	if err != nil {
		return false, err
	}
	return !equalHashes(cur, s.Files), nil
}
func equalHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func recordInstalled(st *State, id string, s SkillState) error {
	if dryRun {
		return nil
	}
	files, err := hashFolder(canonicalPath(s))
	if err != nil {
		return err
	}
	if old, ok := st.Skills[id]; !ok || !equalHashes(old.Files, files) {
		s.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	} else {
		s.InstalledAt = old.InstalledAt
	}
	s.Files = files
	st.Skills[id] = s
	return nil
}

func cmdAdd(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(f.args) != 1 {
		return fmt.Errorf("usage: skl add [--global] [--force] <source>")
	}
	st, _, err := loadState()
	if err != nil {
		return err
	}
	source := f.args[0]
	if info, e := os.Stat(source); e == nil && info.IsDir() {
		absoluteSource, e := filepath.Abs(source)
		if e != nil {
			return e
		}
		source = absoluteSource
		name := filepath.Base(filepath.Clean(source))
		if err := validatePart("skill name", name); err != nil {
			return err
		}
		s := SkillState{Name: name, Namespace: "local", Path: canonicalRel("local", name), Origin: "local", LocalDir: source}
		id := canonicalID("local", name)
		if old, ok := st.Skills[id]; ok && dirExists(canonicalPath(old)) && !f.force {
			return fmt.Errorf("%s already exists; use --force to replace it", id)
		}
		if err := replaceFromSource(s, ""); err != nil {
			return err
		}
		if err := recordInstalled(st, id, s); err != nil {
			return err
		}
		if f.global {
			if err := enableGlobal(st, id); err != nil {
				return err
			}
		}
		return saveState(st)
	}
	before := map[string]bool{}
	for id := range st.Skills {
		before[id] = true
	}
	beforeGlobal := map[string]bool{}
	if entries, readErr := os.ReadDir(cfg.global); readErr == nil {
		for _, entry := range entries {
			beforeGlobal[entry.Name()] = true
		}
	}
	// npx remains sole owner of .skill-lock.json. Any output it puts in skills/
	// is immediately adopted or removed and is never implicit global exposure.
	if err := run("npx", "skills", "add", source); err != nil {
		return err
	}
	lock, err := loadNpxLock()
	if err != nil {
		return fmt.Errorf("read npx lock after add: %w", err)
	}
	added := 0
	for name, ls := range lock.Skills {
		owner, repo, ok := parseGitHubURL(ls.SourceURL)
		if !ok {
			continue
		}
		id := canonicalID(owner, name)
		tmpOutput := filepath.Join(cfg.global, name)
		generatedOutput := !beforeGlobal[name]
		if before[id] {
			if generatedOutput && !dryRun {
				if err := os.RemoveAll(tmpOutput); err != nil {
					return err
				}
			}
			continue
		}
		ref := resolveLatest(owner, repo)
		s := SkillState{Name: name, Namespace: owner, Path: canonicalRel(owner, name), Origin: "npx-lock", SourceURL: ls.SourceURL, Owner: owner, Repo: repo, SkillPath: ls.SkillPath, PinnedRef: ref}
		info, statErr := os.Lstat(tmpOutput)
		if statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && !dryRun {
			if err := os.MkdirAll(filepath.Dir(canonicalPath(s)), 0755); err != nil {
				return err
			}
			if err := os.Rename(tmpOutput, canonicalPath(s)); err != nil {
				return err
			}
		} else {
			if generatedOutput && statErr == nil && !dryRun {
				if err := os.RemoveAll(tmpOutput); err != nil {
					return err
				}
			}
			if err := replaceFromSource(s, ref); err != nil {
				return err
			}
		}
		if err := recordInstalled(st, id, s); err != nil {
			return err
		}
		if f.global {
			if err := enableGlobal(st, id); err != nil {
				return err
			}
		}
		added++
	}
	if added == 0 && !dryRun {
		return fmt.Errorf("npx did not add any new GitHub skills")
	}
	return saveState(st)
}

func cmdSync(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(f.args) > 0 {
		return fmt.Errorf("usage: skl sync [--force]")
	}
	st, migrated, err := loadState()
	if err != nil {
		return err
	}
	if migrated {
		return fmt.Errorf("legacy layout detected; run `skl migrate` first")
	}
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		dest := canonicalPath(s)
		if dirExists(dest) {
			drift, e := hasDrift(s, dest)
			if e != nil {
				return e
			}
			if drift && !f.force {
				return fmt.Errorf("%s has library drift; refusing to overwrite (use --force)", id)
			}
			if !drift {
				continue
			}
		}
		ref := s.PinnedRef
		if s.LocalDir == "" && ref == "" {
			return fmt.Errorf("%s has no pinned revision", id)
		}
		if err := restoreFromSource(s, ref, s.Files); err != nil {
			return err
		}
	}
	// Sync reproduces state and therefore has no state changes to save.
	return nil
}

func cmdUpdate(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	st, m, err := loadState()
	if err != nil {
		return err
	}
	if m {
		return fmt.Errorf("run `skl migrate` first")
	}
	ids, err := resolveSelectors(st, f.args)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s := st.Skills[id]
		dest := canonicalPath(s)
		if dirExists(dest) {
			drift, e := hasDrift(s, dest)
			if e != nil {
				return e
			}
			if drift && !f.force {
				return fmt.Errorf("%s has library drift; refusing update (use --force)", id)
			}
		}
		ref := ""
		if s.LocalDir == "" {
			ref = resolveLatest(s.Owner, s.Repo)
			if ref == "" {
				return fmt.Errorf("%s has no fetchable source", id)
			}
		}
		if err := replaceFromSource(s, ref); err != nil {
			return err
		}
		if ref != "" {
			s.PinnedRef = ref
		}
		if err := recordInstalled(st, id, s); err != nil {
			return err
		}
	}
	return saveState(st)
}

func cmdCheck() error {
	st, m, err := loadState()
	if err != nil {
		return err
	}
	if m {
		return fmt.Errorf("legacy layout detected; run `skl migrate`")
	}
	problems := 0
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		dest := canonicalPath(s)
		if !dirExists(dest) {
			fmt.Printf("MISSING  %s\n", id)
			problems++
			continue
		}
		drift, e := hasDrift(s, dest)
		if e != nil {
			return e
		}
		if drift {
			fmt.Printf("DRIFTED %s\n", id)
			problems++
		} else {
			fmt.Printf("OK      %s @ %s\n", id, shortRef(s.PinnedRef))
		}
		link := filepath.Join(cfg.global, s.Name)
		_, le := os.Lstat(link)
		if s.Global && (le != nil || !linkPointsTo(link, dest)) {
			fmt.Printf("BADGLOBAL %s\n", id)
			problems++
		}
	}
	if entries, e := os.ReadDir(cfg.global); e == nil {
		for _, entry := range entries {
			found := false
			for _, s := range st.Skills {
				if s.Global && s.Name == entry.Name() {
					found = true
				}
			}
			if !found {
				fmt.Printf("UNMANAGED-GLOBAL skills/%s\n", entry.Name())
				problems++
			}
		}
	}
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Println("all clean")
	return nil
}

func cmdList() error {
	st, _, err := loadState()
	if err != nil {
		return err
	}
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		g := ""
		if s.Global {
			g = " global"
		}
		fmt.Printf("%-40s %s%s\n", id, shortRef(s.PinnedRef), g)
	}
	return nil
}

func enableGlobal(st *State, id string) error {
	s := st.Skills[id]
	for oid, other := range st.Skills {
		if oid != id && other.Name == s.Name && other.Global {
			return fmt.Errorf("global name %q already exposes %s", s.Name, oid)
		}
	}
	link := filepath.Join(cfg.global, s.Name)
	rel, err := filepath.Rel(cfg.global, canonicalPath(s))
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("  [dry-run] link %s -> %s\n", link, rel)
	} else {
		status, e := ensureSymlink(link, rel)
		if e != nil {
			return e
		}
		if status == "conflict (exists as real directory)" {
			return fmt.Errorf("%s exists and is not a managed symlink", link)
		}
	}
	s.Global = true
	st.Skills[id] = s
	return nil
}
func disableGlobal(st *State, id string) error {
	s := st.Skills[id]
	link := filepath.Join(cfg.global, s.Name)
	if _, err := os.Lstat(link); err == nil {
		if !linkPointsTo(link, canonicalPath(s)) {
			return fmt.Errorf("refusing to remove unmanaged global path %s", link)
		}
		if dryRun {
			fmt.Printf("  [dry-run] remove %s\n", link)
		} else if err := os.Remove(link); err != nil {
			return err
		}
	}
	s.Global = false
	st.Skills[id] = s
	return nil
}
func linkPointsTo(link, dest string) bool {
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	a, _ := filepath.Abs(target)
	b, _ := filepath.Abs(dest)
	return filepath.Clean(a) == filepath.Clean(b)
}
func cmdGlobal(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: skl global list|enable|disable [skills...]")
	}
	st, m, err := loadState()
	if err != nil {
		return err
	}
	if m {
		return fmt.Errorf("run `skl migrate` first")
	}
	switch args[0] {
	case "list":
		for _, id := range sortedIDs(st) {
			if st.Skills[id].Global {
				fmt.Println(id)
			}
		}
		return nil
	case "enable", "disable":
		ids, e := resolveSelectors(st, args[1:])
		if e != nil {
			return e
		}
		if len(args) == 1 {
			return fmt.Errorf("%s requires at least one skill", args[0])
		}
		for _, id := range ids {
			if args[0] == "enable" {
				e = enableGlobal(st, id)
			} else {
				e = disableGlobal(st, id)
			}
			if e != nil {
				return e
			}
		}
		return saveState(st)
	default:
		return fmt.Errorf("unknown global command %q", args[0])
	}
}

func resolveLatest(owner, repo string) string {
	if dryRun {
		return "HEAD"
	}
	sha, err := latestCommit(owner, repo)
	if err != nil {
		fmt.Printf("warning: cannot resolve %s/%s: %v; using HEAD\n", owner, repo, err)
		return "HEAD"
	}
	return sha
}
func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

var _ = errors.Is
var _ = sort.Strings
