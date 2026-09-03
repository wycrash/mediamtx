// Package main copies private web UI assets into internal/webui at go generate.
//
// Resolution order for each pack: ENV path, sibling checkout, in-tree checkout,
// git clone (private), already-copied dest. dvrplayer is copied as-is;
// mediamtx-admin is npm-built then the Vite dist is copied.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type pack struct {
	name         string
	dest         string
	envPath      string
	sibling      string
	repoEnv      string
	repo         string
	refEnv       string
	ref          string
	sourceMarker string
	destMarker   string
	entries      []string
	dist         string
	npmBuild     bool
	copyAll      bool
	required     bool
}

var packs = []pack{
	{
		name:         "dvrplayer",
		dest:         "dvrplayer",
		envPath:      "DVRPLAYER_PATH",
		sibling:      "../mediamtx-dvrplayer",
		repoEnv:      "DVRPLAYER_REPO",
		repo:         "https://github.com/wycrash/mediamtx-dvrplayer.git",
		refEnv:       "DVRPLAYER_REF",
		ref:          "main",
		sourceMarker: filepath.Join("js", "MediaMtxDvrPlayer.js"),
		destMarker:   filepath.Join("js", "MediaMtxDvrPlayer.js"),
		entries:      []string{"js", "css", "vendor", "embed.html"},
		required:     true,
	},
	{
		name:         "admin",
		dest:         "admin",
		envPath:      "ADMIN_PATH",
		sibling:      "../mediamtx-admin",
		repoEnv:      "ADMIN_REPO",
		repo:         "https://github.com/wycrash/mediamtx-admin.git",
		refEnv:       "ADMIN_REF",
		ref:          "main",
		sourceMarker: "package.json",
		destMarker:   "index.html",
		dist:         "admin",
		npmBuild:     true,
		copyAll:      true,
		required:     true,
	},
}

func main() {
	if err := do(); err != nil {
		log.Printf("ERR: %v", err)
		os.Exit(1)
	}
}

func do() error {
	webuiDir, err := os.Getwd()
	if err != nil {
		return err
	}
	repoRoot := filepath.Clean(filepath.Join(webuiDir, "..", ".."))

	for _, p := range packs {
		if err := copyPack(p, webuiDir, repoRoot); err != nil {
			if p.required {
				return err
			}
			log.Printf("skip %s: %v", p.name, err)
		}
	}
	return nil
}

