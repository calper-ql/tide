# tide — current state

*Living document; update at the end of each increment.*
*Last updated 2026-06-17 (teddy T1). Product contract: [tide-spec-v1.md](tide-spec-v1.md).*

## Where we are

**Phase 1 — the terminal environment — is complete.** Every ruling in the
spec is ratified and implemented; the open-rulings section is empty. The
acceptance test passes (kill the terminal mid-build with a half-typed
command, reattach: output intact, keystroke completes), and the UI-level
flows — menu split, border drag, tab switch, '-' detach, kill-with-confirm
— are driven end-to-end in tests through real SGR/kitty input bytes.

| Foundation (spec §Core foundation)          | Status |
|---------------------------------------------|--------|
| §1 Session daemon                            | **Done** |
| §2 Attach protocol (v2: composed frames)     | **Done** |
| §3 Terminal pane (scoped VT)                 | **Done** |
| §4 Layout + shared input routing + chrome    | **Done** |
| Single CUA keymap incl. the Ctrl+C ruling    | **Done** |
| `tide-edit`, `tide-git`                      | Phase 2 — next |

## Architecture as built

```
cmd/tide            CLI + thin client: alt-screen glass, raw input up, render frames down
internal/client     dial, on-demand daemon spawn, seq-correlated rpc
internal/protocol   wire contract v2 (JSON-lines): rpc + input/resize up, render/detached down
internal/daemon     daemon: locks, serve loop, session lifecycle, exit-with-last-session
  ws.go             per-session workspace: clients, layout, selection, clipboard, render loop
  compositor.go     bar/panes/borders/overlays → ANSI frames + the clickable hitmap
  router.go         the one shared input layer: keymap, mouse routing, menus, paste guards
  pane.go           one shell on a PTY parsed into a VT grid; hooks up to the workspace
internal/layout     tab/split tree: exact tiling geometry, JSON round-trip
internal/input      input decoder (legacy/kitty/SGR/X10/paste/focus) + per-pane re-encoder
internal/vt         vt10x port + snapshot renderer, scrollback, wide glyphs, view API
internal/session    registry: identities+layouts in sessions.json, content per pane file
internal/project    root resolution (.git walk, worktrees, symlinks, --here)
internal/paths      runtime/state dirs, 0700/owner-verified
```

**The composition model.** The daemon owns everything on screen. Pane PTY
output parses into daemon-side VT grids; a per-session compositor renders
grids + chrome into positioned-ANSI frames (8ms coalescing) and fans them
out through per-client outboxes (slow clients evicted with a typed notice).
Clients run in the alt screen and are pure glass: raw input bytes up,
frames down. Every clickable thing the compositor draws lands in a hitmap
the router consults — chrome geometry is never guessed.

**The keymap (ratified rulings, implemented).** Selection-aware Ctrl+C:
drag selections (content-coordinate, transient, cleared by any keystroke
or focus change) copy to the internal clipboard + the client's clipboard
(OSC 52 on the render stream, plus a typed copy frame the client pipes
into pbcopy/wl-copy/xclip/xsel — OSC 52 alone dies in Terminal.app), with
a bar flash; no selection means 0x03 reaches the shell. On macOS the
release-time primary feed also lands in the system clipboard
(copy-on-select: no terminal forwards ⌘C to the app, so select → ⌘V is
the Mac path).
Mouse selection feeds PRIMARY on release. Ctrl+V pastes through guards —
bracketed-paste panes get wrapped pastes, multi-line pastes into bare
shells get a confirm overlay. Ctrl+Shift+E detaches (kitty keyboard
protocol; the bar's '-' button covers every terminal). Everything else is
re-encoded per the destination pane's own terminal modes and forwarded.

**Mouse-first, discoverable (pane frames + window-centric edge splits,
i3-style, ruled 2026-06-19).** Every pane is framed from the start; its top
border is a bar — title left, [≡] pane-menu button right
(Copy/Paste/Restart Shell/Close Pane), focused pane highlighted. The lower
pane's bar IS the stacked divider (shared edges render once). The model is
i3's: every frame cell is the EDGE of the window beside it, and clicking an
edge (press+release in place) opens a compact directional menu scoped to
THAT window — the clicked side's direction first, then the two perpendicular
ones — each item a `Layout.Split` (same-axis joins the run, perpendicular
nests a container). The outer ring is segmented per window, so the bottom
strip under the right pane is the right pane's own bottom edge. Junction
cells (┬┴├┤┼, and the ┴ where a divider meets the bottom ring) instead open
a full-span menu — full-width above/below, or full-height left/right, of the
container the boundary divides — so a full-width pane below an L|R split is
one click on the ┴. Press+drag still resizes (corners grab both axes). On
terminals reporting bare motion (1003; not stock macOS Terminal.app), the
edge under the pointer highlights in heavy strokes. The session bar's project segment (▾) opens the session menu (New
Tab/Detach/Kill Session…); '+' and '-' stay. Nothing requires
right-click (macOS Terminal.app never forwards it), but it remains a
pane-menu accelerator where terminals do. Wheel scrolls daemon-side
scrollback; apps that request mouse reporting get translated events with
press-grab drag semantics; Shift bypasses to tide.

