package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const stateVersion = 2
const projectVersion = 1

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
type NpxLock struct {
	Version int                  `json:"version"`
	Skills  map[string]LockSkill `json:"skills"`
}

type SkillState struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Path        string            `json:"path"`
	Origin      string            `json:"origin"` // npx-lock | github | local
	SourceURL   string            `json:"sourceUrl,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	SkillPath   string            `json:"skillPath,omitempty"`
	LocalDir    string            `json:"localDir,omitempty"`
	PinnedRef   string            `json:"pinnedRef,omitempty"`
	InstalledAt string            `json:"installedAt"`
	Files       map[string]string `json:"files"`
	Global      bool              `json:"global,omitempty"`
}
type State struct {
	Version int                   `json:"version"`
	Skills  map[string]SkillState `json:"skills"` // namespace/name -> record
}

type legacySkillState struct {
	Origin, SourceURL, Owner, Repo, SkillPath, LocalDir, PinnedRef, InstalledAt string
	Files                                                                       map[string]string
}
type legacyState struct {
	Version int                        `json:"version"`
	Skills  map[string]json.RawMessage `json:"skills"`
}

func canonicalID(namespace, name string) string { return namespace + "/" + name }
func canonicalRel(namespace, name string) string {
	return filepath.ToSlash(filepath.Join("lib", namespace, name))
}
func canonicalPath(s SkillState) string { return filepath.Join(cfg.root, filepath.FromSlash(s.Path)) }

func validatePart(kind, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("invalid %s %q", kind, value)
	}
	return nil
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
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
	return os.WriteFile(path, append(data, '\n'), 0644)
}

// loadState accepts v2 or converts the legacy root state in memory. Saving always writes v2.
func loadState() (*State, bool, error) {
	var raw legacyState
	path := cfg.state
	legacy := false
	if err := readJSON(path, &raw); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		path, legacy = cfg.legacyState, true
		if err := readJSON(path, &raw); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &State{Version: stateVersion, Skills: map[string]SkillState{}}, false, nil
			}
			return nil, false, err
		}
	}
	if raw.Version == stateVersion && !legacy {
		var st State
		if err := readJSON(path, &st); err != nil {
			return nil, false, err
		}
		if st.Skills == nil {
			st.Skills = map[string]SkillState{}
		}
		return &st, false, validateState(&st)
	}
	if raw.Version != 1 {
		return nil, false, fmt.Errorf("unsupported state version %d", raw.Version)
	}
	st := &State{Version: stateVersion, Skills: map[string]SkillState{}}
	for name, msg := range raw.Skills {
		var old legacySkillState
		if err := json.Unmarshal(msg, &old); err != nil {
			return nil, false, fmt.Errorf("legacy skill %s: %w", name, err)
		}
		ns := old.Owner
		if ns == "" {
			ns = "local"
		}
		origin := old.Origin
		if origin == "lock" {
			origin = "npx-lock"
		}
		id := canonicalID(ns, name)
		st.Skills[id] = SkillState{Name: name, Namespace: ns, Path: canonicalRel(ns, name), Origin: origin,
			SourceURL: old.SourceURL, Owner: old.Owner, Repo: old.Repo, SkillPath: old.SkillPath,
			LocalDir: old.LocalDir, PinnedRef: old.PinnedRef, InstalledAt: old.InstalledAt, Files: old.Files}
	}
	return st, true, validateState(st)
}

func validateState(st *State) error {
	if st.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", st.Version)
	}
	for id, s := range st.Skills {
		if err := validatePart("namespace", s.Namespace); err != nil {
			return fmt.Errorf("state %s: %w", id, err)
		}
		if err := validatePart("skill name", s.Name); err != nil {
			return fmt.Errorf("state %s: %w", id, err)
		}
		if id != canonicalID(s.Namespace, s.Name) {
			return fmt.Errorf("state key %q does not match %s/%s", id, s.Namespace, s.Name)
		}
		if s.Path != canonicalRel(s.Namespace, s.Name) {
			return fmt.Errorf("state %s has noncanonical path %q", id, s.Path)
		}
	}
	return nil
}
func saveState(st *State) error { st.Version = stateVersion; return writeJSON(cfg.state, st) }
func loadNpxLock() (*NpxLock, error) {
	var l NpxLock
	if err := readJSON(cfg.npxLock, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func sortedIDs(st *State) []string {
	ids := make([]string, 0, len(st.Skills))
	for id := range st.Skills {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func resolveSelectors(st *State, selectors []string) ([]string, error) {
	if len(selectors) == 0 {
		return sortedIDs(st), nil
	}
	seen := map[string]bool{}
	var result []string
	for _, selector := range selectors {
		if strings.Contains(selector, "/") {
			if _, ok := st.Skills[selector]; !ok {
				return nil, fmt.Errorf("skill %q not found in library", selector)
			}
			if !seen[selector] {
				seen[selector] = true
				result = append(result, selector)
			}
			continue
		}
		var matches []string
		for id, s := range st.Skills {
			if s.Name == selector {
				matches = append(matches, id)
			}
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			return nil, fmt.Errorf("skill %q not found in library", selector)
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("skill %q is ambiguous; use one of: %s", selector, strings.Join(matches, ", "))
		}
		if !seen[matches[0]] {
			seen[matches[0]] = true
			result = append(result, matches[0])
		}
	}
	return result, nil
}
