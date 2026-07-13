# tide — design

How tide is built, as it stands. This describes the system that ships today; it is not a
roadmap or a changelog. For a quick tour and install/usage, see the [top-level
README](../README.md); for the connection-loss input guard, see
[security-input-guard.md](security-input-guard.md).

## Contents

- [The idea](#the-idea)
- [The daemon and the client](#the-daemon-and-the-client)
- [The per-pane terminal](#the-per-pane-terminal)
- [Sessions, identity, and persistence](#sessions-identity-and-persistence)
- [Input, controls, and the clipboard](#input-controls-and-the-clipboard)
- [The mouse-first split UI](#the-mouse-first-split-ui)
- [Themes](#themes)
- [Remote attach](#remote-attach)
- [teddy — the editor](#teddy--the-editor)
- [The wire protocol](#the-wire-protocol)
- [Security and the trust boundary](#security-and-the-trust-boundary)
- [Where things live](#where-things-live)

## The idea

tide is one Go binary that runs in two roles. As a **daemon** it holds every byte of your
on-screen state — the shells, their scrollback, the layout, the selection, the clipboard.
As a **client** it is pure glass: it puts the terminal in raw mode, sends your keystrokes
up, and paints the frames the daemon sends down, interpreting nothing itself.

Because the daemon owns everything, a client can die at any moment — you close the terminal,
the SSH link drops, the laptop sleeps — and nothing is lost. Reattach and the session is
exactly where you left it, mid-keystroke. The daemon spawns on demand the first time you run
`tide`, and it exits when its last session is killed; there is no service to manage.

Three ideas follow from "the daemon owns the screen" and recur throughout:

- **One control scheme.** The daemon's router owns the entire keymap and mouse model. The
  client forwards raw bytes and never intercepts a chord, so every attached terminal — and
  every tide-family tool — behaves identically.
- **Mouse-first and discoverable.** Every pane is framed and every clickable thing is drawn.
  As the compositor draws a frame it records a *hitmap*; the router turns a click into an
  action by asking that hitmap. Geometry is never guessed — it is read back from the last
  frame.
- **The message set is the boundary.** Client and daemon speak a small, versioned JSON
  protocol. That message set — not the binary — is what tools target and what a version
  handshake protects.

## The daemon and the client

Inside the daemon each session is a **workspace** (`ws`, `internal/daemon/ws.go`): a layout
tree of panes plus the per-session selection, clipboard, scroll, hover, and overlay state,
and its own render loop. A **pane** (`internal/daemon/pane.go`) is one shell running on a
PTY; the pane's single read loop feeds the PTY's bytes into a daemon-side terminal emulator
(a VT grid) that the pane owns. Panes know nothing about clients; clients are dumb glass; the
workspace is the one place input is routed and the screen is composed.

**The compositor** (`internal/daemon/compositor.go`) renders one frame under the workspace
lock: it lays down the session bar, the frame ring, each pane's bar and content, hover
strokes, any open popup, and finally the cursor — all as absolute-positioned ANSI
(`CSI y;xH` moves, then styled cells). The client writes those bytes verbatim into the alt
screen. A three-tier dirty model keeps this cheap: a *full* render clears and repaints
everything (on resize or restore), a *chrome* render repaints the frame and pane bars in
place with no clear (so hover never flickers), and a plain *content* render repaints only the
panes whose output changed.

**The render loop** coalesces work: every state change signals the loop, which then waits out
an 8 ms window (a ~120 fps cap), collapses all the dirt that accumulated, composes exactly
one frame, and broadcasts it. Each attached client has its own buffered outbox drained by a
dedicated writer goroutine, so a slow terminal can never stall the render loop — if its
outbox fills, that client is dropped with a typed "too slow" notice rather than blocking
everyone else. All clients of a session see one shared virtual screen, sized latest-wins from
the most recent client's resize.

**The hitmap** is a byproduct of that render. As the compositor draws, it appends a region
for every interactive element — bar segments, each pane's content rect, the `[+]`/`[≡]`
buttons, shared borders, the outer-ring edge strips, popup items — in z-order. The router
resolves a click by scanning the regions back-to-front so the topmost wins. Because the
hitmap is rebuilt on every frame, what you can click is always exactly what was last drawn.

## The per-pane terminal

Each pane owns an independent terminal emulator (`internal/vt`), a fork of
[vt10x](https://github.com/hinshun/vt10x) (MIT) with tide's extensions marked `tide:` in the
source. It is a full `xterm-256color`-class emulator: alt screen, the whole mouse-reporting
family (X10/1000/1002/1003 with SGR encoding), focus events, the Kitty keyboard protocol and
xterm `modifyOtherKeys`, wide (double-width) glyphs, DECSCUSR cursor shapes, and an extended
SGR set. It hosts shells, build output, and full-screen TUIs (its own comments are tuned for
lazygit, vim, and bubbletea alike). The only input it deliberately does not model is
zero-width / combining runes.

Two things make it more than a passthrough:

- **It answers its own queries.** Because the compositor renders *grids*, an app's query
  (DA/DSR/CPR, OSC color, Kitty flags) can never reach a real terminal — so the VT answers
  them itself, writing the reply straight back into the pane's PTY. OSC 52 clipboard writes
  by an inner program are captured and handed to the client's native clipboard tool.
- **It snapshots to ANSI for exact reattach.** The VT can serialize its entire live state —
  both screen grids, palette overrides, cursor, scroll region, custom tabs, deviating modes,
  and even a half-finished escape sequence — into a self-contained ANSI stream that a fresh
  emulator replays identically (deliberately never using RIS, which wipes scrollback in some
  terminals). This is what a new client's first frame is, and what a pane checkpoint stores.

Scrollback is a fixed-capacity ring (5000 lines) fed only by full-screen main-screen scrolls;
the alt screen has no history. Resize is tmux-exact: each grid keeps its own cursor visible,
and only the main grid trades rows with the history ring, so a shrink-then-grow round-trips.

## Sessions, identity, and persistence

A session is identified **only by its canonical project root** — there are no session names.
`tide` walks up from the working directory to the nearest `.git` (a directory *or* a
worktree `.git`-file) and uses its parent as the root; no repo found means the directory
itself is the root. The path is made absolute, symlink-resolved, and cleaned, so one project
on disk always maps to one identity. `tide <path>` and `tide --here` override the walk.

Lifecycle follows one **prime rule: nothing destroys a session as a side effect.** Every
client disconnect — closing the terminal, `Ctrl+Shift+E`, the bar's `−`, even a SIGHUP — is a
*detach* that leaves the session running. A session ends only by an explicit `tide kill`, the
"Kill Session" menu item, or `tide restart`. Killing the last session exits the daemon; the
next `tide` respawns it. Spawn races are settled by an exclusive `flock` — first to bind wins,
losers step aside — and a live-but-incompatible daemon is never raced; you are told to run
`tide restart`.

State is split across two file kinds under the state directory:

- **`sessions.json`** — the session identities and their layout trees (tabs, splits, ratios,
  focus). Small, rewritten only on structural change.
- **`pane-<hash>.json`** — one file per pane holding its ANSI snapshot and scrollback,
  rewritten by that pane's own 1 s debounce alone, so a busy pane never rewrites anything
  else. The filename is a hash of the pane id, which crosses the protocol boundary and is
  never trusted as a path component.

Every write is crash-atomic (temp + fsync + rename + dir-sync). A corrupt `sessions.json` is
renamed aside and the daemon starts fresh rather than dying; a corrupt pane file costs only
that pane's scrollback. On restart the layout and each pane's scrollback are restored, but
every pane runs a **fresh shell** with input-affecting modes (mouse reporting, bracketed
paste, app cursor) reset — the new shell never asked for the old app's modes — and a notice
is printed. Live processes are not, and cannot be, resurrected; the guarantee is
"recoverable to the last checkpoint," and for a client death (not a daemon death) that
checkpoint is the live state, i.e. exact.

Multiple clients may attach to one session at once. Panes are spawned with a **scrubbed
environment** and a `TIDE_SESSION=<pane-id>:<socket>` pointer (see
[Security](#security-and-the-trust-boundary)); tide refuses to *attach* from inside one of its
own panes (the tmux-in-tmux trap), though `ls`/`kill`/`restart` from a pane are fine.

## Input, controls, and the clipboard

There is one input layer, and it lives in the daemon's router (`internal/daemon/router.go`).
The client forwards its raw byte stream up; the router decodes it once per client (legacy
CSI/SS3, SGR mouse, Kitty CSI-u, `modifyOtherKeys`, bracketed paste, focus) and dispatches
each event. Query replies from apps (DSR/CPR/DA/OSC color) are recognized and dropped, never
delivered as keystrokes. A lone trailing `ESC` is resolved on a 50 ms idle flush, guarded by a
feed generation so a still-arriving sequence is never shredded.

Keys are checked in a fixed order: **detach first** (`Ctrl+Shift+E` fires even with a menu
open, so an overlay can never trap a client), then **the open overlay** if any (Up/Down/Enter/
Esc, everything else swallowed), then the **reserved CUA chords**, then **the focused pane**.

The signature ruling is **selection-aware `Ctrl+C`**. With a drag-selection active on the
focused pane, `Ctrl+C` copies and clears it and flashes a notice; with no selection, the byte
falls through to the pane as `0x03`, which the inner tty turns into SIGINT — so a second
`Ctrl+C` always interrupts. The guardrails that make this safe were paid for by prior art:
selections in terminal panes are *transient* drag-selections only, in content coordinates so
they stay glued to text as output scrolls, and any keystroke, focus change, tab switch, or
split clears them; the `Ctrl+Shift+C` alias copies-or-does-nothing but never falls through as
a stray control byte; and Enter is never a copy key.

The clipboard has three layers so copy/paste works everywhere:

- A daemon-side **internal clipboard** per workspace. `Ctrl+V` pastes *this* — no OSC 52 read,
  no system-clipboard read — so copy-then-paste inside tide works regardless of the host
  terminal.
- **OSC 52** on the render stream, for terminals that honor it (this is the path that crosses
  an SSH link back to a capable terminal).
- A typed **copy frame** the client pipes into the platform tool (`pbcopy` / `wl-copy` /
  `xclip` / `xsel`), because Terminal.app and older VTE silently drop OSC 52.

Mouse drag-selection feeds PRIMARY on release (middle-click paste) on Linux; on macOS — where
no terminal forwards `⌘C` to the app — that release-time feed maps to the system clipboard
instead, giving the native copy-on-select path (select, then `⌘V`). Explicit `Ctrl+V` runs
through paste guards: a bracketed-paste pane gets the payload wrapped in `200~`/`201~`; a bare
shell receiving a multi-line or control-laden paste gets a "Paste N lines?" confirmation
first. Terminal-native pastes ride the same guards.

Finally, keys are **re-encoded per destination pane.** The router snapshots the target pane's
own terminal modes (app cursor, app keypad, bracketed paste, LNM, Kitty/`modifyOtherKeys`) and
renders exactly what a terminal in those modes would have sent — arrows as SS3 vs CSI, control
folds, and the modifier combinations legacy encodings drop (shift+enter, ctrl+/) via the
pane's enabled enhanced protocol. Mouse events are likewise translated to pane-local
coordinates and the pane's own reporting protocol, with Shift as a bypass that hands the mouse
to tide and a wheel that falls back to daemon-side scrollback when the app isn't reporting.

## The mouse-first split UI

A session's arrangement is a plain data tree (`internal/layout`): a layout holds ordered
tabs, each tab a tree of pane leaves and splits with per-child ratios. Same-direction runs are
flattened into one node with N children, and the whole tree round-trips losslessly through
JSON — which is exactly what the daemon checkpoints. Geometry (`geometry.go`) tiles the tree
into cell rectangles with an exact-tiling invariant: side-by-side panes reserve a one-column
border between them; stacked panes reserve none, because the lower pane's bar row *is* the
divider. Borders are the draggable dividers; dragging one rewrites the split's ratios from the
realized integer sizes so the next layout reproduces the drag exactly.

On top of that geometry the router implements an **i3-style, mouse-first** model:

- **Every pane is framed.** Its top border is a bar: title on the left, a `[+]` split button
  and a `[≡]` pane menu on the right (both drop before the bar does on narrow panes).
- **Bars are focus and drag handles, never menus.** Clicking a title bar just focuses the
  pane — the most frequent gesture is 100% side-effect-free. (Dragging a bar that is also a
  stacked divider resizes; a dead pane's bar reads "(exited) — click to restart".)
- **Splitting is spatial.** You reach a split by clicking a window's *edge* — a shared
  vertical border is the right edge of the pane on its left; the outer ring is segmented per
  window so the strip under the right pane is *that* pane's bottom edge — or the `[+]` button.
  The `[≡]` menu is Copy / Paste / Restart Shell / Close Pane, with no split items.
- **Click-click menus.** An edge menu lists all four directions with the clicked side's
  direction first and pre-lit, and opens *anchored so that default row sits under the
  pointer* — so a second click on the same cell splits the obvious way with zero travel, no
  hover required. Near the bottom the menu flips upward, keeping the default under the
  pointer. Menus also take Up/Down + Enter and Esc.
- **Junctions split containers.** The cells where boundaries meet (`┬ ┴ ├ ┤ ┼`) open a
  full-span "Across" menu that splits the whole *container* a boundary divides — this is how
  you get a full-width pane below a left/right split. Junction glyphs are resolved from the
  arms that are actually drawn, never assumed, and the focused pane is drawn last so its
  accent wins the shared perimeter.
- **Gestures disambiguate by motion.** A press on a draggable boundary becomes a resize on the
  first pointer motion; a press with nothing to drag opens its menu on release-in-place, with
  a 3×3 slop so a jittery click still counts. An app that requested mouse reporting grabs the
  mouse so its own drags stay with it.

Popups are borderless cards — a title, a dim rule, items, dim rules as separators — where the
card's own background is the boundary; disabled items stay visible and say *why* they are
disabled, destructive items are red, and the default is pre-lit from open.

## Themes

tide builds its chrome **strictly from your terminal's own 16-color palette** and default
fg/bg — every color role is a self-resetting SGR prefix that references palette slots or
reverse video, never truecolor — so it inherits your palette and stays legible on light and
dark terminals with no configuration. A single selectable **accent slot** drives everything:
normal glyphs on slot `3n`, bright/bold pills on `9n`, and the whole session-bar strip and
popup card carry the accent fill (with default-bg glyphs), not just the pills.

Six presets ship, in picker order: **Tide** (cyan, the default), **Ocean** (blue), **Moss**
(green), **Plum** (magenta), **Ember** (yellow), and **Ink** — a no-chroma reverse-video
fallback whose contrast cannot fail on any palette. A few presets override the generic
derivation where a slot fails a contrast test (Ocean's dark blue, Plum's red-into-magenta,
Ember's over-glowing yellow). Every preset holds the same invariants: no style pairs fg and
bg from the same slot; chromatic fills never rely on dim; red is reserved for dead panes and
destructive actions; and color never carries meaning alone (bold, reverse, or a text suffix
always doubles the signal).

The picker is a sticky item in the session menu — it applies live and re-opens in place so you
can cycle presets and compare them click by click. The choice is the *user's*, not a
session's, so it persists daemon-globally in `prefs.json` (written only by the picker); a
missing, corrupt, or unknown value silently falls back to Tide and can never block an attach.
The active theme is an atomic pointer read once per frame, so a switch lands on every session
and client at once.

## Remote attach

`tide -r user@host [path]` runs the **full interactive client on your local machine** and the
daemon on the host, bridging them over a single `ssh -T` subprocess's stdio (wrapped as a
`net.Conn` so the same protocol frames flow over the tunnel — no network listener is opened).
The client being local is the entire point: it runs the native clipboard tool, so **copy
lands on your machine's clipboard** regardless of what any terminal does with OSC 52. The
copy frames simply ride back over the bridge to the local client, which drives exactly the
same render loop a local attach does.

No binary is pushed. The host must already have `tide` reachable — the remote command prefers
`tide` on PATH and falls back to the `tide install` default (`~/.local/bin/tide`), so neither
a missing PATH entry nor a shell alias (invisible to a non-interactive SSH shell) matters.
`tide install` symlinks the binary (and `teddy`) onto PATH for exactly this reason. A missing
`tide` or a protocol mismatch surfaces as an actionable error, and an incompatible host daemon
is **never killed** — the prime rule holds across the bridge.

The handshake deadline is generous (60 s) so it spans interactive SSH auth — a password prompt
or a 2FA push approved on a phone — plus the protocol handshake, and is cleared on success.
The connection-loss input guard protects a password typed blind if that auth stalls or the
link drops (see [security-input-guard.md](security-input-guard.md)).

With no path argument, the host side opens an interactive **picker**: a session chooser listing
the host daemon's live sessions (plus "+ New session…"), falling through to a filesystem
browser rooted at the host `$HOME`. It runs inside `tide --serve` on the host — so it reads the
*host* filesystem and reaches the *host* daemon — rendering frames the local client just
paints while shipping clicks and keys back. `tide -r host manage` runs the session manager over
the same bridge.

## teddy — the editor

teddy is a standalone terminal editor that ships as its own binary alongside `tide`. It runs
with **no daemon**: it drives the tty directly through a small cell-grid renderer
(`internal/tui`) in one single-threaded event loop, and it behaves identically inside or
outside a tide pane. (The daemon injects `TIDE_SESSION` into pane environments, but teddy does
not read it — the cross-tide tab tear-off that integration is designed for is not yet built.) Its root is the folder you open it in, taken verbatim — unlike `tide`, teddy does *not*
walk up to a `.git`, so the browser and search stay scoped to that subtree.

The layout is the familiar graphical-editor shape: a fixed activity bar (Explorer / Search /
Source Control), a collapsible, draggable side panel, a path-labeled draggable tab strip, one
editor/viewer area (tide owns tiling; teddy never tiles internally), and a status bar with an
actions-menu pill.
All three activities are live:

- **Explorer** — a lazy file tree that reconciles with disk on a 2 s poll while it is on
  screen (adding new files, dropping vanished ones, preserving expansion and selection) and
  auto-reveals the active file.
- **Editor** — a rune-line buffer with grouped undo/redo, a real content-diff dirty flag (undo
  back to the saved state clears the marker), tab-aware and wide-glyph-aware display geometry,
  and click + wheel navigation. Syntax highlighting comes from vendored **chroma** used
  strictly as a *lexer*: its tokens collapse to a small category enum mapped onto teddy's own
  16-color styles, so highlighting obeys your terminal's palette (files over 10k lines render
  unhighlighted). Markdown buffers get a raw ↔ preview toggle rendered in the same palette.
- **Search** — literal-by-default project search (regex, whole-word, and case toggles), async
  and cancellable, skipping `.git`, binaries, and files over 1 MiB, capped at 1000 results. It
  finds and opens; there is no replace.
- **Source Control** — a `git status` view (branch, ahead/behind, staged / changed / untracked
  groups) that shells out to the git CLI, with per-row stage/unstage and a commit box, polled
  every 2 s while visible. Clicking a changed file opens a read-only **diff tab** in the strip,
  rendered inline or side-by-side (side-by-side by default), rebuilt live while focused.

Known limits, honestly: no in-editor text selection or copy-*within* (paste-in works), no LSP,
no fuzzy open, no search-and-replace, whole-buffer re-lex on each edit, and unsaved buffers are
not yet checkpointed (closing a tab loses unsaved edits).

## The wire protocol

Client and daemon speak one protocol (`internal/protocol`): newline-delimited JSON frames,
each a single `Message` envelope whose `Type` selects which fields are meaningful, over a
user-private Unix socket — or, unchanged, over the SSH-tunneled stdio pipe for remote attach.
The protocol is transport-agnostic; the socket is one transport and SSH stdio is another.

The message set divides into RPC frames that carry a correlation `Seq` — `hello`, `attach`,
`ls`, `kill`, `shutdown`, and the `ok` / `error` / `sessions` / `killed` replies — and stream
frames that interleave without one: `input` and `resize` up, `render`, `detached`, `dropped`,
and `copy` down. That set — not the Go binary — is the coherence boundary tide-family tools
target.

Every connection opens with a bidirectional **`hello`** carrying a binary version and an
integer protocol version, sent send-then-read on the server and read-then-send on the client
so the exchange can't deadlock on any transport. The two sides attach **only on an exact
protocol-version match**; on a mismatch nothing is killed and the user is pointed at
`tide restart`. The protocol is at **version 3** (v2 replaced the raw pane-output stream with
composed render frames; v3 added the copy frame that carries selection text to the client's
native clipboard tool); the binary version is informational, and attach keys on the protocol
integer alone.

An attach reply carries the session's full-repaint snapshot as its first frame, enqueued under
the workspace lock so no render can ever precede it on the wire. From there the workspace fans
render frames out to N clients through the per-client outboxes described earlier.

## Security and the trust boundary

tide's trust boundary is the **OS user account**, and it is enforced twice over. The runtime
directory (socket, lock, log) is created `0700`, and on every start its owner is verified and
its mode re-hardened, so a pre-created or widened directory can't expose the socket. The
daemon binds a single **Unix-domain socket, `chmod 0600`** — the only listener in the whole
codebase. There is **no network listener anywhere and no telemetry**: tide makes no network
calls of its own, and remote use is SSH-in, so all transport encryption and auth are SSH's.

Panes are where foreign code runs, so each is spawned with a **scrubbed environment**: the
daemon's own environment minus stale terminal and multiplexer variables (`TIDE_SESSION`,
`TMUX`, `STY`, `TERM*`, `LINES`, `COLUMNS`), plus exactly a pinned `TERM=xterm-256color` and a
fresh `TIDE_SESSION=<pane-id>:<socket>`. That value is a **capability pointer, not a token** —
it tells a tide-family tool where the daemon is and lets `tide` itself refuse to nest; the
actual boundary remains the uid and the `0700`/`0600` permissions, since any same-uid process
can already reach the socket.

Finally, a dropped connection while you are typing blind into a no-echo prompt is contained by
the **abandoned-input guard**: tide keeps ownership of the tty in raw, no-echo mode and
discards keystrokes through the next Enter, so a password typed into a since-dropped remote
prompt can never fall through to your local shell's history. Its full threat model and
invariants are in [security-input-guard.md](security-input-guard.md).

## Where things live

```
cmd/tide            CLI + thin client (attach loop, remote attach, install)
cmd/teddy           the editor
internal/daemon     the daemon: workspaces, the input router, the compositor, panes, themes
internal/vt         the per-pane terminal (a vt10x port) + snapshot renderer + scrollback
internal/protocol   the wire contract
internal/layout     the tab/split tree and its exact tiling geometry
internal/input      the input decoder + per-pane key/mouse re-encoder
internal/session    the session registry, pane checkpoints, and prefs
internal/project    project-root resolution (.git walk, worktrees, symlinks, --here)
internal/paths      user-private runtime/state directories
internal/picker     the remote folder / session picker
internal/client     dial, on-demand daemon spawn, RPC
internal/tui        teddy's cell grid + diff renderer
internal/highlight  chroma-as-a-lexer for teddy
```

Everything is Go, every dependency is vendored and pinned, and the pinned build runs
`--network=none` — the repo builds offline, from itself. See the
[README](../README.md#development) for `./cli.sh`.
