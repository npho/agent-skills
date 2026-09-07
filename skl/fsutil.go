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
	info, err := os.Lstat(p)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}
func fileExists(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular()
}
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in skill payload: %s", p)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("refusing non-regular payload entry: %s", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
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
func hashFolder(dir string) (map[string]string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a real directory", dir)
	}
	res := map[string]string{}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed in payload: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("non-regular payload entry: %s", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		h, err := sha256File(p)
		if err != nil {
			return err
		}
		res[filepath.ToSlash(rel)] = h
		return nil
	})
	return res, err
}
func parseGitHubURL(u string) (owner, repo string, ok bool) {
	u = strings.TrimSuffix(strings.TrimSuffix(u, ".git"), "/")
	const marker = "github.com/"
	idx := strings.Index(u, marker)
	if idx < 0 {
		return "", "", false
	}
	parts := strings.Split(u[idx+len(marker):], "/")
	if len(parts) < 2 || validatePart("owner", parts[0]) != nil || validatePart("repo", parts[1]) != nil {
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
	if !isCommitSHA(j.SHA) {
		return "", fmt.Errorf("GitHub returned non-commit revision %q", j.SHA)
	}
	return strings.ToLower(j.SHA), nil
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func downloadTarball(owner, repo, ref, destDir string) (string, error) {
	if !isCommitSHA(ref) {
		return "", fmt.Errorf("ref %q is not an immutable commit SHA", ref)
	}
	url := fmt.Sprintf("https://github.com/%s/%s/archive/%s.tar.gz", owner, repo, ref)
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	if err := extractTarGz(resp.Body, destDir); err != nil {
		return "", fmt.Errorf("extract %s: %w", url, err)
	}
	return destDir, nil
}
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	top := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("absolute archive path %q", hdr.Name)
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path traversal %q", hdr.Name)
		}
		parts := strings.SplitN(filepath.ToSlash(clean), "/", 2)
		if top == "" {
			top = parts[0]
		} else if parts[0] != top {
			return fmt.Errorf("archive has multiple top-level directories")
		}
		if len(parts) == 1 {
			if hdr.Typeflag != tar.TypeDir {
				return fmt.Errorf("invalid top-level archive entry %q", hdr.Name)
			}
			continue
		}
		rel := filepath.FromSlash(parts[1])
		target := filepath.Join(root, rel)
		abs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(abs, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(hdr.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive links are not allowed: %q", hdr.Name)
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
	if top == "" {
		return fmt.Errorf("empty tarball")
	}
	return nil
}
func folderOfSkillPath(skillPath string) string {
	parts := strings.Split(strings.Trim(skillPath, "/"), "/")
	if len(parts) > 1 && parts[len(parts)-1] == "SKILL.md" {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return strings.Trim(skillPath, "/")
}
