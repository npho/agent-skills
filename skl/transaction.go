package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type stagedLibrarySkill struct {
	ID      string
	State   SkillState
	Payload string
}

type linkSnapshot struct {
	exists bool
	target string
}

// rootedFS pins a real directory and refuses a mutation when the pathname no
// longer resolves to that same directory.  Root confines child operations; the
// identity check makes a concurrently replaced visible root fail closed rather
// than committing state for a different tree.
type rootedFS struct {
	path string
	root *os.Root
	info os.FileInfo
}

func openRootedFS(path string) (*rootedFS, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a real directory", path)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	pinned, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(pinned, current) {
		_ = r.Close()
		return nil, fmt.Errorf("root %s was replaced while opening", path)
	}
	return &rootedFS{path: path, root: r, info: pinned}, nil
}

func (r *rootedFS) check() error {
	current, err := os.Lstat(r.path)
	if err != nil {
		return fmt.Errorf("inspect pinned root %s: %w", r.path, err)
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(r.info, current) {
		return fmt.Errorf("pinned root %s was replaced", r.path)
	}
	return nil
}
func (r *rootedFS) Close() error { return r.root.Close() }
func (r *rootedFS) Lstat(name string) (os.FileInfo, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return r.root.Lstat(name)
}
func (r *rootedFS) Readlink(name string) (string, error) {
	if err := r.check(); err != nil {
		return "", err
	}
	return r.root.Readlink(name)
}
func (r *rootedFS) ReadDir(name string) ([]os.DirEntry, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	dir, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return entries, nil
}
func (r *rootedFS) Remove(name string) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.Remove(name)
}
func (r *rootedFS) RemoveAll(name string) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.RemoveAll(name)
}
func (r *rootedFS) Rename(old, new string) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.Rename(old, new)
}
func (r *rootedFS) Symlink(target, name string) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.Symlink(target, name)
}
func (r *rootedFS) MkdirAll(name string, perm os.FileMode) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.MkdirAll(name, perm)
}
func (r *rootedFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.root.WriteFile(name, data, perm)
}
func (r *rootedFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return r.root.OpenFile(name, flag, perm)
}

func openChildRoot(parent *rootedFS, child string) (*rootedFS, error) {
	if err := parent.check(); err != nil {
		return nil, err
	}
	r, err := parent.root.OpenRoot(child)
	if err != nil {
		return nil, err
	}
	info, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	path := filepath.Join(parent.path, child)
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) || parent.check() != nil {
		_ = r.Close()
		return nil, fmt.Errorf("child root %s was replaced while opening", path)
	}
	return &rootedFS{path: path, root: r, info: info}, nil
}

// renamePath is a failure-injection seam. Production transactions use
// descriptor-relative rename through rootedFS; project code uses os.Rename
// directly because it has its own project-root transaction.
var renamePath = func(string, string) error { return errRootedOperation }

// These seam functions return errRootedOperation in production; tests may
// replace them to inject a failure. Child mutation itself is always rooted.
var errRootedOperation = errors.New("perform rooted operation")
var symlinkPath = func(string, string) error { return errRootedOperation }
var removeLinkPath = func(string) error { return errRootedOperation }

func rootedRemoveHook(root *rootedFS, name string) error {
	err := removeLinkPath(filepath.Join(root.path, name))
	if errors.Is(err, errRootedOperation) {
		return root.Remove(name)
	}
	return err
}
func projectRename(old, new string) error {
	err := renamePath(old, new)
	if errors.Is(err, errRootedOperation) {
		return os.Rename(old, new)
	}
	return err
}

func rootedSymlinkHook(root *rootedFS, target, name string) error {
	err := symlinkPath(target, filepath.Join(root.path, name))
	if errors.Is(err, errRootedOperation) {
		return root.Symlink(target, name)
	}
	return err
}

// beforeNamespaceRename is an observation-only test hook. The actual mutation
// remains directory-relative through os.Root, so a hook cannot bypass confinement.
var beforeNamespaceRename = func(string, string, string) error { return nil }

