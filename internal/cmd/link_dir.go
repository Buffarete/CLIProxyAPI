package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// DoLinkDirs writes one linked auth file per Claude Code config dir and Codex
// home. The files hold no token; the running server picks them up from the auth dir.
func DoLinkDirs(cfg *config.Config, claudeDirs, codexHomes []string) error {
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil {
		return err
	}
	link := func(dir string, build func(string) (map[string]any, error)) error {
		// The shell leaves ~ unexpanded inside a comma-separated list.
		if rest, ok := strings.CutPrefix(dir, "~/"); ok {
			if home, errHome := os.UserHomeDir(); errHome == nil {
				dir = filepath.Join(home, rest)
			}
		}
		metadata, errBuild := build(dir)
		if errBuild != nil {
			return fmt.Errorf("link %s: %w", dir, errBuild)
		}
		path, errWrite := linked.WriteAuthFile(authDir, metadata)
		if errWrite != nil {
			return fmt.Errorf("link %s: %w", dir, errWrite)
		}
		fmt.Printf("linked %s (%s, %v) -> %s\n", linked.Dir(metadata), metadata["label"], metadata["plan_type"], path)
		return nil
	}
	for _, dir := range claudeDirs {
		if err = link(dir, linked.ClaudeDir); err != nil {
			return err
		}
	}
	for _, dir := range codexHomes {
		if err = link(dir, linked.CodexHome); err != nil {
			return err
		}
	}
	return nil
}
