package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	st, legacy, err := loadState()
	if err != nil {
		return err
	}
	moved := 0
	for _, id := range sortedIDs(st) {
		s := st.Skills[id]
		src := filepath.Join(cfg.global, s.Name)
		dest := canonicalPath(s)
		if dirExists(src) {
			cur, err := hashFolder(src)
			if err != nil {
				return err
			}
			if !equalHashes(cur, s.Files) {
				return fmt.Errorf("%s has drift in legacy skills directory; migration stopped without overwriting it", id)
			}
			if dirExists(dest) {
				other, err := hashFolder(dest)
				if err != nil {
					return err
				}
				if !equalHashes(cur, other) {
					return fmt.Errorf("both legacy and canonical copies exist with different content for %s", id)
				}
				if dryRun {
					fmt.Printf("  [dry-run] remove duplicate %s\n", src)
				} else if err := os.RemoveAll(src); err != nil {
					return err
				}
			} else if dryRun {
				fmt.Printf("  [dry-run] move %s -> %s\n", src, dest)
			} else {
				if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					return err
				}
				if err := os.Rename(src, dest); err != nil {
					return err
				}
			}
			moved++
		} else if !dirExists(dest) {
			return fmt.Errorf("%s is missing from both legacy and canonical paths", id)
		}
	}
	removed := 0
	if entries, err := os.ReadDir(piDir); err == nil {
		for _, entry := range entries {
			link := filepath.Join(piDir, entry.Name())
			target, err := os.Readlink(link)
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
			} else if err := os.Remove(link); err != nil {
				return err
			}
			removed++
		}
	}
	if legacy || !fileExists(cfg.state) {
		if err := saveState(st); err != nil {
			return err
		}
	}
	if fileExists(cfg.legacyState) {
		if dryRun {
			fmt.Printf("  [dry-run] remove %s\n", cfg.legacyState)
		} else if err := os.Remove(cfg.legacyState); err != nil {
			return err
		}
	}
	if !dryRun {
		if err := os.MkdirAll(cfg.global, 0755); err != nil {
			return err
		}
	}
	fmt.Printf("migration complete: %d skill directories handled, %d managed Pi links removed\n", moved, removed)
	return nil
}
