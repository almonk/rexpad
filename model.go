package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	StatusDone    Status = "done"
)

type Item struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status Status `json:"status"`
	Note   string `json:"note,omitempty"`
}

type Group struct {
	Title string `json:"title"`
	// Progress overrides the ring's fill (0 to 1). Unset, it's the share of done items.
	Progress *float64 `json:"progress,omitempty"`
	Items    []Item   `json:"items"`
}

type List struct {
	Groups []Group `json:"groups"`
	NextID int     `json:"next_id"`
}

func (g *Group) progress() float64 {
	if g.Progress != nil {
		return clamp01(*g.Progress)
	}
	if len(g.Items) == 0 {
		return 0
	}
	done := 0.0
	for _, it := range g.Items {
		switch it.Status {
		case StatusDone:
			done++
		case StatusActive:
			done += 0.5
		}
	}
	return done / float64(len(g.Items))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// --- storage ------------------------------------------------------------

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "rexpad")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "rexpad")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// listName picks the list: --list, then $REXPAD_LIST, then the agent's
// pane ($REX_BLOCK) so each agent gets its own list, then "default".
func listName(flag string) string {
	for _, v := range []string{flag, os.Getenv("REXPAD_LIST"), os.Getenv("REX_BLOCK")} {
		if v != "" {
			return strings.Trim(unsafeName.ReplaceAllString(v, "-"), "-")
		}
	}
	return "default"
}

func listPath(name string) string { return filepath.Join(stateDir(), name+".json") }

func loadList(path string) (*List, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &List{NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var l List
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	l.normalize()
	return &l, nil
}

func saveList(path string, l *List) error {
	l.normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalize fills in IDs and statuses so hand-written JSON works.
func (l *List) normalize() {
	max := 0
	for _, g := range l.Groups {
		for _, it := range g.Items {
			if it.ID > max {
				max = it.ID
			}
		}
	}
	if l.NextID <= max {
		l.NextID = max + 1
	}
	for gi := range l.Groups {
		for ii := range l.Groups[gi].Items {
			it := &l.Groups[gi].Items[ii]
			if it.ID == 0 {
				it.ID = l.NextID
				l.NextID++
			}
			switch it.Status {
			case StatusPending, StatusActive, StatusDone:
			case "in_progress", "in-progress", "doing", "working":
				it.Status = StatusActive
			case "completed", "complete", "checked":
				it.Status = StatusDone
			default:
				it.Status = StatusPending
			}
		}
	}
}

func (l *List) find(id int) *Item {
	for gi := range l.Groups {
		for ii := range l.Groups[gi].Items {
			if l.Groups[gi].Items[ii].ID == id {
				return &l.Groups[gi].Items[ii]
			}
		}
	}
	return nil
}

func (l *List) remove(id int) bool {
	for gi := range l.Groups {
		items := l.Groups[gi].Items
		for ii := range items {
			if items[ii].ID == id {
				l.Groups[gi].Items = append(items[:ii], items[ii+1:]...)
				return true
			}
		}
	}
	return false
}

// group finds a group by case-insensitive title prefix, or by 1-based index.
func (l *List) group(ref string) *Group {
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(l.Groups) {
		return &l.Groups[n-1]
	}
	lr := strings.ToLower(ref)
	for i := range l.Groups {
		if strings.ToLower(l.Groups[i].Title) == lr {
			return &l.Groups[i]
		}
	}
	for i := range l.Groups {
		if strings.HasPrefix(strings.ToLower(l.Groups[i].Title), lr) {
			return &l.Groups[i]
		}
	}
	return nil
}

// --- markdown -----------------------------------------------------------

var (
	headingRE = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*$`)
	itemRE    = regexp.MustCompile(`^[-*+]\s+\[(.)\]\s+(.*?)\s*$`)
	bulletRE  = regexp.MustCompile(`^[-*+]\s+(.*?)\s*$`)
	idRE      = regexp.MustCompile(`\s*\{#(\d+)\}$`)
)

// parseMarkdown reads a checklist:
//
//	# Group title
//	- [ ] pending item
//	- [~] active item        ([>] and [/] also mean active)
//	- [x] done item
//	  An indented line under an item is its note.
func parseMarkdown(src string) (*List, error) {
	l := &List{NextID: 1}
	var cur *Group
	var last *Item
	for n, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		switch {
		case trimmed == "":
			continue
		case headingRE.MatchString(trimmed) && !indented:
			l.Groups = append(l.Groups, Group{Title: headingRE.FindStringSubmatch(trimmed)[1]})
			cur, last = &l.Groups[len(l.Groups)-1], nil
		case indented && last != nil:
			note := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			if last.Note != "" {
				last.Note += "\n"
			}
			last.Note += note
		default:
			text, status := "", StatusPending
			if m := itemRE.FindStringSubmatch(trimmed); m != nil {
				text = m[2]
				switch strings.ToLower(m[1]) {
				case "x":
					status = StatusDone
				case "~", ">", "/", "-", "*":
					status = StatusActive
				}
			} else if m := bulletRE.FindStringSubmatch(trimmed); m != nil {
				text = m[1]
			} else {
				return nil, fmt.Errorf("line %d: expected \"# Group\" or \"- [ ] item\", got %q", n+1, line)
			}
			if cur == nil {
				l.Groups = append(l.Groups, Group{})
				cur = &l.Groups[len(l.Groups)-1]
			}
			id := 0
			if m := idRE.FindStringSubmatch(text); m != nil {
				id, _ = strconv.Atoi(m[1])
				text = strings.TrimSpace(idRE.ReplaceAllString(text, ""))
			}
			cur.Items = append(cur.Items, Item{ID: id, Text: text, Status: status})
			last = &cur.Items[len(cur.Items)-1]
		}
	}
	l.normalize()
	return l, nil
}

// markdown prints the list in the format parseMarkdown reads, with each
// item's ID as a trailing {#n} so agents can refer to items.
func (l *List) markdown(withIDs bool) string {
	var b strings.Builder
	for gi, g := range l.Groups {
		if gi > 0 {
			b.WriteString("\n")
		}
		if g.Title != "" {
			fmt.Fprintf(&b, "# %s\n", g.Title)
		}
		for _, it := range g.Items {
			mark := " "
			switch it.Status {
			case StatusActive:
				mark = "~"
			case StatusDone:
				mark = "x"
			}
			fmt.Fprintf(&b, "- [%s] %s", mark, it.Text)
			if withIDs {
				fmt.Fprintf(&b, " {#%d}", it.ID)
			}
			b.WriteString("\n")
			for _, nl := range strings.Split(it.Note, "\n") {
				if nl != "" {
					fmt.Fprintf(&b, "  %s\n", nl)
				}
			}
		}
	}
	return b.String()
}
