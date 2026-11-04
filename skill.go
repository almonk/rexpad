package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// The agent skill ships inside the binary so `go install` users can set it up
// without cloning the repo.
//
//go:embed skills/rexpad/SKILL.md
var skillMD []byte

func defaultSkillsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "skills")
}

func runSkill(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rexpad skill show | install [--dir SKILLS_DIR] | path [--dir SKILLS_DIR]")
	}
	fs := flag.NewFlagSet("skill "+args[0], flag.ContinueOnError)
	dir := fs.String("dir", defaultSkillsDir(), "skills directory to install into")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := filepath.Join(*dir, "rexpad", "SKILL.md")
	switch args[0] {
	case "show":
		_, err := os.Stdout.Write(skillMD)
		return err
	case "path":
		fmt.Println(target)
		return nil
	case "install":
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, skillMD, 0o644); err != nil {
			return err
		}
		fmt.Println("installed the rexpad skill at", target)
		return nil
	}
	return fmt.Errorf("unknown skill command %q", args[0])
}
