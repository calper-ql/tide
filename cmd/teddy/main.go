// Command teddy is tide's editor: a standalone terminal editor with a
// VS Code-lineage activity bar, a collapsible file browser, draggable
// path-labeled tabs, and a single editor/viewer area (tide owns tiling).
// It runs with no daemon; inside a tide session it discovers TIDE_SESSION
// and the tab drag is delivered through tide (the T2 increment).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/calper-ql/tide/internal/project"
	"github.com/calper-ql/tide/internal/tui"
)

const usage = `teddy — tide's editor

usage:
  teddy [path]   open teddy rooted at a directory, or with a file open
  teddy --help
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "teddy:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Print(usage)
			return nil
		}
	}
	root, openPath, err := resolveTarget(args)
	if err != nil {
		return err
	}

	scr, err := tui.NewScreen()
	if err != nil {
		return err
	}
	defer scr.Close()

	app := newApp(scr, root)
	app.openArg = openPath
	return app.Run()
}

// resolveTarget maps the command line to teddy's root and an optional file to
// open. The root is the folder teddy was opened in — the path argument, or its
// parent when the argument is a file, or cwd — taken verbatim (canonicalized),
// with NO .git walk: the browser and search stay scoped to that folder and its
// subtree, never climbing to an ancestor repository.
func resolveTarget(args []string) (root, openPath string, err error) {
	target := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			target = a
			break
		}
	}

	dir := target
	if target == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", "", err
		}
	} else {
		abs, aerr := filepath.Abs(target)
		if aerr != nil {
			return "", "", aerr
		}
		if info, serr := os.Stat(abs); serr != nil || !info.IsDir() {
			openPath = abs // a file: existing, or one to be created
			dir = filepath.Dir(abs)
		} else {
			dir = abs
		}
	}

	root, err = project.Canonical(dir)
	return root, openPath, err
}
