package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

var stageRootOverride string

type commonFlags struct {
	force, global bool
	project       string
	cache         string
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
		case "--cache":
			if i+1 == len(args) {
				return f, fmt.Errorf("--cache requires a directory")
			}
			i++
			f.cache = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return f, fmt.Errorf("unknown option %s", args[i])
			}
			f.args = append(f.args, args[i])
		}
	}
	return f, nil
}

func runExternal(name string, args ...string) error {
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

func sourceToStageImpl(s SkillState, ref string) (string, func(), error) {
	if dryRun {
		return "", func() {}, nil
	}
	base := cfg.root
	if stageRootOverride != "" {
		base = stageRootOverride
	}
	if base == cfg.root {
		root, err := openRootedFS(cfg.root)
		if err != nil {
			return "", func() {}, fmt.Errorf("pin root for stage: %w", err)
		}
		name := fmt.Sprintf(".skl-stage-%d-%d", time.Now().UnixNano(), os.Getpid())
		if err := root.MkdirAll(name, 0755); err != nil {
			root.Close()
			return "", func() {}, err
		}
		root.Close()
		stage := filepath.Join(cfg.root, name)
		cleanup := func() {
			if r, err := openRootedFS(cfg.root); err == nil {
				_ = r.RemoveAll(name)
				r.Close()
			}
		}
		payload := filepath.Join(stage, "payload")
		if s.LocalDir != "" {
			if !fileExists(filepath.Join(s.LocalDir, "SKILL.md")) {
				cleanup()
				return "", func() {}, fmt.Errorf("local source %s has no SKILL.md", s.LocalDir)
			}
			err = copyDir(s.LocalDir, payload)
		} else {
			var top string
			top, err = downloadTarballFunc(s.Owner, s.Repo, ref, stage)
			if err == nil {
				folder := filepath.Clean(filepath.FromSlash(folderOfSkillPath(s.SkillPath)))
				if folder == "." || folder == ".." || filepath.IsAbs(folder) || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
					err = fmt.Errorf("unsafe skill path %q", s.SkillPath)
				}
				src := filepath.Join(top, folder)
				if err == nil {
					if !fileExists(filepath.Join(src, "SKILL.md")) {
						err = fmt.Errorf("skill path %q not found in %s/%s@%s", s.SkillPath, s.Owner, s.Repo, ref)
					} else {
						err = copyDir(src, payload)
					}
				}
			}
		}
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		return payload, cleanup, nil
	}
	stage, err := os.MkdirTemp(base, ".skl-stage-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		_ = os.RemoveAll(stage)
	}
	payload := filepath.Join(stage, "payload")
	if s.LocalDir != "" {
		if !fileExists(filepath.Join(s.LocalDir, "SKILL.md")) {
			cleanup()
			return "", func() {}, fmt.Errorf("local source %s has no SKILL.md", s.LocalDir)
		}
		err = copyDir(s.LocalDir, payload)
	} else {
		var top string
		top, err = downloadTarballFunc(s.Owner, s.Repo, ref, stage)
		if err == nil {
			folder := filepath.Clean(filepath.FromSlash(folderOfSkillPath(s.SkillPath)))
			if folder == "." || folder == ".." || filepath.IsAbs(folder) || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
				err = fmt.Errorf("unsafe skill path %q", s.SkillPath)
			}
			src := filepath.Join(top, folder)
			if err == nil {
				if !fileExists(filepath.Join(src, "SKILL.md")) {
					err = fmt.Errorf("skill path %q not found in %s/%s@%s", s.SkillPath, s.Owner, s.Repo, ref)
				} else {
					err = copyDir(src, payload)
				}
			}
		}
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return payload, cleanup, nil
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
func cmdAdd(args []string) (retErr error) {
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
		if err := validateCanonicalNamespace(s.Namespace); err != nil {
			return fmt.Errorf("add %s: %w", id, err)
		}
		dest := canonicalPath(s)
		if _, statErr := os.Lstat(dest); statErr == nil && !f.force {
			return fmt.Errorf("%s already exists; use --force to replace it", id)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if prior, ok := st.Skills[id]; ok {
			s.Global = prior.Global
		}
		if f.global {
			s.Global = true
		}
		next, e := cloneState(st)
		if e != nil {
			return e
		}
		item := stagedLibrarySkill{ID: id, State: s}
		if dryRun {
			next.Skills[id] = s
			return commitLibraryTransaction(st, next, []stagedLibrarySkill{item})
		}
		payload, cleanup, e := sourceToStage(s, "")
		if e != nil {
			return e
		}
		defer cleanup()
		item.Payload = payload
		if err := recordStaged(next, st, item); err != nil {
			return err
		}
		return commitLibraryTransaction(st, next, []stagedLibrarySkill{item})
	}
	// npx writes temporary payloads below the global skills path, so reject an
	// unsafe root before invoking it rather than relying only on commit preflight.
	if err := validateGlobalRoot(); err != nil {
		return err
	}
	// Pin the npx output directory before the subprocess runs. A replacement
	// with a real directory is external data: cleanup must fail rather than
	// interpreting its children as npx output.
	var npxGlobal *rootedFS
	if !dryRun {
		if err := ensureGlobalRoot(); err != nil {
			return err
		}
		var pinErr error
		npxGlobal, pinErr = openRootedFS(cfg.global)
		if pinErr != nil {
			return fmt.Errorf("pin npx global root: %w", pinErr)
		}
		defer npxGlobal.Close()
	}
	beforeLock := &NpxLock{Skills: map[string]LockSkill{}}
	if old, lockErr := loadNpxLock(); lockErr == nil {
		beforeLock = old
	} else if !os.IsNotExist(lockErr) {
		return lockErr
	}
	beforeGlobal := map[string]bool{}
	beforeOutput := map[string]string{}
	initialLinks := map[string]string{}
	initialEnabled := map[string]SkillState{}
	for _, s := range st.Skills {
		if s.Global {
			initialEnabled[s.Name] = s
		}
	}
	addSucceeded := false
	if !dryRun {
		entries, readErr := npxGlobal.ReadDir(".")
		if readErr != nil {
			return fmt.Errorf("snapshot npx global output: %w", readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			beforeGlobal[name] = true
			beforeOutput[name] = rootedPathSignature(npxGlobal, name)
			if s, ok := initialEnabled[name]; ok {
				if target, err := npxGlobal.Readlink(name); err == nil && rootedLinkPointsTo(npxGlobal, target, canonicalPath(s)) {
					initialLinks[name] = target
				}
			}
		}
	}
	// Install the restoration guard before npx runs: npx may leave output even
	// when it exits nonzero. Cleanup errors are joined with the command error so
	// callers are never told a failed invocation was safely restored when it was not.
	if !dryRun {
		defer func() {
			// A real-directory replacement belongs to somebody else and is never
			// inspected or modified. A symlink node itself can be unlinked, then a
			// new real root is pinned before restoration resumes.
			if pinErr := npxGlobal.check(); pinErr != nil {
				info, statErr := os.Lstat(cfg.global)
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					retErr = errors.Join(retErr, fmt.Errorf("restore npx global output: %w", pinErr))
					return
				}
				_ = npxGlobal.Close()
				if err := recoverGlobalRoot(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("restore npx global output: %w", err))
					return
				}
				var openErr error
				npxGlobal, openErr = openRootedFS(cfg.global)
				if openErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("repin recovered npx global root: %w", openErr))
					return
				}
			}
			desired := map[string]string{}
			if addSucceeded {
				for _, s := range st.Skills {
					if s.Global {
						target, err := filepath.Rel(cfg.global, canonicalPath(s))
						if err != nil {
							retErr = errors.Join(retErr, err)
							continue
						}
						desired[s.Name] = target
					}
				}
			} else {
				for name, target := range initialLinks {
					desired[name] = target
				}
			}
			entries, err := npxGlobal.ReadDir(".")
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("inspect npx output: %w", err))
			} else {
				for _, entry := range entries {
					name := entry.Name()
					if beforeGlobal[name] {
						continue
					}
					if target, keep := desired[name]; keep {
						if current, err := npxGlobal.Readlink(name); err == nil && current == target {
							continue
						}
					}
					if err := npxGlobal.RemoveAll(name); err != nil {
						retErr = errors.Join(retErr, fmt.Errorf("remove npx output %s: %w", name, err))
					}
				}
			}
			for name, target := range desired {
				if current, err := npxGlobal.Readlink(name); err == nil && current == target {
					continue
				}
				if err := npxGlobal.RemoveAll(name); err != nil && !os.IsNotExist(err) {
					retErr = errors.Join(retErr, fmt.Errorf("restore global link %s: %w", name, err))
					continue
				}
				if err := npxGlobal.Symlink(target, name); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("restore global link %s: %w", name, err))
				}
			}
		}()
	}

	if err := runCommand("npx", "skills", "add", source); err != nil {
		return err
	}
	if !dryRun {
		if err := npxGlobal.check(); err != nil {
			return fmt.Errorf("npx global root changed: %w", err)
		}
	}
	lock, err := loadNpxLock()
	if err != nil {
		return fmt.Errorf("read npx lock after add: %w", err)
	}
	candidates := map[string]LockSkill{}
	for name, ls := range lock.Skills {
		old, existed := beforeLock.Skills[name]
		if !existed || !reflect.DeepEqual(old, ls) {
			candidates[name] = ls
		}
	}
	if f.force && !dryRun { // unchanged lock entries count only when npx selected/output them
		entries, e := npxGlobal.ReadDir(".")
		if e != nil {
			return fmt.Errorf("inspect npx output: %w", e)
		}
		for _, entry := range entries {
			name := entry.Name()
			if beforeOutput[name] != rootedPathSignature(npxGlobal, name) {
				if ls, ok := lock.Skills[name]; ok {
					candidates[name] = ls
				}
			}
		}
	}
	if len(candidates) == 0 {
		if dryRun {
			return nil
		}
		return fmt.Errorf("npx lock did not add or change any skills")
	}
	cache := map[string]string{}
	next, err := cloneState(st)
	if err != nil {
		return err
	}
	var staged []stagedLibrarySkill
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	candidateNames := make([]string, 0, len(candidates))
	for name := range candidates {
		candidateNames = append(candidateNames, name)
	}
	sort.Strings(candidateNames)
	for _, name := range candidateNames {
		if err := validatePart("skill name", name); err != nil {
			return fmt.Errorf("npx lock: %w", err)
		}
		ls := candidates[name]
		owner, repo, ok := parseGitHubURL(ls.SourceURL)
		if !ok {
			return fmt.Errorf("npx skill %s has unsupported source %q", name, ls.SourceURL)
		}
		id := canonicalID(owner, name)
		if err := validateCanonicalNamespace(owner); err != nil {
			return fmt.Errorf("add %s: %w", id, err)
		}
		_, exists := st.Skills[id]
		if exists && !f.force {
			return fmt.Errorf("%s already exists; use --force to replace it", id)
		}
		ref, e := resolveLatestCached(owner, repo, cache)
		if e != nil {
			return e
		}
		s := SkillState{Name: name, Namespace: owner, Path: canonicalRel(owner, name), Origin: "npx-lock", SourceURL: ls.SourceURL, Owner: owner, Repo: repo, SkillPath: ls.SkillPath, PinnedRef: ref}
		if _, statErr := os.Lstat(canonicalPath(s)); statErr == nil && !f.force {
			return fmt.Errorf("%s destination already exists; use --force", id)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if prior, ok := st.Skills[id]; ok {
			s.Global = prior.Global
		}
		if f.global {
			s.Global = true
		}
		if dryRun {
			staged = append(staged, stagedLibrarySkill{ID: id, State: s})
			continue
		}
		// Fetch every candidate before any canonical payload is replaced.
		payload, cleanup, e := sourceToStage(s, ref)
		if e != nil {
			return e
		}
		cleanups = append(cleanups, cleanup)
		item := stagedLibrarySkill{ID: id, State: s, Payload: payload}
		if err := recordStaged(next, st, item); err != nil {
			return err
		}
		staged = append(staged, item)
	}
	if !dryRun {
		for _, name := range candidateNames {
			if !beforeGlobal[name] {
				if err := npxGlobal.RemoveAll(name); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove npx output %s: %w", name, err)
				}
			}
		}
	}
	err = commitLibraryTransaction(st, next, staged)
	if err == nil {
		st = next
		addSucceeded = true
	}
	return err
}