// openCanonicalNamespace pins both trusted directory components. os.Root keeps
// operations in the opened library tree even if lib or its namespace is renamed
// or replaced after this check.
func openCanonicalNamespace(namespace string) (*rootedFS, error) {
	if err := validateCanonicalNamespace(namespace); err != nil {
		return nil, err
	}
	// Pin lib first. Opening namespace from that descriptor prevents a lib
	// replacement from selecting an attacker-controlled child.
	lib, err := openRootedFS(cfg.lib)
	if err != nil {
		return nil, fmt.Errorf("open canonical library: %w", err)
	}
	defer lib.Close()
	if err := lib.check(); err != nil {
		return nil, err
	}
	nsRoot, err := lib.root.OpenRoot(namespace)
	if err != nil {
		return nil, fmt.Errorf("open canonical namespace %s: %w", namespace, err)
	}
	pinned, err := nsRoot.Stat(".")
	if err != nil {
		_ = nsRoot.Close()
		return nil, err
	}
	path := filepath.Join(cfg.lib, namespace)
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(pinned, current) || lib.check() != nil {
		_ = nsRoot.Close()
		return nil, fmt.Errorf("canonical namespace %s was replaced while opening", namespace)
	}
	return &rootedFS{path: path, root: nsRoot, info: pinned}, nil
}

func copyRootPayload(src *rootedFS, source string, dst *rootedFS, dest string) error {
	return fs.WalkDir(src.root.FS(), source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := src.check(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := dest
		if rel != "." {
			target = filepath.ToSlash(filepath.Join(dest, rel))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in payload: %s", path)
		}
		if entry.IsDir() {
			return dst.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing non-regular payload entry: %s", path)
		}
		data, err := src.root.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return dst.WriteFile(target, data, info.Mode().Perm())
	})
}

func hashRootFolder(root *rootedFS, dir string) (map[string]string, error) {
	info, err := root.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a real directory", dir)
	}
	out := map[string]string{}
	err = fs.WalkDir(root.root.FS(), dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := root.check(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed in payload: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular payload entry: %s", path)
		}
		data, err := root.root.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out, err
}

func copyPayloadToRoot(src string, root *rootedFS, dest string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := dest
		if rel != "." {
			target = filepath.ToSlash(filepath.Join(dest, rel))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in staged payload: %s", path)
		}
		if entry.IsDir() {
			return root.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing non-regular staged payload entry: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return root.WriteFile(target, data, info.Mode().Perm())
	})
}

func renameInNamespace(root *rootedFS, namespace, old, new string) error {
	if err := beforeNamespaceRename(namespace, old, new); err != nil {
		return err
	}
	if err := validateCanonicalNamespace(namespace); err != nil {
		return err
	}
	return root.Rename(old, new)
}

func cloneState(st *State) (*State, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out State
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func recordStaged(next, old *State, item stagedLibrarySkill) error {
	files, err := hashFolder(item.Payload)
	if err != nil {
		return err
	}
	s := item.State
	installedAt := ""
	if old != nil {
		if prior, ok := old.Skills[item.ID]; ok && equalHashes(prior.Files, files) {
			installedAt = prior.InstalledAt
		}
	}
	if installedAt == "" {
		installedAt = time.Now().UTC().Format(time.RFC3339)
	}
	s.InstalledAt = installedAt
	s.Files = files
	next.Skills[item.ID] = s
	return nil
}

func validateCanonicalNamespace(namespace string) error {
	if err := validatePart("namespace", namespace); err != nil {
		return err
	}
	for _, path := range []string{cfg.lib, filepath.Join(cfg.lib, namespace)} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect canonical path %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("canonical path %s is not a real directory", path)
		}
	}
	return nil
}

