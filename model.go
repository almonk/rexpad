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

	"golang.org/x/sys/unix"
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
	// By is byUser when the user set Status by clicking the item's box in
	// the viewer. Any command that changes the item clears it, so it means
	// a change that the agent did not act on yet.
	By string `json:"by,omitempty"`

	// lines are where Text's wrapped lines start, as byte offsets, for the
	// width the list was last wrapped at (see renderer.wrapList); nil is
	// one line.
	lines []int
}

const byUser = "user"

// setByCommand and editByCommand are how commands change an item. They
// clear the user's mark, which says that the agent did not act on a click yet.
func (it *Item) setByCommand(st Status)    { it.Status, it.By = st, "" }
func (it *Item) editByCommand(text string) { it.Text, it.By = text, "" }

// clickedStatus is the status a click on an item's box gives it: a done
// item opens again, and anything else is done.
func clickedStatus(shown Status) Status {
	if shown == StatusDone {
		return StatusPending
	}
	return StatusDone
}

// toggleByUser applies a click on item id's box, as the user's change.
// shown is the status the viewer showed: the click means what the user saw,
// even if the list changed since the last frame.
func toggleByUser(path string, id int, shown Status) error {
	return updateList(path, func(l *List) error {
		it := l.find(id)
		if it == nil {
			return fmt.Errorf("no item %d", id)
		}
		it.Status, it.By = clickedStatus(shown), byUser
		return nil
	})
}

// toggleCollapsedByUser applies a click on the header of the group that
// groupKeys names key. shown is whether the viewer showed it collapsed: the
// click means what the user saw, even if the list changed since the last frame.
func toggleCollapsedByUser(path, key string, shown bool) error {
	return updateList(path, func(l *List) error {
		g := l.groupByKey(key)
		if g == nil {
			title, _, _ := strings.Cut(key, "\x00")
			return fmt.Errorf("no group %q", title)
		}
		g.Collapsed = !shown
		return nil
	})
}

// expandGroupOf expands the group that holds item id, so that the item
// shows.
func expandGroupOf(path string, id int) error {
	return updateList(path, func(l *List) error {
		g := l.groupOf(id)
		if g == nil {
			return fmt.Errorf("no item %d", id)
		}
		g.Collapsed = false
		return nil
	})
}

// extra is how much taller than one line the item is, in points.
func (it *Item) extra() float64 {
	if len(it.lines) < 2 {
		return 0
	}
	return float64(len(it.lines)-1) * lineH
}

type Group struct {
	Title string `json:"title"`
	// Progress overrides the ring's fill (0 to 1). Unset, it's the share of done items.
	Progress *float64 `json:"progress,omitempty"`
	// Collapsed hides the group's items. The user changes it with a click
	// on the group's header in the viewer, and an agent with collapse or
	// expand.
	Collapsed bool   `json:"collapsed,omitempty"`
	Items     []Item `json:"items"`
}

type List struct {
	Groups []Group `json:"groups"`
	NextID int     `json:"next_id"`

	wrapKey string // the width and scale its items are wrapped for
}

// folded says the group's items are hidden. Only a group with a title
// folds: without one, it has no header to click open again.
func (g *Group) folded() bool { return g.Collapsed && g.Title != "" }

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

// saveList writes l to a temp file and renames it into place, so a reader
// never sees half a list. Callers hold the list's lock (withListLock).
func saveList(path string, l *List) error {
	l.normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename succeeds
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644) // CreateTemp makes it private
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// withListLock runs fn while it holds an exclusive lock on path's list.
// Every writer takes it, so the CLI and the viewer never lose each other's
// changes between a load and a save.
func withListLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() // closing the file also releases the lock
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	return fn()
}

// updateList loads the list at path, applies change, and saves it, all
// under the list's lock.
func updateList(path string, change func(l *List) error) error {
	return withListLock(path, func() error {
		l, err := loadList(path)
		if err != nil {
			return err
		}
		if err := change(l); err != nil {
			return err
		}
		return saveList(path, l)
	})
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

func (l *List) groupOf(id int) *Group {
	for gi := range l.Groups {
		for _, it := range l.Groups[gi].Items {
			if it.ID == id {
				return &l.Groups[gi]
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

// groupKeys names each group so a group can be followed across reloads
// and rewrites of the list: its title, and which of the groups with that title it is.
func groupKeys(l *List) map[*Group]string {
	keys := map[*Group]string{}
	seen := map[string]int{}
	for gi := range l.Groups {
		g := &l.Groups[gi]
		keys[g] = fmt.Sprint(g.Title, "\x00", seen[g.Title])
		seen[g.Title]++
	}
	return keys
}

// groupByKey is the group that groupKeys names key, or nil.
func (l *List) groupByKey(key string) *Group {
	for g, k := range groupKeys(l) {
		if k == key {
			return g
		}
	}
	return nil
}

// keepCollapsed collapses each group of l that old had collapsed under the
// same title. Groups that share a title pair up in order.
func (l *List) keepCollapsed(old *List) {
	collapsed := map[string]bool{}
	for g, k := range groupKeys(old) {
		collapsed[k] = g.Collapsed
	}
	for g, k := range groupKeys(l) {
		g.Collapsed = g.Collapsed || collapsed[k]
	}
}

// group finds a group by case-insensitive title prefix, or by 1-based index.
func (l *List) group(ref string) *Group {
	if ref == "" {
		return nil // every title starts with "", so it would name the first group
	}
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
//
// Indented lines under an item are skipped.
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
			continue // detail under an item isn't kept
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
		}
	}
	return b.String()
}
