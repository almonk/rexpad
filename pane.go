package main

import (
	"encoding/json"
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
var viewerSettings = []string{"REXPAD_FONT", "REXPAD_FONT_BOLD", "REXPAD_SCALE", "XDG_STATE_HOME"}

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
	float bool
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
	if opt.float {
		id, err := floatViewer(viewerCommand(self, name, opt.theme), opt)
		if err != nil {
			return err
		}
		return rememberViewer(name, id)
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
	return rememberViewer(name, id)
}

func rememberViewer(name, id string) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(viewerPath(name), []byte(id+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// rexJSON runs a rex command and decodes the JSON it prints into v.
func rexJSON(v any, args ...string) error {
	cmd := exec.Command("rex", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err == nil {
		err = json.Unmarshal(out, v)
	}
	if err != nil {
		return fmt.Errorf("rex %s: %w", args[0], err)
	}
	return nil
}

// floatViewer starts the viewer in a floating layer along the right edge of
// the caller's window, or the bottom edge for a split below. rex split can't
// float a pane, so this calls the server API.
func floatViewer(command []string, opt showOptions) (string, error) {
	var pane struct {
		SessionID string `json:"session_id"`
		WindowID  string `json:"window_id"`
	}
	if err := rexJSON(&pane, "whoami", "--json"); err != nil {
		return "", err
	}
	share := float64(opt.ratio) / 100
	bounds := [4]float64{1 - share, 0, share, 1} // x, y, w, h as fractions of the window
	if opt.split == "below" {
		bounds = [4]float64{0, 1 - share, 1, share}
	}
	req, _ := json.Marshal(map[string]any{
		"session_id": pane.SessionID,
		"window_id":  pane.WindowID,
		"bounds":     bounds,
		"layout": map[string]any{"block": map[string]any{
			"flavor":  "com.superlogical.terminal.shell",
			"label":   "Todo",
			"options": map[string]any{"shell": "none", "command": command},
		}},
	})
	var layer struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := rexJSON(&layer, "api", "call", "session.new_layer", string(req)); err != nil {
		return "", err
	}
	if len(layer.BlockIDs) == 0 {
		return "", errors.New("rex api call session.new_layer returned no block ID")
	}
	return layer.BlockIDs[0], nil
}

func hidePane(name string) error {
	id, ok := viewerOpen(name)
	os.Remove(viewerPath(name))
	if !ok {
		return nil
	}
	return exec.Command("rex", "block", "close", id).Run()
}