// ensureCanonicalNamespace creates only one checked path component at a time,
// then revalidates it so MkdirAll can never follow a namespace symlink.
func ensureCanonicalNamespace(namespace string) (createdLib, createdNamespace bool, err error) {
	if err = validateCanonicalNamespace(namespace); err != nil {
		return false, false, err
	}
	root, err := openRootedFS(cfg.root)
	if err != nil {
		return false, false, fmt.Errorf("pin root for canonical namespace: %w", err)
	}
	defer root.Close()
	if err := root.check(); err != nil {
		return false, false, err
	}
	// Ensure lib directory exists via pinned root
	if _, lerr := root.Lstat("lib"); os.IsNotExist(lerr) {
		if err := root.MkdirAll("lib", 0755); err != nil && !os.IsExist(err) {
			return false, false, err
		}
		createdLib = true
	} else if lerr != nil {
		return false, false, lerr
	}
	if err := root.check(); err != nil {
		return createdLib, false, err
	}
	if err = validateCanonicalNamespace(namespace); err != nil {
		return createdLib, false, err
	}
	// Ensure namespace directory exists via pinned root
	nsPath := filepath.Join("lib", namespace)
	if _, lerr := root.Lstat(nsPath); os.IsNotExist(lerr) {
		if err := root.MkdirAll(nsPath, 0755); err != nil && !os.IsExist(err) {
			return createdLib, false, err
		}
		createdNamespace = true
	} else if lerr != nil {
		return createdLib, false, lerr
	}
	if err := root.check(); err != nil {
		return createdLib, createdNamespace, err
	}
	if err = validateCanonicalNamespace(namespace); err != nil {
		return createdLib, createdNamespace, err
	}
	return createdLib, createdNamespace, nil
}

func validateGlobalRoot() error {
	info, err := os.Lstat(cfg.global)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect global skills directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("global skills path %s is not a real directory", cfg.global)
	}
	return nil
}

func ensureGlobalRoot() error {
	if err := validateGlobalRoot(); err != nil {
		return err
	}
	root, err := openRootedFS(cfg.root)
	if err != nil {
		return fmt.Errorf("pin root for global: %w", err)
	}
	defer root.Close()
	if err := root.check(); err != nil {
		return err
	}
	if _, lerr := root.Lstat("skills"); os.IsNotExist(lerr) {
		if err := root.MkdirAll("skills", 0755); err != nil && !os.IsExist(err) {
			return err
		}
	} else if lerr != nil {
		return lerr
	}
	if err := root.check(); err != nil {
		return err
	}
	return validateGlobalRoot()
}

