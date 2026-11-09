package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// waitPoll is how often wait reads the list. Writes are a rename, so a
// read never sees half a list and needs no lock.
const waitPoll = 250 * time.Millisecond

// waitForUser blocks until an item among ids (any item when ids is empty)
// carries the user's mark, and returns those items in their groups. A mark
// set before the wait started counts too, so a click that lands while the
// agent starts the wait is not missed.
func waitForUser(path string, ids []int, poll time.Duration) (*List, error) {
	for {
		l, err := loadList(path)
		if err != nil {
			return nil, err
		}
		if err := checkWatched(l, ids); err != nil {
			return nil, err
		}
		if changed := userChanges(l, ids); len(changed.Groups) > 0 {
			return changed, nil
		}
		time.Sleep(poll)
	}
}

// checkWatched fails when none of ids is in the list, since the user can
// no longer change them and the wait would never end.
func checkWatched(l *List, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if l.find(id) != nil {
			return nil
		}
	}
	return fmt.Errorf("the list has no item %s", strings.Trim(fmt.Sprint(ids), "[]"))
}

// userChanges is the part of l that has the user's mark: the items among
// ids (all of them when ids is empty), in their groups.
func userChanges(l *List, ids []int) *List {
	out := &List{NextID: l.NextID}
	for _, g := range l.Groups {
		var items []Item
		for _, it := range g.Items {
			if it.By == byUser && (len(ids) == 0 || slices.Contains(ids, it.ID)) {
				items = append(items, it)
			}
		}
		if len(items) > 0 {
			out.Groups = append(out.Groups, Group{Title: g.Title, Items: items})
		}
	}
	return out
}
