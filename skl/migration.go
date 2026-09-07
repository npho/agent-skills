package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdMigrate(args []string) error {
	piDir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--pi-skills" && i+1 < len(args) {
			i++
			piDir = args[i]
		} else {
			return fmt.Errorf("usage: skl migrate [--pi-skills DIR]")
		}
	}
	if piDir == "" {
		home, _ := os.UserHomeDir()
		piDir = filepath.Join(home, ".pi", "agent", "skills")
	}
	// Migration reads, moves, and removes children of the legacy global root.
	// Reject a redirected root before loading or mutating any migration data.
	if err := validateGlobalRoot(); err != nil {
		return err
	}
	st, legacy, err := loadState()
	if err != nil {
		return err
	}
	for _, id := range sortedIDs(st) {
		if err := validateCanonicalNamespace(st.Skills[id].Namespace); err != nil {
			return fmt.Errorf("migrate %s: %w", id, err)
		}
	}
	if err := ensureGlobalRoot(); err != nil {
		return err
	}
	global, err := openRootedFS(cfg.global)
	if err != nil {
		return fmt.Errorf("pin legacy global root: %w", err)
	}
	defer global.Close()
	moved := 0
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		srcInfo, srcErr := global.Lstat(s.Name)
		if srcErr == nil {
			if !srcInfo.IsDir() || srcInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s legacy payload is not a real directory", id)
			}
			cur, err := hashRootFolder(global, s.Name)
			if err != nil {
				return err
			}
			if !equalHashes(cur, s.Files) {
				return fmt.Errorf("%s has drift in legacy skills directory; migration stopped without overwriting it", id)
			}
			if _, _, err := ensureCanonicalNamespace(s.Namespace); err != nil {
				return err
			}
			ns, err := openCanonicalNamespace(s.Namespace)
			if err != nil {
				return err
			}
			name := s.Name
			destInfo, destErr := ns.Lstat(name)
			if destErr == nil {
				if !destInfo.IsDir() || destInfo.Mode()&os.ModeSymlink != 0 {
					ns.Close()
					return fmt.Errorf("canonical destination for %s is not a real directory", id)
				}
				other, e := hashRootFolder(ns, name)
				ns.Close()
				if e != nil {
					return e
				}
				if !equalHashes(cur, other) {
					return fmt.Errorf("both legacy and canonical copies exist with different content for %s", id)
				}
				if dryRun {
					fmt.Printf("  [dry-run] remove duplicate %s\n", filepath.Join(cfg.global, name))
				} else if err := global.RemoveAll(name); err != nil {
					return err
				}
			} else if !os.IsNotExist(destErr) {
				ns.Close()
				return destErr
			} else if dryRun {
				ns.Close()
				fmt.Printf("  [dry-run] move %s -> %s\n", filepath.Join(cfg.global, name), canonicalPath(s))
			} else {
				stage := "." + name + ".migrate-stage"
				if err := copyRootPayload(global, name, ns, stage); err != nil {
					ns.Close()
					return err
				}
				if err := ns.Rename(stage, name); err != nil {
					_ = ns.RemoveAll(stage)
					ns.Close()
					return err
				}
				// Commit destination before deleting source, and check both pinned
				// roots immediately before that irreversible source removal.
				if err := ns.check(); err != nil {
					ns.Close()
					return err
				}
				ns.Close()
				if err := global.RemoveAll(name); err != nil {
					return err
				}
			}
			moved++
		} else if !os.IsNotExist(srcErr) {
			return srcErr
		} else {
			ns, e := openCanonicalNamespace(s.Namespace)
			if e != nil {
				return e
			}
			_, e = ns.Lstat(s.Name)
			ns.Close()
			if os.IsNotExist(e) {
				return fmt.Errorf("%s is missing from both legacy and canonical paths", id)
			}
			if e != nil {
				return e
			}
		}
	}
	repaired, err := repairSymbolicPins(st)
	if err != nil {
		return err
	}
	removed := 0
	if pi, err := openRootedFS(piDir); err == nil {
		defer pi.Close()
		dir, openErr := pi.root.Open(".")
		if openErr != nil {
			return fmt.Errorf("read legacy Pi root: %w", openErr)
		}
		entries, err := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("read legacy Pi root: %w", err)
		}
		for _, entry := range entries {
			link := filepath.Join(piDir, entry.Name())
			target, err := pi.Readlink(entry.Name())
			if err != nil {
				continue
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(piDir, resolved)
			}
			expected := filepath.Join(cfg.global, entry.Name())
			a, _ := filepath.Abs(resolved)
			b, _ := filepath.Abs(expected)
			if filepath.Clean(a) != filepath.Clean(b) {
				continue
			}
			known := false
			for _, s := range st.Skills {
				if s.Name == entry.Name() {
					known = true
					break
				}
			}
			if !known {
				continue
			}
			if dryRun {
				fmt.Printf("  [dry-run] remove legacy Pi link %s\n", link)
			} else if err := pi.Remove(entry.Name()); err != nil {
				return err
			}
			removed++
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("pin legacy Pi root: %w", err)
	}
	if legacy || repaired > 0 || !fileExists(cfg.state) {
		if err := saveState(st); err != nil {
			return err
		}
	}
	if fileExists(cfg.legacyState) {
		if dryRun {
			fmt.Printf("  [dry-run] remove %s\n", cfg.legacyState)
		} else {
			agent, e := openRootedFS(cfg.root)
			if e != nil {
				return fmt.Errorf("pin legacy state root: %w", e)
			}
			e = agent.Remove(filepath.Base(cfg.legacyState))
			_ = agent.Close()
			if e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	if !dryRun {
		if err := global.check(); err != nil {
			return err
		}
	}
	fmt.Printf("migration complete: %d skill directories handled, %d immutable pins repaired, %d managed Pi links removed\n", moved, repaired, removed)
	return nil
}

func repairSymbolicPins(st *State) (int, error) {
	candidates := map[string][]string{}
	cohorts := map[string][]string{}
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		if s.LocalDir == "" && isCommitSHA(s.PinnedRef) {
			key := s.Owner + "/" + s.Repo
			seen := false
			for _, ref := range candidates[key] {
				if ref == s.PinnedRef {
					seen = true
				}
			}
			if !seen {
				candidates[key] = append(candidates[key], s.PinnedRef)
			}
			if s.InstalledAt != "" {
				cohort := key + "@" + s.InstalledAt
				seen = false
				for _, ref := range cohorts[cohort] {
					if ref == s.PinnedRef {
						seen = true
					}
				}
				if !seen {
					cohorts[cohort] = append(cohorts[cohort], s.PinnedRef)
				}
			}
		}
	}
	if dryRun {
		count := 0
		for id, s := range st.Skills {
			if s.LocalDir == "" && !isCommitSHA(s.PinnedRef) {
				fmt.Printf("  [dry-run] resolve immutable pin for %s\n", id)
				count++
			}
		}
		return count, nil
	}
	stage, err := os.MkdirTemp(cfg.root, ".skl-pin-repair-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)
	roots := map[string]string{}
	resolved := map[string]string{}
	repaired := 0
	verify := func(s SkillState, ref string) (bool, error) {
		key := s.Owner + "/" + s.Repo + "@" + ref
		top := roots[key]
		if top == "" {
			dir := filepath.Join(stage, fmt.Sprintf("repo-%d", len(roots)))
			if err := os.MkdirAll(dir, 0755); err != nil {
				return false, err
			}
			top, err = downloadTarballFunc(s.Owner, s.Repo, ref, dir)
			if err != nil {
				return false, err
			}
			roots[key] = top
		}
		folder := filepath.Clean(filepath.FromSlash(folderOfSkillPath(s.SkillPath)))
		if folder == ".." || filepath.IsAbs(folder) || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("unsafe skill path %q", s.SkillPath)
		}
		skillDir := filepath.Join(top, folder)
		if _, err := os.Lstat(skillDir); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		files, err := hashFolder(skillDir)
		if err != nil {
			return false, err
		}
		return equalHashes(files, s.Files), nil
	}
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		if s.LocalDir != "" || isCommitSHA(s.PinnedRef) {
			continue
		}
		if len(s.Files) == 0 {
			return repaired, fmt.Errorf("cannot verify immutable revision for %s without recorded payload hashes", id)
		}
		key := s.Owner + "/" + s.Repo
		found := ""
		// A timestamp cohort only prioritizes candidates; every candidate must
		// reproduce this skill's hashes. Empty timestamps are never cohorts.
		tryRefs := []string{}
		if s.InstalledAt != "" {
			tryRefs = append(tryRefs, cohorts[key+"@"+s.InstalledAt]...)
		}
		for _, ref := range candidates[key] {
			if !containsString(tryRefs, ref) {
				tryRefs = append(tryRefs, ref)
			}
		}
		for _, ref := range tryRefs {
			ok, e := verify(s, ref)
			if e != nil {
				return repaired, e
			}
			if ok {
				found = ref
				break
			}
		}
		if found == "" {
			ref := resolved[key]
			if ref == "" {
				ref, err = resolveLatestCached(s.Owner, s.Repo, resolved)
				if err != nil {
					return repaired, err
				}
			}
			ok, e := verify(s, ref)
			if e != nil {
				return repaired, e
			}
			if !ok {
				return repaired, fmt.Errorf("cannot prove immutable revision for %s without changing payload", id)
			}
			found = ref
		}
		s.PinnedRef = found
		st.Skills[id] = s
		repaired++
	}
	return repaired, nil
}
