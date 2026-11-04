// rexpad shows an agent's todo list in a Rex pane, drawn with the kitty
// graphics protocol. Agents drive it with short commands; the viewer pane
// redraws whenever the list changes.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"strconv"
	"strings"
)

const usage = `rexpad: show an agent's todo list in a Rex pane (kitty graphics)

Usage: rexpad [--list NAME] COMMAND [ARGS]

Showing the list
  show [--below] [--ratio PCT] [--theme T]   open the viewer beside your pane (30% by default, no focus change); idempotent
  hide                                       close the viewer pane
  view [--theme terminal|light|dark] [--scale N]
                                             run the viewer in this terminal (q to quit)
  render -o FILE.png [--width PT] [--height PT] [--scale N] [--theme T]
                                             write a PNG of the list

Writing the whole list
  set [--json]        replace the list from stdin: a Markdown checklist (default) or JSON
  get [--json]        print the list; items end with {#ID}
  clear               empty the list

Changing items
  group TITLE                   add a group (no-op if it exists); later adds go to it
  add [-g GROUP] [-n NOTE] TEXT add an item (to the last group unless -g); prints its ID
  start ID                      mark ID active (others active become pending; --also keeps them)
  done [ID...]                  mark done (no ID: the active items)
  next                          finish the active item and start the next open one
  undo ID...                    mark pending
  edit ID TEXT                  change an item's text
  note ID [TEXT]                set or clear an item's note (shows a page icon)
  rm ID...                      delete items
  progress GROUP PCT|auto       pin a group's ring fill (0-100), or go back to counting items

Markdown checklist for set:
  # Group title
  - [ ] pending item
  - [~] active item
  - [x] done item
    An indented line under an item is its note.

Agent skill
  skill install [--dir DIR]     install the skill for Claude Code (~/.claude/skills), or into DIR
  skill show                    print the skill

The list defaults to one per agent pane ($REX_BLOCK). Override with --list or $REXPAD_LIST.
`