func copyPack(p pack, webuiDir, repoRoot string) error {
	dest := filepath.Join(webuiDir, p.dest)
	src, origin, err := resolveSource(p, repoRoot)
	if err != nil {
		if markerPresent(dest, p.destMarker) {
			log.Printf("%s: using existing copy (%s)", p.name, dest)
			return nil
		}
		return err
	}
	defer func() {
		if strings.HasPrefix(origin, "git:") {
			_ = os.RemoveAll(src)
		}
	}()

	copyRoot := src
	if p.npmBuild && markerPresent(src, p.sourceMarker) {
		env := []string{}
		if player := playerPath(repoRoot, webuiDir); player != "" {
			env = append(env, "DVRPLAYER_PATH="+player)
		}
		if err := npmBuild(src, env); err != nil {
			if markerPresent(dest, p.destMarker) {
				log.Printf("%s: npm build failed, using existing copy (%s): %v", p.name, dest, err)
				return nil
			}
			return err
		}
		if p.dist != "" {
			copyRoot = filepath.Join(src, p.dist)
		}
	}

	if !markerPresent(copyRoot, p.destMarker) {
		return fmt.Errorf("%s: %s missing in %s", p.name, p.destMarker, copyRoot)
	}

	tmp, err := os.MkdirTemp(webuiDir, p.dest+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if p.copyAll {
		if err := copyDir(copyRoot, tmp); err != nil {
			return err
		}
	} else {
		copied := 0
		for _, name := range p.entries {
			from := filepath.Join(copyRoot, name)
			st, err := os.Stat(from)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			to := filepath.Join(tmp, name)
			if st.IsDir() {
				if err := copyDir(from, to); err != nil {
					return err
				}
			} else if err := copyFile(from, to); err != nil {
				return err
			}
			copied++
		}
		if copied == 0 {
			return fmt.Errorf("%s: nothing to copy from %s", p.name, copyRoot)
		}
	}

	if !markerPresent(tmp, p.destMarker) {
		return fmt.Errorf("%s: marker %s missing after copy from %s", p.name, p.destMarker, copyRoot)
	}

	if err := replaceDest(dest, tmp, p.copyAll, p.entries); err != nil {
		return err
	}
	log.Printf("%s: copied ← %s", p.name, origin)
	return nil
}

func replaceDest(dest, staged string, copyAll bool, entries []string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(dest, ".keep"), nil, 0o644)

	names := entries
	if copyAll {
		list, err := os.ReadDir(staged)
		if err != nil {
			return err
		}
		names = nil
		existing, _ := os.ReadDir(dest)
		for _, e := range existing {
			if e.Name() == ".keep" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(dest, e.Name()))
		}
		for _, e := range list {
			names = append(names, e.Name())
		}
	}

	for _, name := range names {
		_ = os.RemoveAll(filepath.Join(dest, name))
		from := filepath.Join(staged, name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		to := filepath.Join(dest, name)
		if err := os.Rename(from, to); err != nil {
			if err := copyTree(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveSource(p pack, repoRoot string) (src, origin string, err error) {
	if v := strings.TrimSpace(os.Getenv(p.envPath)); v != "" {
		src = v
		if packRootOK(src, p) {
			return src, p.envPath + "=" + src, nil
		}
		return "", "", fmt.Errorf("%s: %s=%s is not a source or dist", p.name, p.envPath, src)
	}

	for _, candidate := range packCandidates(p, repoRoot) {
		if packRootOK(candidate, p) {
			return candidate, candidate, nil
		}
	}

	repo := strings.TrimSpace(os.Getenv(p.repoEnv))
	if repo == "" {
		repo = p.repo
	}
	ref := strings.TrimSpace(os.Getenv(p.refEnv))
	if ref == "" {
		ref = p.ref
	}
	if repo == "" {
		return "", "", fmt.Errorf(
			"%s not found at %s (set %s, clone next to mediamtx, or set %s)",
			p.name, filepath.Join(repoRoot, p.sibling), p.envPath, p.repoEnv)
	}

	tmp, err := os.MkdirTemp("", "mtx-webui-"+p.name+"-")
	if err != nil {
		return "", "", err
	}
	if err := gitClone(repo, ref, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("%s: git clone %s: %w", p.name, repo, err)
	}
	if !packRootOK(tmp, p) {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("%s: cloned %s but markers are missing", p.name, repo)
	}
	return tmp, "git:" + repo + "@" + ref, nil
}

func packCandidates(p pack, repoRoot string) []string {
	sibling := filepath.Clean(filepath.Join(repoRoot, p.sibling))
	out := []string{sibling, filepath.Join(repoRoot, filepath.Base(p.sibling))}
	if p.dist != "" {
		out = append(out, filepath.Join(sibling, p.dist))
	}
	return out
}

func packRootOK(root string, p pack) bool {
	return markerPresent(root, p.sourceMarker) || markerPresent(root, p.destMarker)
}

func playerPath(repoRoot, webuiDir string) string {
	if v := strings.TrimSpace(os.Getenv("DVRPLAYER_PATH")); v != "" {
		return v
	}
	marker := filepath.Join("js", "MediaMtxDvrPlayer.js")
	for _, p := range []string{
		filepath.Clean(filepath.Join(repoRoot, "..", "mediamtx-dvrplayer")),
		filepath.Join(repoRoot, "mediamtx-dvrplayer"),
		filepath.Join(webuiDir, "dvrplayer"),
	} {
		if markerPresent(p, marker) {
			return p
		}
	}
	return ""
}

func npmBuild(src string, extraEnv []string) error {
	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("npm not found (need Node >= 20): %w", err)
	}
	if err := runNPM(src, extraEnv, "ci"); err != nil {
		if err := runNPM(src, extraEnv, "install"); err != nil {
			return err
		}
	}
	return runNPM(src, extraEnv, "run", "build")
}

func runNPM(dir string, extraEnv []string, args ...string) error {
	cmd := exec.Command("npm", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("npm %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func markerPresent(root, marker string) bool {
	if root == "" || marker == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, marker))
	return err == nil && !st.IsDir()
}

func gitClone(repo, ref, dest string) error {
	args := []string{"clone", "--depth=1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, repo, dest)

	cmd := exec.Command("git", args...)
	cmd.Env = gitEnv()
	if hdr := gitAuthHeader(repo); hdr != "" {
		cmd.Args = append([]string{"git", "-c", hdr}, cmd.Args[1:]...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
}

func gitAuthHeader(repo string) string {
	token := firstEnv("DVRPLAYER_TOKEN", "ADMIN_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
	if token == "" {
		return ""
	}
	u, err := url.Parse(repo)
	if err != nil || (u.Host != "github.com" && !strings.HasSuffix(u.Host, ".github.com")) {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	return "http.extraHeader=Authorization: Bearer " + token
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func copyTree(src, dest string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return copyDir(src, dest)
	}
	return copyFile(src, dest)
}

func copyDir(src, dest string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o755)
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		return copyFile(p, filepath.Join(dest, rel))
	})
}

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