// recoverGlobalRoot removes only an unsafe root entry itself (never its target),
// recreates the root one component at a time, and verifies it before child I/O.
func recoverGlobalRoot() error {
	root, err := openRootedFS(cfg.root)
	if err != nil {
		return fmt.Errorf("pin root for recover global: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat("skills")
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		if err := root.Remove("skills"); err != nil {
			return fmt.Errorf("remove redirected global root: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect redirected global root: %w", err)
	}
	if err := root.check(); err != nil {
		return err
	}
	if err := ensureGlobalRoot(); err != nil {
		return fmt.Errorf("recreate global root: %w", err)
	}
	return nil
}

func globalTargets(st *State) (map[string]SkillState, error) {
	out := map[string]SkillState{}
	for id, s := range st.Skills {
		if !s.Global {
			continue
		}
		if old, ok := out[s.Name]; ok {
			return nil, fmt.Errorf("global name %q is selected by both %s and %s", s.Name, canonicalID(old.Namespace, old.Name), id)
		}
		out[s.Name] = s
	}
	return out, nil
}

func commitLibraryTransaction(old, next *State, items []stagedLibrarySkill, requestedGlobalNames ...string) error {
	if err := validateGlobalRoot(); err != nil {
		return err
	}
	_, globalRootErr := os.Lstat(cfg.global)
	globalRootExisted := globalRootErr == nil
	if err := validateState(next); err != nil {
		return fmt.Errorf("invalid prospective state: %w", err)
	}
	stagedIDs := make(map[string]bool, len(items))
	for _, item := range items {
		if err := validateCanonicalNamespace(item.State.Namespace); err != nil {
			return fmt.Errorf("stage %s: %w", item.ID, err)
		}
		if stagedIDs[item.ID] {
			return fmt.Errorf("duplicate staged skill %s", item.ID)
		}
		stagedIDs[item.ID] = true
		s, ok := next.Skills[item.ID]
		if !ok || canonicalID(item.State.Namespace, item.State.Name) != item.ID || canonicalPath(s) != canonicalPath(item.State) {
			return fmt.Errorf("staged skill %s does not match prospective state", item.ID)
		}
		if !dryRun {
			files, err := hashFolder(item.Payload)
			if err != nil {
				return fmt.Errorf("validate staged skill %s: %w", item.ID, err)
			}
			if !equalHashes(files, s.Files) {
				return fmt.Errorf("staged skill %s does not match prospective hashes", item.ID)
			}
		}
	}
	oldGlobals, err := globalTargets(old)
	if err != nil {
		return err
	}
	newGlobals, err := globalTargets(next)
	if err != nil {
		return err
	}
	changedNames := map[string]bool{}
	for name, s := range oldGlobals {
		n, ok := newGlobals[name]
		if !ok || canonicalID(n.Namespace, n.Name) != canonicalID(s.Namespace, s.Name) {
			changedNames[name] = true
		}
	}
	for name, s := range newGlobals {
		o, ok := oldGlobals[name]
		if !ok || canonicalID(o.Namespace, o.Name) != canonicalID(s.Namespace, s.Name) {
			changedNames[name] = true
		}
	}
	for _, name := range requestedGlobalNames {
		changedNames[name] = true
	}
	changed := make([]string, 0, len(changedNames))
	for name := range changedNames {
		changed = append(changed, name)
	}
	sort.Strings(changed)
	snaps := map[string]linkSnapshot{}
	for _, name := range changed {
		if err := validateGlobalRoot(); err != nil {
			return err
		}
		link := filepath.Join(cfg.global, name)
		target, e := os.Readlink(link)
		if e == nil {
			allowed := false
			if previous, ok := oldGlobals[name]; ok && linkPointsTo(link, canonicalPath(previous)) {
				allowed = true
			}
			if desired, ok := newGlobals[name]; ok && linkPointsTo(link, canonicalPath(desired)) {
				allowed = true
			}
			if !allowed {
				return fmt.Errorf("refusing to replace unrelated global link %s", link)
			}
			snaps[name] = linkSnapshot{true, target}
			continue
		}
		if !os.IsNotExist(e) {
			return fmt.Errorf("global path %s is not a symlink: %w", link, e)
		}
		snaps[name] = linkSnapshot{}
	}
	rollbackSnaps := make(map[string]linkSnapshot, len(snaps)+len(oldGlobals))
	for name, snap := range snaps {
		rollbackSnaps[name] = snap
	}
	for name, s := range oldGlobals {
		if _, recorded := rollbackSnaps[name]; recorded {
			continue
		}
		if err := validateGlobalRoot(); err != nil {
			return err
		}
		link := filepath.Join(cfg.global, name)
		if target, err := os.Readlink(link); err == nil && linkPointsTo(link, canonicalPath(s)) {
			rollbackSnaps[name] = linkSnapshot{exists: true, target: target}
		}
	}
	rollbackNames := make([]string, 0, len(rollbackSnaps))
	for name := range rollbackSnaps {
		rollbackNames = append(rollbackNames, name)
	}
	sort.Strings(rollbackNames)
	// A global command cannot create a valid exposure unless its canonical
	// source is already a real directory. Add/update transactions may instead
	// provide that source as one of their fully validated staged payloads.
	for _, name := range changed {
		s, enabling := newGlobals[name]
		if !enabling {
			continue
		}
		if err := validateCanonicalNamespace(s.Namespace); err != nil {
			return fmt.Errorf("global source for %s: %w", canonicalID(s.Namespace, s.Name), err)
		}
		if stagedIDs[canonicalID(s.Namespace, s.Name)] {
			continue
		}
		info, err := os.Lstat(canonicalPath(s))
		if err != nil {
			return fmt.Errorf("global source for %s: %w", canonicalID(s.Namespace, s.Name), err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("global source for %s is not a real directory", canonicalID(s.Namespace, s.Name))
		}
		files, err := hashFolder(canonicalPath(s))
		if err != nil {
			return fmt.Errorf("global source for %s: %w", canonicalID(s.Namespace, s.Name), err)
		}
		if !equalHashes(files, s.Files) {
			return fmt.Errorf("global source for %s has library drift", canonicalID(s.Namespace, s.Name))
		}
	}
	if dryRun {
		for _, item := range items {
			fmt.Printf("  [dry-run] atomically install %s\n", item.ID)
		}
		fmt.Printf("  [dry-run] atomically update %s and global links\n", cfg.state)
		return nil
	}
	// Pin the agent root while preparing state and re-check it immediately
	// before the pathname-owned state swap below. Payload and global mutations
	// are descriptor-relative; this check prevents committing their state after
	// a visible agent root replacement.
	agentRoot, err := openRootedFS(cfg.root)
	if err != nil {
		return fmt.Errorf("pin agent root: %w", err)
	}
	defer agentRoot.Close()
	if err := agentRoot.MkdirAll("var", 0755); err != nil {
		return err
	}
	stateRoot, err := openChildRoot(agentRoot, "var")
	if err != nil {
		return fmt.Errorf("pin state parent: %w", err)
	}
	defer stateRoot.Close()
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	token := fmt.Sprintf(".skl-txn-%d", time.Now().UnixNano())
	stateTmpName := ".state-" + token
	stateTmp, err := stateRoot.OpenFile(stateTmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stateRoot.Remove(stateTmpName)
	if _, err := stateTmp.Write(append(data, '\n')); err != nil {
		stateTmp.Close()
		return err
	}
	if err := stateTmp.Chmod(0644); err != nil {
		stateTmp.Close()
		return err
	}
	if err := stateTmp.Close(); err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	type swap struct {
		dest, backup, stage, namespace string
		root                           *rootedFS
		had, installed                 bool
	}
	swaps := make([]swap, len(items))
	stateSwap := swap{dest: filepath.Base(cfg.state), backup: filepath.Base(cfg.state) + token}
	createdNamespaces := map[string]bool{}
	createdLib := false
	var globalPinned *rootedFS
	rollback := func() error {
		var rollbackErr error
		if stateSwap.installed {
			rollbackErr = errors.Join(rollbackErr, stateRoot.Remove(stateSwap.dest))
		}
		if stateSwap.had {
			rollbackErr = errors.Join(rollbackErr, stateRoot.Rename(stateSwap.backup, stateSwap.dest))
		}
		if globalPinned == nil {
			if err := recoverGlobalRoot(); err != nil {
				rollbackErr = errors.Join(rollbackErr, err)
			} else if reopened, openErr := openRootedFS(cfg.global); openErr != nil {
				rollbackErr = errors.Join(rollbackErr, openErr)
			} else {
				globalPinned = reopened
			}
		} else if err := globalPinned.check(); err != nil {
			// A redirected symlink may be removed safely (never its target); a
			// replacement real directory is external data and must remain intact.
			info, statErr := os.Lstat(cfg.global)
			if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				_ = globalPinned.Close()
				if recoverErr := recoverGlobalRoot(); recoverErr != nil {
					rollbackErr = errors.Join(rollbackErr, recoverErr)
				} else if reopened, openErr := openRootedFS(cfg.global); openErr != nil {
					rollbackErr = errors.Join(rollbackErr, openErr)
				} else {
					globalPinned = reopened
				}
			} else {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
		if globalPinned != nil && globalPinned.check() == nil {
			for _, name := range rollbackNames {
				snap := rollbackSnaps[name]
				if err := globalPinned.RemoveAll(name); err != nil {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove global link %s during rollback: %w", name, err))
					continue
				}
				if snap.exists {
					if err := rootedSymlinkHook(globalPinned, snap.target, name); err != nil {
						rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore global link %s: %w", name, err))
					}
				}
			}
			if !globalRootExisted {
				if err := agentRoot.Remove("skills"); err != nil && !os.IsNotExist(err) {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove transaction-created global root: %w", err))
				}
			}
		}
		for i := len(swaps) - 1; i >= 0; i-- {
			s := swaps[i]
			if s.root == nil {
				continue
			}
			if s.installed {
				rollbackErr = errors.Join(rollbackErr, s.root.RemoveAll(filepath.Base(s.dest)))
			}
			if s.had {
				rollbackErr = errors.Join(rollbackErr, renameInNamespace(s.root, s.namespace, filepath.Base(s.backup), filepath.Base(s.dest)))
			}
		}
		if libRoot, openErr := openRootedFS(cfg.lib); openErr == nil {
			for namespace := range createdNamespaces {
				if err := libRoot.Remove(namespace); err != nil && !os.IsNotExist(err) {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
			_ = libRoot.Close()
		} else if len(createdNamespaces) > 0 {
			rollbackErr = errors.Join(rollbackErr, openErr)
		}
		if createdLib {
			if err := agentRoot.Remove("lib"); err != nil && !os.IsNotExist(err) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
		return rollbackErr
	}
	fail := func(err error) error { return errors.Join(err, rollback()) }
	for _, item := range items {
		madeLib, madeNamespace, err := ensureCanonicalNamespace(item.State.Namespace)
		if err != nil {
			return fail(err)
		}
		createdLib = createdLib || madeLib
		if madeNamespace {
			createdNamespaces[item.State.Namespace] = true
		}
	}
	for i, item := range items {
		dest := canonicalPath(item.State)
		root, e := openCanonicalNamespace(item.State.Namespace)
		if e != nil {
			return fail(e)
		}
		name := filepath.Base(dest)
		backup := name + token
		stage := "." + name + token + ".stage"
		swaps[i] = swap{dest: dest, backup: filepath.Join(filepath.Dir(dest), backup), stage: stage, namespace: item.State.Namespace, root: root}
		defer root.Close()
		// Revalidate immediately before inspection; the pinned root below also
		// prevents a replacement in the remaining check/use interval escaping.
		if e := validateCanonicalNamespace(item.State.Namespace); e != nil {
			return fail(e)
		}
		if _, e := root.Lstat(name); e == nil {
			if e := renameInNamespace(root, item.State.Namespace, name, backup); e != nil {
				return fail(e)
			}
			swaps[i].had = true
		} else if !os.IsNotExist(e) {
			return fail(e)
		}
		if e := copyPayloadToRoot(item.Payload, root, stage); e != nil {
			return fail(e)
		}
		if e := renameInNamespace(root, item.State.Namespace, stage, name); e != nil {
			return fail(e)
		}
		swaps[i].installed = true
	}
	if err := ensureGlobalRoot(); err != nil {
		return fail(err)
	}
	globalPinned, err = openRootedFS(cfg.global)
	if err != nil {
		return fail(fmt.Errorf("pin global skills directory: %w", err))
	}
	defer globalPinned.Close()
	for _, name := range changed {
		if err := rootedRemoveHook(globalPinned, name); err != nil && !os.IsNotExist(err) {
			return fail(err)
		}
		if s, ok := newGlobals[name]; ok {
			rel, e := filepath.Rel(cfg.global, canonicalPath(s))
			if e != nil {
				return fail(e)
			}
			if e := rootedSymlinkHook(globalPinned, rel, name); e != nil {
				return fail(e)
			}
		}
	}
	for _, s := range swaps {
		if s.root != nil {
			if err := s.root.check(); err != nil {
				return fail(err)
			}
		}
	}
	if err := globalPinned.check(); err != nil {
		return fail(err)
	}
	if err := agentRoot.check(); err != nil {
		return fail(err)
	}
	if err := stateRoot.check(); err != nil {
		return fail(err)
	}
	if info, err := stateRoot.Lstat(stateSwap.dest); err == nil {
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("%s is not a regular state file", cfg.state))
		}
		if err := stateRoot.Rename(stateSwap.dest, stateSwap.backup); err != nil {
			return fail(err)
		}
		stateSwap.had = true
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	if injected := renamePath(filepath.Join(filepath.Dir(cfg.state), stateTmpName), cfg.state); !errors.Is(injected, errRootedOperation) {
		return fail(injected)
	}
	if err := stateRoot.Rename(stateTmpName, stateSwap.dest); err != nil {
		return fail(err)
	}
	stateSwap.installed = true
	for _, s := range swaps {
		if s.had && s.root != nil {
			_ = s.root.RemoveAll(filepath.Base(s.backup))
		}
	}
	if stateSwap.had {
		_ = stateRoot.Remove(stateSwap.backup)
	}
	return nil
}