func cmdSync(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(f.args) > 0 {
		return fmt.Errorf("usage: skl sync [--force]")
	}
	if f.cache != "" {
		if info, err := os.Stat(f.cache); err != nil || !info.IsDir() {
			return fmt.Errorf("--cache directory %q does not exist or is not a directory", f.cache)
		}
		stageRootOverride = f.cache
	} else {
		stageRootOverride = ""
	}
	defer func() { stageRootOverride = "" }()
	st, migrated, err := loadState()
	if err != nil {
		return err
	}
	if migrated {
		return fmt.Errorf("legacy layout detected; run `skl migrate` first")
	}
	next, err := cloneState(st)
	if err != nil {
		return err
	}
	var staged []stagedLibrarySkill
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		if err := validateCanonicalNamespace(s.Namespace); err != nil {
			return fmt.Errorf("sync %s: %w", id, err)
		}
		dest := canonicalPath(s)
		if info, statErr := os.Lstat(dest); statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				if !f.force {
					return fmt.Errorf("%s destination is not a real directory; use --force", id)
				}
			} else {
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
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		ref := s.PinnedRef
		if s.LocalDir == "" && ref == "" {
			return fmt.Errorf("%s has no pinned revision", id)
		}
		item := stagedLibrarySkill{ID: id, State: s}
		if !dryRun {
			payload, cleanup, err := sourceToStage(s, ref)
			if err != nil {
				return err
			}
			cleanups = append(cleanups, cleanup)
			files, err := hashFolder(payload)
			if err != nil {
				return err
			}
			if !equalHashes(files, s.Files) {
				return fmt.Errorf("recorded source for %s does not reproduce its locked hashes", id)
			}
			item.Payload = payload
		}
		staged = append(staged, item)
	}
	if len(staged) == 0 {
		return nil
	}
	return commitLibraryTransaction(st, next, staged)
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
	cache := map[string]string{}
	next, err := cloneState(st)
	if err != nil {
		return err
	}
	var staged []stagedLibrarySkill
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	for _, id := range ids {
		s := st.Skills[id]
		if err := validateCanonicalNamespace(s.Namespace); err != nil {
			return fmt.Errorf("update %s: %w", id, err)
		}
		dest := canonicalPath(s)
		if info, statErr := os.Lstat(dest); statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				if !f.force {
					return fmt.Errorf("%s destination is not a real directory; use --force", id)
				}
			} else {
				drift, e := hasDrift(s, dest)
				if e != nil {
					return e
				}
				if drift && !f.force {
					return fmt.Errorf("%s has library drift; refusing update (use --force)", id)
				}
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		ref := ""
		if s.LocalDir == "" {
			ref, err = resolveLatestCached(s.Owner, s.Repo, cache)
			if err != nil {
				return err
			}
		}
		if ref != "" {
			s.PinnedRef = ref
		}
		if dryRun {
			staged = append(staged, stagedLibrarySkill{ID: id, State: s})
			continue
		}
		payload, cleanup, e := sourceToStage(s, ref)
		if e != nil {
			return e
		}
		cleanups = append(cleanups, cleanup)
		item := stagedLibrarySkill{ID: id, State: s, Payload: payload}
		if err := recordStaged(next, st, item); err != nil {
			return err
		}
		staged = append(staged, item)
	}
	return commitLibraryTransaction(st, next, staged)
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
		if s.LocalDir == "" && !isCommitSHA(s.PinnedRef) {
			fmt.Printf("SYMBOLIC %s has non-immutable pin %q (run skl migrate)\n", id, s.PinnedRef)
			problems++
		}
		info, statErr := os.Lstat(dest)
		if os.IsNotExist(statErr) {
			fmt.Printf("MISSING  %s\n", id)
			problems++
			continue
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			fmt.Printf("INVALID  %s canonical destination is not a real directory\n", id)
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
		if len(args) == 1 {
			return fmt.Errorf("%s requires at least one skill", args[0])
		}
		ids, e := resolveSelectors(st, args[1:])
		if e != nil {
			return e
		}
		next, e := cloneState(st)
		if e != nil {
			return e
		}
		requestedNames := make([]string, 0, len(ids))
		for _, id := range ids {
			s := next.Skills[id]
			s.Global = args[0] == "enable"
			next.Skills[id] = s
			requestedNames = append(requestedNames, s.Name)
		}
		return commitLibraryTransaction(st, next, nil, requestedNames...)
	default:
		return fmt.Errorf("unknown global command %q", args[0])
	}
}