**Theme.** Built strictly from the terminal's own 16-color palette and
default fg/bg — tide inherits the user's theme, adapts to light/dark, and
needs no truecolor. One ANSI-cyan accent carries all signaling: focused
pane perimeter, accent pills for the project segment and active tab,
hover brightening on every interactive element. Rounded corners
throughout; menu hierarchy via a soft surface with dimmed titles; exited
panes tint red. The palette is one constant block in compositor.go.

**Sessions and persistence.** Identity = canonical project root; layout
(tabs, splits, ratios, focus) persists in sessions.json on every
structural change and at teardown; pane content checkpoints debounced into
per-pane files (stale-pane and killed-session writes rejected; strays
swept at startup; corrupt state quarantined, malformed layouts fall back
fresh with a warning). Daemon restart restores tabs, splits, focus,
scrollback, and spawns fresh shells with a notice — input-affecting modes
(mouse reporting, bracketed paste, app cursor) reset on restore, since
the fresh shell never asked for the old app's modes. The daemon exits
with its last session (ruled) and respawns on demand.

**Pane fidelity.** The VT answers DSR/CPR/DA queries itself, tracks
bracketed paste and mouse modes, models wide glyphs, survives split escape
sequences across snapshots, and delivers focus reporting (CSI I/O) to apps
that asked. `TIDE_SESSION=<pane-id>:<socket>` is injected per pane with a
scrubbed environment (spec: capability model).

## Development

- `./cli.sh {build,test,check,ci,shell}` — pinned Docker toolchain,
  `--network=none` (the offline-build constraint re-proven every run).
- 110 tests, `-race` clean: unit, PTY integration against real shells, the
  spec acceptance test, and UI flows driven through real input bytes.
- Substantial increments pass a multi-agent adversarial review before
  commit (the interaction layer fixed 26 confirmed findings, incl. a
  restore-path data race and a state-file-triggered daemon panic). The
  pane-frames redesign is a UX-exploration increment: its deep review is
  deferred until the feel is validated (ruled with the design).

## Known v0 limits (accepted, flagged in code)

- Latest-wins client sizing across simultaneously attached clients of
  different sizes (discarded rows go to scrollback, never lost).
- Selection drift when a pane's history ring is at capacity; no
  double-click word-select yet; combining characters not modeled.
- JSON+base64 frame encoding and full-pane-rect redraws (no intra-pane
  diffing yet) — revisit before the protocol freezes for tools.
- Keymap editing UI ("editable from inside the UI") not built yet; the
  CUA defaults are fixed until it is.

## Next: Phase 2 — the tools (`teddy` first)

The editor ships as **`teddy`** (ruled 2026-06-17; design in
[tide-spec-v1.md](tide-spec-v1.md)): a standalone terminal editor that
integrates with tide via `TIDE_SESSION`. Activity bar + collapsible file
browser, draggable editor tabs labeled by path, a single editor/viewer area
(tide owns tiling), minimal editing with Ctrl+S/undo, chroma syntax
highlighting, and a markdown raw/viz toggle.

- **T1 (done)** — teddy standalone, built on `internal/tui` (a small
  cell-grid + diff renderer reusing `internal/input` and the 16-color
  palette) and `internal/highlight` (chroma as a lexer only). Ships the
  activity bar + collapsible file tree, draggable path-labeled tabs with
  in-strip reorder, a minimal editor (edit/save/undo, gutter, tab expansion,
  wide glyphs, click + wheel), syntax highlighting, and a markdown raw/viz
  toggle. `cli.sh build` produces `bin/teddy` alongside `bin/tide`; CI is
  `-race` green. UX-exploration increment: deep adversarial audit deferred
  until the feel is validated.
- **T2 (next)** — the cross-tide tab drag: a tab dragged out of teddy is
  delivered through tide (tear-off to a new pane, drop onto another teddy, or
  path paste into a terminal). Adds the first tool-facing protocol surface
  (`tool_attach`, `drag_start`/`drag_drop`, `open`) and a
  spawn-pane-with-command daemon capability.

### teddy T1 known limits (accepted, to revisit)
- Closing a tab discards its unsaved buffer without a prompt; no
  unsaved-buffer crash survival yet (the full-product acceptance test —
  teddy will self-checkpoint dirty buffers later).
- Project-wide search is literal substring only (async, results capped at
  1000, files >1MiB and binaries skipped); no regex or search-&-replace yet.
  No editor text selection / copy-paste-within (paste-in works), no fuzzy
  open, no LSP. Markdown tables render as raw text.
- Re-lexes the whole buffer on edit; files past 10k lines render unhighlighted.

`tide-git` follows teddy.
