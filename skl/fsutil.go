package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// copyDir copies src into dst, merging (files in dst that are absent from src are kept).
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, d.Type().Perm()|0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) // follows symlinks; skills are plain files
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func removeDir(p string) error {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(p)
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFolder returns slash-separated relative path -> sha256 of contents.
func hashFolder(dir string) (map[string]string, error) {
	res := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		h, err := sha256File(p)
		if err != nil {
			return err
		}
		res[filepath.ToSlash(rel)] = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ensureSymlink makes link point to target (a relative path). Returns a status string.
func ensureSymlink(link, target string) (string, error) {
	if _, err := os.Lstat(link); err == nil {
		if tgt, e := os.Readlink(link); e == nil {
			if tgt == target {
				return "ok", nil
			}
			if err := os.Remove(link); err != nil {
				return "", err
			}
			if err := os.Symlink(target, link); err != nil {
				return "", err
			}
			return "repointed", nil
		}
		return "conflict (exists as real directory)", nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return "", err
	}
	if err := os.Symlink(target, link); err != nil {
		return "", err
	}
	return "created", nil
}

func parseGitHubURL(u string) (owner, repo string, ok bool) {
	u = strings.TrimSuffix(strings.TrimSuffix(u, ".git"), "/")
	const marker = "github.com/"
	idx := strings.Index(u, marker)
	if idx < 0 {
		return "", "", false
	}
	parts := strings.Split(u[idx+len(marker):], "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func latestCommit(owner, repo string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/HEAD", owner, repo)
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github api: status %d", resp.StatusCode)
	}
	var j struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return "", err
	}
	return j.SHA, nil
}

// downloadTarball downloads github.com/owner/repo@ref as a tarball into destDir,
// extracts it, and returns the path of the extracted top-level directory.
func downloadTarball(owner, repo, ref, destDir string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	url := fmt.Sprintf("https://github.com/%s/%s/archive/%s.tar.gz", owner, repo, ref)
	tmpGz := filepath.Join(destDir, "dl.tar.gz")
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	f, err := os.Create(tmpGz)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	fIn, err := os.Open(tmpGz)
	if err != nil {
		return "", err
	}
	defer fIn.Close()
	gz, err := gzip.NewReader(fIn)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	top := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimPrefix(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, 'L', 'K': // pax headers, GNU long name/link
			continue
		}
		parts := strings.SplitN(name, "/", 2)
		if top == "" {
			top = parts[0]
		}
		if len(parts) == 1 {
			continue
		}
		target := filepath.Join(destDir, parts[1])
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return "", err
			}
			out, err := os.Create(target)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
			if err := os.Chmod(target, os.FileMode(hdr.Mode)&0777); err != nil {
				return "", err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return "", err
			}
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
	os.Remove(tmpGz)
	if top == "" {
		return "", fmt.Errorf("empty tarball")
	}
	// entries are extracted with the top-level prefix stripped, so destDir is the root
	return destDir, nil
}

// folderOfSkillPath turns a lock skillPath like "skills/docx/SKILL.md"
// into the repo-relative folder "skills/docx".
func folderOfSkillPath(skillPath string) string {
	parts := strings.Split(strings.Trim(skillPath, "/"), "/")
	if len(parts) > 1 && parts[len(parts)-1] == "SKILL.md" {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return strings.Trim(skillPath, "/")
}