var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rexpad:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	listFlag := ""
	// --list may come before or after the command.
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--list" || a == "-l":
			if i+1 >= len(args) {
				return errors.New("--list needs a name")
			}
			listFlag = args[i+1]
			i++
		case strings.HasPrefix(a, "--list="):
			listFlag = strings.TrimPrefix(a, "--list=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	if rest[0] == "skill" {
		return runSkill(rest[1:])
	}
	if rest[0] == "version" || rest[0] == "--version" {
		fmt.Println("rexpad", version)
		return nil
	}
	name := listName(listFlag)
	path := listPath(name)
	cmd, args := rest[0], rest[1:]

	// mutate loads the list, applies f, saves, and prints the result.
	mutate := func(f func(l *List) error) error {
		l, err := loadList(path)
		if err != nil {
			return err
		}
		if err := f(l); err != nil {
			return err
		}
		return saveList(path, l)
	}
	ids := func(args []string) ([]int, error) {
		var out []int
		for _, a := range args {
			n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(a, "{#"), "}"), "#"))
			if err != nil {
				return nil, fmt.Errorf("not an item ID: %q", a)
			}
			out = append(out, n)
		}
		return out, nil
	}
	setStatus := func(args []string, st Status) error {
		ns, err := ids(args)
		if err != nil {
			return err
		}
		return mutate(func(l *List) error {
			for _, n := range ns {
				it := l.find(n)
				if it == nil {
					return fmt.Errorf("no item %d", n)
				}
				it.Status = st
			}
			return nil
		})
	}

	switch cmd {
	case "show":
		fs := flag.NewFlagSet("show", flag.ContinueOnError)
		below := fs.Bool("below", false, "split below instead of right")
		ratio := fs.Int("ratio", 30, "pane share of the split, percent")
		theme := fs.String("theme", "terminal", "terminal (follow the Rex theme), light, or dark")
		if err := fs.Parse(args); err != nil {
			return err
		}
		split := "right"
		if *below {
			split = "below"
		}
		return showPane(name, showOptions{split: split, ratio: *ratio, theme: *theme})

	case "hide":
		return hidePane(name)

	case "view":
		fs := flag.NewFlagSet("view", flag.ContinueOnError)
		theme := fs.String("theme", "terminal", "terminal (follow the Rex theme), light, or dark")
		scale := fs.Float64("scale", 0, "device pixels per point (0: from the cell size)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return runView(name, viewOptions{theme: *theme, scale: *scale})

	case "render":
		fs := flag.NewFlagSet("render", flag.ContinueOnError)
		out := fs.String("o", "", "output PNG")
		width := fs.Float64("width", 520, "width in points")
		height := fs.Float64("height", 0, "height in points (0: fit the content)")
		scale := fs.Float64("scale", 2, "device pixels per point")
		theme := fs.String("theme", "light", "light or dark")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("render needs -o FILE.png")
		}
		l, err := loadList(path)
		if err != nil {
			return err
		}
		fonts, err := loadFonts()
		if err != nil {
			return err
		}
		pal := lightPalette
		if *theme == "dark" {
			pal = darkPalette
		}
		h := *height
		if h == 0 {
			_, h = layout(l)
			if h == 0 {
				h = 60
			}
		}
		r := &renderer{fonts: fonts, scale: *scale, pal: pal}
		img := r.Render(l, int(*width**scale), int(h**scale))
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		return png.Encode(f, img)

	case "set":
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "stdin is JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		var l *List
		trimmed := strings.TrimSpace(string(data))
		if *asJSON || strings.HasPrefix(trimmed, "{") {
			l = &List{}
			if err := json.Unmarshal(data, l); err != nil {
				return fmt.Errorf("parse JSON: %w", err)
			}
		} else if l, err = parseMarkdown(string(data)); err != nil {
			return err
		}
		if err := saveList(path, l); err != nil {
			return err
		}
		fmt.Print(l.markdown(true))
		return nil

	case "get", "list", "ls":
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		l, err := loadList(path)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(l)
		}
		fmt.Print(l.markdown(true))
		return nil

	case "clear":
		return saveList(path, &List{NextID: 1})

	case "path":
		fmt.Println(path)
		return nil

	case "group":
		title := strings.Join(args, " ")
		if title == "" {
			return errors.New("group needs a title")
		}
		return mutate(func(l *List) error {
			for i, g := range l.Groups {
				if strings.EqualFold(g.Title, title) {
					// Move it last so the next add lands in it.
					l.Groups = append(append(l.Groups[:i:i], l.Groups[i+1:]...), g)
					return nil
				}
			}
			l.Groups = append(l.Groups, Group{Title: title})
			return nil
		})

	case "add":
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		group := fs.String("g", "", "group title (prefix) or 1-based index")
		note := fs.String("n", "", "note")
		active := fs.Bool("start", false, "add it as the active item")
		if err := fs.Parse(args); err != nil {
			return err
		}
		text := strings.Join(fs.Args(), " ")
		if text == "" {
			return errors.New("add needs text")
		}
		var id int
		err := mutate(func(l *List) error {
			var g *Group
			if *group != "" {
				if g = l.group(*group); g == nil {
					l.Groups = append(l.Groups, Group{Title: *group})
					g = &l.Groups[len(l.Groups)-1]
				}
			} else if len(l.Groups) > 0 {
				g = &l.Groups[len(l.Groups)-1]
			} else {
				l.Groups = append(l.Groups, Group{})
				g = &l.Groups[0]
			}
			l.normalize()
			id = l.NextID
			l.NextID++
			st := StatusPending
			if *active {
				st = StatusActive
			}
			g.Items = append(g.Items, Item{ID: id, Text: text, Status: st, Note: *note})
			return nil
		})
		if err == nil {
			fmt.Println(id)
		}
		return err

	case "start":
		fs := flag.NewFlagSet("start", flag.ContinueOnError)
		also := fs.Bool("also", false, "keep other active items active")
		if err := fs.Parse(args); err != nil {
			return err
		}
		ns, err := ids(fs.Args())
		if err != nil {
			return err
		}
		if len(ns) == 0 {
			return errors.New("start needs an item ID")
		}
		return mutate(func(l *List) error {
			for _, n := range ns {
				if l.find(n) == nil {
					return fmt.Errorf("no item %d", n)
				}
			}
			if !*also {
				for gi := range l.Groups {
					for ii := range l.Groups[gi].Items {
						if it := &l.Groups[gi].Items[ii]; it.Status == StatusActive {
							it.Status = StatusPending
						}
					}
				}
			}
			for _, n := range ns {
				l.find(n).Status = StatusActive
			}
			return nil
		})

	case "done", "check":
		if len(args) > 0 {
			return setStatus(args, StatusDone)
		}
		return mutate(func(l *List) error {
			for gi := range l.Groups {
				for ii := range l.Groups[gi].Items {
					if it := &l.Groups[gi].Items[ii]; it.Status == StatusActive {
						it.Status = StatusDone
					}
				}
			}
			return nil
		})

	case "next":
		var started *Item
		err := mutate(func(l *List) error {
			for gi := range l.Groups {
				for ii := range l.Groups[gi].Items {
					if it := &l.Groups[gi].Items[ii]; it.Status == StatusActive {
						it.Status = StatusDone
					}
				}
			}
			for gi := range l.Groups {
				for ii := range l.Groups[gi].Items {
					if it := &l.Groups[gi].Items[ii]; it.Status == StatusPending && started == nil {
						it.Status = StatusActive
						started = it
					}
				}
			}
			return nil
		})
		if err == nil {
			if started != nil {
				fmt.Printf("%s {#%d}\n", started.Text, started.ID)
			} else {
				fmt.Println("all done")
			}
		}
		return err

	case "undo", "uncheck":
		return setStatus(args, StatusPending)

	case "edit":
		if len(args) < 2 {
			return errors.New("edit needs an ID and text")
		}
		ns, err := ids(args[:1])
		if err != nil {
			return err
		}
		return mutate(func(l *List) error {
			it := l.find(ns[0])
			if it == nil {
				return fmt.Errorf("no item %d", ns[0])
			}
			it.Text = strings.Join(args[1:], " ")
			return nil
		})

	case "note":
		if len(args) < 1 {
			return errors.New("note needs an ID")
		}
		ns, err := ids(args[:1])
		if err != nil {
			return err
		}
		return mutate(func(l *List) error {
			it := l.find(ns[0])
			if it == nil {
				return fmt.Errorf("no item %d", ns[0])
			}
			it.Note = strings.Join(args[1:], " ")
			return nil
		})

	case "rm", "remove", "delete":
		ns, err := ids(args)
		if err != nil {
			return err
		}
		return mutate(func(l *List) error {
			for _, n := range ns {
				if !l.remove(n) {
					return fmt.Errorf("no item %d", n)
				}
			}
			return nil
		})

	case "progress":
		if len(args) != 2 {
			return errors.New("progress needs GROUP and PCT (or auto)")
		}
		return mutate(func(l *List) error {
			g := l.group(args[0])
			if g == nil {
				return fmt.Errorf("no group %q", args[0])
			}
			if args[1] == "auto" {
				g.Progress = nil
				return nil
			}
			pct, err := strconv.ParseFloat(strings.TrimSuffix(args[1], "%"), 64)
			if err != nil {
				return fmt.Errorf("not a percentage: %q", args[1])
			}
			v := clamp01(pct / 100)
			g.Progress = &v
			return nil
		})
	}
	return fmt.Errorf("unknown command %q (try rexpad help)", cmd)
}
