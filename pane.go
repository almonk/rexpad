package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The viewer pane's block ID is kept beside the list so `show` is idempotent
// and `hide` can close it.
func viewerPath(name string) string { return filepath.Join(stateDir(), name+".viewer") }

func viewerOpen(name string) (string, bool) {
	data, err := os.ReadFile(viewerPath(name))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", false
	}
	if err := exec.Command("rex", "block", "inspect", id).Run(); err != nil {
		return id, false
	}
	return id, true
}

// The Rex server starts the viewer in its own environment, not the caller's,
// so the settings that change how the viewer draws or where it finds the
// list go on its command line.
var viewerSettings = []string{
	"REXPAD_FONT", "REXPAD_FONT_BOLD", "REXPAD_FONT_SIZE", "REXPAD_TEXT_GAMMA", "REXPAD_SCALE", "XDG_STATE_HOME",
}

func viewerCommand(self, name, theme string) []string {
	view := []string{self, "view", "--list", name, "--theme", theme}
	var settings []string
	for _, key := range viewerSettings {
		if value := os.Getenv(key); value != "" {
			settings = append(settings, key+"="+value)
		}
	}
	if len(settings) == 0 {
		return view
	}
	return slices.Concat([]string{"/usr/bin/env"}, settings, view)
}

type showOptions struct {
	split string
	ratio int
	theme string
}

func showPane(name string, opt showOptions) error {
	if id, ok := viewerOpen(name); ok {
		fmt.Println(id)
		return nil
	}
	if _, err := exec.LookPath("rex"); err != nil || os.Getenv("REX_SERVER") == "" {
		return errors.New("not running inside Rex; run `rexpad view` in a terminal that supports the kitty graphics protocol")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"split", "--focus=false", "--split", opt.split, "--ratio", strconv.Itoa(opt.ratio),
		"--shell=none", "--label", "Todo"}
	if b := os.Getenv("REX_BLOCK"); b != "" {
		args = append(args, "-b", b)
	}
	args = append(args, "--")
	args = append(args, viewerCommand(self, name, opt.theme)...)
	cmd := exec.Command("rex", args...)
	cmd.Stderr = os.Stderr
	outBytes, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("rex split: %w", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(outBytes)))
	if len(lines) == 0 {
		return errors.New("rex split printed no block ID")
	}
	id := lines[len(lines)-1]
	// rex split doesn't apply --ratio yet (the split lands at 50/50), but a
	// move does, so re-place the new pane beside its anchor at the ratio.
	if b := os.Getenv("REX_BLOCK"); b != "" {
		_ = exec.Command("rex", "block", "move", id, opt.split, b, "--ratio", strconv.Itoa(opt.ratio)).Run()
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(viewerPath(name), []byte(id+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func hidePane(name string) error {
	id, ok := viewerOpen(name)
	os.Remove(viewerPath(name))
	if !ok {
		return nil
	}
	return exec.Command("rex", "block", "close", id).Run()
}