func fileIdentity(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return ""
	}
	var parts []string
	for _, name := range []string{"Dev", "Ino"} {
		field := value.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			parts = append(parts, fmt.Sprint(field.Interface()))
		}
	}
	return strings.Join(parts, ":")
}

// rootedLinkPointsTo resolves a link target lexically from a pinned root; it
// deliberately does not follow the link through the mutable global pathname.
func rootedLinkPointsTo(root *rootedFS, target, dest string) bool {
	if filepath.IsAbs(target) {
		return filepath.Clean(target) == filepath.Clean(dest)
	}
	return filepath.Clean(filepath.Join(root.path, target)) == filepath.Clean(dest)
}

// rootedPathSignature is used only for npx output selection. All child reads
// are descriptor-relative so a replaced global root cannot influence it.
func rootedPathSignature(root *rootedFS, name string) string {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "error:" + err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		if err != nil {
			return "error:" + err.Error()
		}
		return "link:" + target
	}
	if !info.IsDir() {
		return fmt.Sprintf("mode:%s:size:%d:mtime:%d:identity:%s", info.Mode(), info.Size(), info.ModTime().UnixNano(), fileIdentity(info))
	}
	files, err := hashRootFolder(root, name)
	if err != nil {
		return "error:" + err.Error()
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "dir:%s:%d:%d:", fileIdentity(info), info.ModTime().UnixNano(), len(files))
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(files[k])
	}
	return b.String()
}

