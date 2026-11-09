---
name: rexpad
description: Show the user a live, visual todo list of what you're working on, in a Rex pane beside yours, drawn with the kitty image protocol. Use at the start of any multi-step task in a Rex terminal (three or more steps, or when the user says "show your plan", "todo list", "show progress", "what are you working on"), and keep it updated as you work. Drive it with the `rexpad` CLI.
---

# rexpad: a visible todo list in Rex

`rexpad` draws your task list as a picture in a split pane next to your own
pane, in the style of a Things project: groups with a progress ring, a divider,
and checkbox rows. The pane redraws on its own whenever the list changes, so
the user can glance over and see what you're doing and what's left.

Each agent pane gets its own list automatically (keyed by `$REX_BLOCK`), so you
don't need to name it.

## Start of a task

Write the whole plan in one call, then open the pane. `set` takes a Markdown
checklist on stdin and prints it back with each item's ID as `{#ID}`.

```bash
rexpad set <<'EOF'
# Reproduce the crash
- [~] Run the failing test on the Studio
- [ ] Capture the stack trace

# Fix
- [ ] Guard the nil session in SidebarView
- [ ] Add a regression test
EOF
rexpad show
```

- `# Heading` starts a group. Group by phase or area; one group is fine.
- `- [ ]` pending, `- [~]` active (working on it now), `- [x]` done.
- `show` opens the pane without taking focus and is idempotent: calling it
  again when it's already open does nothing.

## While you work

Keep exactly one item active, and update the list the moment the state
changes, not in a batch at the end.

```bash
rexpad next              # finish the active item, start the next open one
rexpad start 4           # make item 4 the active one (others drop back to pending)
rexpad done 3 5          # check items off
rexpad add "Bump the swift-ghostty pin"          # append to the last group, prints the ID
rexpad add -g Fix "Handle the empty-tab case"    # append to a group by title prefix or index
rexpad edit 4 "Guard the nil session and window"
rexpad rm 6
rexpad get               # print the list with IDs when you lose track
```

Replanning mid-task is normal: `rexpad get`, edit the text, and `set` it
again. Keep the `{#ID}` suffixes to preserve IDs, or drop them to renumber.

## Writing good items

- Short, specific, imperative, on one line: "Add a regression test for
  restore-on-launch", not "Testing". Long items wrap onto more lines.
- Use the user's names for things (files, tickets, people).
- A group's ring fills as its items are done (active counts half). Pin it with
  `rexpad progress GROUP 75` if the items don't tell the whole story, and
  `rexpad progress GROUP auto` to go back.

## Steps the user must do

Some steps need the user, for example a login, an approval, or a value that
only they know. Add each one as an item, and ask the user to click its box
in the pane when they finish. The pane must be open (`rexpad show`), or the
user has no box to click. Then run `rexpad wait` as a background command,
so that you continue when the user clicks:

```bash
rexpad add -g "Your part" "Log in to the AWS console"   # prints the ID, here 5
rexpad wait 5    # in the background: exits when the user clicks box 5
```

`wait` prints the clicked items in the format of `rexpad get`, for example
`- [x] Log in to the AWS console {#5}`, and exits.

- A click marks the item `"by": "user"`, which `rexpad get --json` shows.
  Any command that changes the item clears the mark.
- `wait` exits at once if an item already has the mark. So a click that
  comes before `wait` starts is not lost.
- `rexpad wait` with no ID waits for a click on any item. The user can
  also uncheck one of your done items, to tell you that it is not done.
- After you act on a click, clear its mark with a command on the item, for
  example `rexpad done 5`. Otherwise the next `rexpad wait` exits at once
  for the same click.
- `set` replaces every item and drops the user's marks. While a `wait`
  runs, change the list with `add`, `edit`, `done`, and `rm`, not `set`.
  Before a `set`, run `rexpad get --json` and act on each item that has
  `"by": "user"`.

## End of a task

Check off the last item so the user sees everything done. Leave the pane open
so they can review it; close it with `rexpad hide` only when they ask, or
when you start unrelated work (then `set` the new plan instead).

## Other details

- Subagents or several agents sharing one list: pass the same `--list NAME`
  (or `REXPAD_LIST=NAME`) to every command, including `show`.
- `rexpad show` takes 30% of your pane (a 70/30 split). `--below` splits
  downward; `--ratio PCT` sets another share.
- Colors follow the Rex theme (its background, foreground, and blue) and
  update when the user switches themes. `--theme light` or `--theme dark`
  pins the original design's colors instead.
- `rexpad render -o /path/list.png` writes a PNG of the list, which you can
  read to check what the user sees.
- `rexpad set --json` accepts `{"groups":[{"title":"...","items":[{"text":"...","status":"pending|active|done"}]}]}`.
- Outside Rex (`$REX_SERVER` unset), `show` fails with a message. The list
  commands still work; skip the pane.
- The pane's process is `rexpad view`; pressing `q` in it closes the viewer.
- If the user wants a pane that ignores clicks, run `rexpad hide`, then
  `rexpad show --no-click`. `show` does nothing while the pane is open.
