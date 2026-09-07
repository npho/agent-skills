// skl manages a namespaced user skill library and explicit project copies.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type paths struct {
	root, lib, global, profiles, state, legacyState, npxLock string
}

var cfg paths
var dryRun bool

func initPaths() {
	root := os.Getenv("SKL_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			root = ".agents"
		} else {
			root = filepath.Join(home, ".agents")
		}
	}
	cfg = paths{
		root: root, lib: filepath.Join(root, "lib"), global: filepath.Join(root, "skills"),
		profiles: filepath.Join(root, "etc", "profiles"), state: filepath.Join(root, "var", "state.json"),
		legacyState: filepath.Join(root, ".sync-state.json"), npxLock: filepath.Join(root, ".skill-lock.json"),
	}
}

func usage() {
	fmt.Print(`skl — namespaced skill library and project manager

Usage:
  skl migrate [--pi-skills DIR]                 migrate the legacy layout safely
  skl add [--global] [--force] <source>         add GitHub repo/URL or local skill directory
  skl sync [--force] [--cache DIR]              restore missing library skills at recorded pins
  skl update [--force] [selectors...]           advance library skills from original sources
  skl check                                     verify library hashes and global links
  skl list                                      list canonical library skills
  skl global list|enable|disable [selectors...] manage explicit global exposure
  skl profile list|show <name>                  inspect profiles in etc/profiles
  skl project init [--profile NAME] [skills...] create .agents manifest, lock, and real copies
  skl project add [--force] <skills...>         add selections to an existing project
  skl project refresh [--force]                 explicitly re-resolve manifest and profiles
  skl project sync [--force]                    reproduce the exact project lock
  skl project update [--force] [skills...]      advance directly from recorded sources

Selectors are namespace/name or an unqualified name when unique. Project commands
use the current directory (or --project DIR). Every command accepts -n/--dry-run.
Library payloads live in lib/<namespace>/<skill>; skills/ contains only enabled
relative symlinks. npx owns .skill-lock.json; skl owns var/state.json.
`)
}

func main() {
	initPaths()
	args := os.Args[1:]
	filtered := args[:0]
	for _, arg := range args {
		if arg == "-n" || arg == "--dry-run" {
			dryRun = true
		} else {
			filtered = append(filtered, arg)
		}
	}
	if len(filtered) == 0 {
		usage()
		os.Exit(2)
	}
	var err error
	switch filtered[0] {
	case "migrate":
		err = cmdMigrate(filtered[1:])
	case "add":
		err = cmdAdd(filtered[1:])
	case "sync":
		err = cmdSync(filtered[1:])
	case "update":
		err = cmdUpdate(filtered[1:])
	case "check":
		err = cmdCheck()
	case "list":
		err = cmdList()
	case "global":
		err = cmdGlobal(filtered[1:])
	case "profile":
		err = cmdProfile(filtered[1:])
	case "project":
		err = cmdProject(filtered[1:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", filtered[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if !strings.HasPrefix(err.Error(), "usage:") {
			fmt.Fprintln(os.Stderr, "run `skl help` for usage")
		}
		os.Exit(1)
	}
}