func pathSignature(path string) string {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "error:" + err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return "link:" + target
	}
	if !info.IsDir() {
		return fmt.Sprintf("mode:%s:size:%d:mtime:%d:identity:%s", info.Mode(), info.Size(), info.ModTime().UnixNano(), fileIdentity(info))
	}
	var metadata strings.Builder
	if err := filepath.WalkDir(path, func(entryPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, entryPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(&metadata, "%s:%s:%d:%d:%s\x00", filepath.ToSlash(rel), entryInfo.Mode(), entryInfo.Size(), entryInfo.ModTime().UnixNano(), fileIdentity(entryInfo))
		return nil
	}); err != nil {
		return "error:" + err.Error()
	}
	files, err := hashFolder(path)
	if err != nil {
		return "error:" + err.Error()
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metadata.String())
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(files[k])
	}
	return b.String()
}

func parseGitHubSource(source string) (string, string, bool) {
	if owner, repo, ok := parseGitHubURL(source); ok {
		return owner, repo, true
	}
	parts := strings.Split(strings.Trim(source, "/"), "/")
	if len(parts) == 2 {
		repo := strings.TrimSuffix(parts[1], ".git")
		if validatePart("owner", parts[0]) == nil && validatePart("repo", repo) == nil {
			return parts[0], repo, true
		}
	}
	return "", "", false
}

var runCommand = runExternal
var sourceToStage = sourceToStageImpl
var latestCommitFunc = latestCommit
var downloadTarballFunc = downloadTarball

func resolveLatestCached(owner, repo string, cache map[string]string) (string, error) {
	key := owner + "/" + repo
	if ref := cache[key]; ref != "" {
		return ref, nil
	}
	if dryRun {
		return strings.Repeat("0", 40), nil
	}
	sha, err := latestCommitFunc(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve immutable revision for %s: %w", key, err)
	}
	if !isCommitSHA(sha) {
		return "", fmt.Errorf("resolved non-immutable revision %q for %s", sha, key)
	}
	cache[key] = strings.ToLower(sha)
	return cache[key], nil
}

func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}
