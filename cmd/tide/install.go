// `tide install` symlinks the running binary onto the user's PATH so it is
// reachable from a NON-INTERACTIVE ssh shell — which never sources .zshrc, so
// a shell alias to the binary is useless for `tide -r host` (ssh runs
// `tide --serve` non-interactively). Linking the real binary into ~/.local/bin
// (commonly on the default PATH) fixes that without touching shell config.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func install(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// /proc/self/exe is already resolved on Linux, but EvalSymlinks makes the
	// target the real file everywhere, so we never chain symlink→symlink.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	dir := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			dir = a
		}
	}
	if dir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return herr
		}
		dir = filepath.Join(home, ".local", "bin")
	}

	linked, err := installLinks(exe, dir)
	if err != nil {
		return err
	}
	for _, l := range linked {
		fmt.Printf("[tide] linked %s -> %s\n", l, exe)
	}
	if onPath(dir) {
		fmt.Printf("[tide] %s is on your PATH — `tide -r <this-host>` from another machine will now find tide\n", dir)
	} else {
		fmt.Printf("[tide] note: %s is not on your PATH yet — add it (e.g. in ~/.zshenv) so ssh can find tide non-interactively\n", dir)
	}
	return nil
}

// installLinks symlinks srcExe into dir as "tide", plus a sibling "teddy" if
// one sits next to the binary (they are built and aliased together). It never
// clobbers a non-symlink it did not create.
func installLinks(srcExe, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var linked []string
	if err := linkOne(dir, "tide", srcExe); err != nil {
		return nil, err
	}
	linked = append(linked, filepath.Join(dir, "tide"))

	teddy := filepath.Join(filepath.Dir(srcExe), "teddy")
	if fi, err := os.Stat(teddy); err == nil && !fi.IsDir() {
		if err := linkOne(dir, "teddy", teddy); err == nil {
			linked = append(linked, filepath.Join(dir, "teddy"))
		}
	}
	return linked, nil
}

// linkOne points dir/name at target. An existing symlink is repointed (it is
// ours to manage); a real file/dir is left alone with an error, so we never
// delete something the user put there.
func linkOne(dir, name, target string) error {
	link := filepath.Join(dir, name)
	if link == target {
		return nil // the binary already lives here
	}
	switch fi, err := os.Lstat(link); {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		if cur, _ := os.Readlink(link); cur == target {
			return nil // already correct
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	case err == nil:
		return fmt.Errorf("%s already exists and is not a tide symlink — remove it or pass a different dir", link)
	}
	return os.Symlink(target, link)
}

func onPath(dir string) bool {
	dir = filepath.Clean(dir)
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == dir {
			return true
		}
	}
	return false
}
