# tide — terminal IDE
**Product spec v1 — frozen 2026-06-10**
*Methodology: pain driven development. Every requirement below was paid for.*

## Thesis
Zed's coherence, terminal-resident, crash-proof, mouse-first. One coherent
environment: one session, one control scheme, one protocol — whether a
capability ships inside the main binary or as a first-party tool. Never a
federation of foreign tools with N keybind dialects.

## Requirements

1. **Survives crashes.** UI, terminal, or GUI session dies for any reason →
   work is exactly recoverable, mid-keystroke. Always on, no setup ritual.
2. **One environment, three capabilities.** Edit code, work git, run
   commands — one coherent surface.
3. **Mouse-first.** Everything reachable by click: open, close, switch,
   resize, select, stage. Keyboard is an accelerator, never a requirement.
4. **One control scheme.** Single keymap, CUA defaults (Ctrl+S, Ctrl+C/V),
   editable from inside the UI. Config files are not a user-facing concept.
5. **Discoverable.** Every available action visible on screen in context.
   Nothing hidden behind memorized chords. Actions may be tucked behind a
   popup UI (gear menu, context menu, right-click) — hidden behind a click
   is fine; hidden behind knowledge is not.
6. **Tabs/tiles on demand.** Click to spawn an editor, terminal, or git view;
   click to close it.

## Scope decisions

| Decision        | v1                                            |
|-----------------|-----------------------------------------------|
| Workspaces      | Single project per session; N projects = N sessions |
| Remote          | Daemon lives where the code lives; attach from any terminal over SSH. Disconnect survival = crash survival. |
| Editor bar      | Replaces *my* IDE usage — ships as `teddy`, phase 2 |
| Code intel      | LSP — completion, goto-def, diagnostics; lives in `teddy` |
| Search          | Project-wide search & replace; lives in `teddy` |
| Git             | `tide-git` first-party tool, phase 2          |
| Debugger        | **Out** — stays in GoLand                      |
| Agent pane      | **Out** — agents run in terminal panes as anywhere |
| Assumed basics  | Phase 1: tabs/tiles, mouse selection, clickable chrome. `teddy` table stakes: syntax highlighting, fuzzy file open, editor tabs |

## Invocation

- `tide` in a project directory: if a session exists for that path → attach
  to it; otherwise create one and attach. One command, both cases.
- Session identity = project root path. No session names to invent or
  remember in v1.
- **Root resolution:** `tide` walks up from cwd to the nearest `.git`
  (worktree `.git`-files included) and uses that as the project root. No
  repo found → cwd is the root, stated in the status line. `tide <path>` /
  `tide --here` override. Nested repos: nearest `.git` wins; override
  covers the exceptions.
- `tide ls` lists live sessions (path, panes, since-when). `tide kill`
  ends the current path's session explicitly.
- Detach leaves the session running, always. Killing the terminal *is* a
  valid detach.
- **No nesting** *(ruled 2026-06-19)*. Panes always carry `TIDE_SESSION`, so
  `tide` launched inside one of its own panes refuses to attach (stacking a
  second alt-screen + mouse/keyboard regime is the tmux-in-tmux trap). The
  message points to detach, and to `unset TIDE_SESSION` as the force escape
  hatch. `ls`/`kill`/`restart` from within a pane stay allowed — they never
  attach.

## Platforms

- **Linux** — first-class, developed and tested on Ubuntu first.
- **macOS** — first-class, same release cadence.
- Implication: daemon/PTY/socket layer sticks to POSIX-portable mechanisms;
  no Linux-only dependencies in core. Windows explicitly out of scope.

## Core foundation — build first, everything sits on it

1. **Session daemon** — owns all state (buffers, PTYs, git, layout);
   survives any client/UI death. The crash-survival requirement lives here.
2. **Attach protocol** — client connects over local socket; SSH-in-and-attach
   is the remote story. `tide` invocation behavior is built on this.
3. **Terminal pane (scoped VT)** — PTY + VT emulation sized for shells,
   build output, and possibly tide-family tools (see Open rulings: tool
   render path). Never a general-purpose host for arbitrary third-party
   full-screen apps.
4. **Pane/tab layout + input routing** — one mouse/keyboard routing layer the
   whole app shares; this is where mouse-first and the single control scheme
   are enforced.

Capabilities ship as first-party standalone tools that attach to the session
— see Capability model.

## Capability model — first-party tools, one protocol

- The editor (`teddy`) and git tool (`tide-git`) ship as standalone
  binaries — usable outside tide, products in their own right.
- When tide spawns a pane it injects `TIDE_SESSION` (session id + socket
  path). Any tide-family tool launched in that pane discovers the session
  and speaks to the daemon: shared state, shared keymap, shared mouse
  routing.
- Coherence contract: every first-party tool reads the same control scheme
  from the session. One dialect — enforced by protocol, not by monolith.

## Delivery

- **Phase 1 — the terminal environment.** Foundation §1–4: daemon, attach,
  terminal panes/tabs, mouse-first chrome, `TIDE_SESSION` injection, and a
  session protocol stable enough for tools to target.
- **Phase 2 — the tools.** `teddy` (LSP, project-wide search & replace)
  and `tide-git`, attaching via the protocol.

## Engineering constraints

- **Go for everything we can.** Daemon, client, `teddy`, `tide-git` —
  all Go. Other languages only where Go is genuinely impossible.
- **All dependencies vendored** (`go mod vendor`), pinned, committed. The
  repo builds offline, from itself, forever.
- **Self-contained artifacts.** Each tool ships as a single static binary;
  no runtime dependencies, no install ritual.
- **License policy.** Permissive deps only (MIT/BSD/Apache-2). Vendoring +
  pinning insulates against upstream relicensing (Charm/Bubble Tea v2
  included — today's MIT copy is ours regardless of their future). Any
  load-bearing dep must be small enough to fork and carry if it comes to
  that.
- **No telemetry.** tide makes no network calls except those the user's own
  commands make. Local-first, always.
- **Socket security.** Daemon socket is user-private (0700 dir, owner-only).
  No network listeners, ever — remote is SSH-in-and-attach by design.

## Reference implementations & validation

- Submodule license-compatible references under `reference/` — for study and
  test extraction only, never linked into the build: **Zellij** (MIT; VT/grid
  behavior, pane semantics, test cases), **alacritty / vte** (Apache-2.0; the
  canonical VT parser state machine), **vt10x** (MIT; existing Go VT as
  porting baseline).
- Port upstream unit tests where applicable; validate the Go VT against
  conformance suites (vttest, esctest) scoped to what tide panes must
  actually host.
- Ported logic retains upstream attribution and license notices.

## Architecture decisions (ratified)

- **Central daemon, tmux-style.** One daemon owns all sessions. `tide ls`
  asks the daemon; session identity remains the project root path.
- **Consequence → daemon state serialization.** Because all sessions share
  one process, the daemon continuously serializes session state to disk.
  Guarantee tiers: client/UI/terminal death → exact, mid-keystroke recovery
  (requirement 1); daemon death → recovery to last checkpoint, sessions
  never lost.
- **On-demand spawn, single binary.** No service manager required. `tide`
  resolves project root → connects to the daemon socket → attaches. No
  daemon: the binary re-execs itself detached as the daemon, then attaches.
  Stale socket: remove, spawn, retry. Spawn races resolved by first-to-bind
  wins.
- **Version handshake first.** First protocol message both ways:
  `hello{binary_version, protocol_version}`. Protocol match → attach.
  Mismatch → never kill implicitly; user chooses (attach read-only /
  restart session).
- **Prime rule:** no code path may destroy a session as a side effect.
  Sessions end only by `tide kill` or explicit user choice.
- **Tool render path → pane VT** *(ruled 2026-06-10)*. Tide-family tools are
  ordinary TUIs rendering through the pane's VT. Integration is purely via the
  injected `TIDE_SESSION` (session uuid + socket): tools read it and speak to
  the daemon. VT scope therefore includes hosting tide-family tools.
- **Multi-client attach → yes in v1** *(ruled 2026-06-10)*. Multiple terminals
  may attach to one session simultaneously; the wire protocol must support
  fan-out to N clients.
- **Session detach UI** *(ruled 2026-06-10)*. A persistent bar (top or
  bottom, placement TBD) carries a '-' minimize-style button that detaches:
  the client exits, the session keeps running. Keyboard accelerator
  Ctrl+Shift+E. Typing `tide` in a native terminal again reattaches (standard
  invocation behavior). Sessions are killed only via `tide kill` (prime
  rule).
- **Version mismatch → `tide restart`** *(ruled 2026-06-10)*. No read-only
  attach in v1. On protocol mismatch the client prompts the user to run
  `tide restart`; `tide restart` warns that sessions will be shut down before
  proceeding.
- **Ctrl+C in terminal panes → selection-aware** *(ruled 2026-06-10)*.
  Selection active → copy and clear the selection; no selection → 0x03 to the
  pty (the inner tty's line discipline turns it into SIGINT). A second Ctrl+C
  therefore always interrupts. Precedent: Windows Terminal's default,
  JetBrains/JediTerm, VS Code on Windows. Guardrails the prior art proved
  load-bearing:
  - Any keystroke that sends input to the pane clears the selection first
    (xterm.js / Windows Terminal behavior) — a stale selection can only exist
    between mouse-up and the very next key.
  - Selections in terminal panes are transient drag-selections only — never
    persistent or structural (JetBrains' block-terminal rewrite made
    click-a-block count as selection and broke interrupt; IJPL-102573).
  - On copy, flash feedback ("Copied — Ctrl+C again to interrupt") in the
    pane chrome — discoverability, requirement 5.
  - Ctrl+V → paste, bracketed (DECSET 2004), with paste guards: confirm on
    multi-line or embedded control codes (Windows Terminal / kitty precedent).
  - Two Windows Terminal mistakes not to repeat: Enter is never a copy key;
    if Ctrl+Shift+C ships as an alias it is a no-op without a selection — it
    must not fall through and reach the shell as a control sequence (WT
    issue #10253 kills processes that way).
  - Linux: mouse selection still feeds PRIMARY (middle-click paste) per
    platform convention; CLIPBOARD is written only by explicit copy.
    Copy-on-select to CLIPBOARD stays off.
  - macOS: the collision does not exist — CUA there is Cmd-based (Cmd+C/V
    copy/paste; Ctrl+C always SIGINT). "One control scheme" means the
    platform's CUA convention. *Amended 2026-06-12: this assumed Cmd+C
    reaches the app; it never does — every macOS terminal handles ⌘
    shortcuts itself (Terminal.app copies its own native selection, which
    is empty while tide owns mouse reporting, and beeps). Since no copy
    chord can be Mac-CUA, the Mac behavior is copy-on-select: the
    release-time primary feed maps to the system clipboard on darwin
    (there is no PRIMARY to collide with) — select, then ⌘V anywhere.
    Selection-aware Ctrl+C still copies; bare Ctrl+C is still SIGINT.
    Linux is unchanged: PRIMARY on select, CLIPBOARD only on explicit
    copy.*

- **Session bar → top** *(ruled 2026-06-10)*. The persistent bar sits at the
  top of the screen (Zed/browser tab-bar model); tabs join it as they land.
- **Daemon exits with its last session** *(ruled 2026-06-10)*. Killing the
  last session ends the daemon process (tmux behavior, minimal footprint);
  on-demand spawn makes the next `tide` self-healing.
- **Clipboard model** *(ruled 2026-06-10, implementation consequence of the
  Ctrl+C ruling; amended 2026-06-11)*. Copy writes both a daemon-side
  internal clipboard and the client's system clipboard; mouse selection
  additionally feeds PRIMARY on Linux. Ctrl+V pastes the internal clipboard
  (portable without OSC 52 read permission); terminal-native paste arrives
  as bracketed input and passes through the same guards. *Amendment*: the
  system clipboard is fed two ways, because OSC 52 alone proved
  insufficient — Terminal.app discards it entirely and VTE gained it late.
  The daemon emits OSC 52 on the render stream (covers SSH attaches in
  capable terminals) **and** a `copy` protocol frame the client pipes into
  the platform tool when installed: `pbcopy` on macOS; `wl-copy`,
  `xclip`, or `xsel` on Linux (Wayland tool first when `WAYLAND_DISPLAY`
  is set). No tool found degrades to OSC 52 only. The client stays glass
  on the render path — `copy` is a typed frame, not output parsing.

- **Pane frames and the two-menu model** *(ruled 2026-06-11)*. Supersedes
  right-click-only pane chrome: macOS Terminal.app never forwards right
  clicks through mouse reporting, so no action may require one. Every pane
  is framed Zellij-style from the start (not just when split); shared
  edges render once — between stacked panes the lower pane's top bar IS
  the divider. The pane's top border is its bar: title on the left, a
  dedicated `[≡]` pane-menu button on the right (Copy, Paste, Restart
  Shell, Close Pane). Frame edges disambiguate by gesture: press+drag
  resizes (where a neighbor exists), press+release in place opens the
  layout menu (Split Right/Down), which names its target pane. A shared
  vertical border belongs to the pane on its left; a bar-divider row to
  the pane below; outer edges to their adjacent pane. Right-click stays
  as a pane-menu accelerator where terminals forward it. The session
  bar's project-name segment opens the session menu (New Tab, Detach,
  Kill Session…); '-' quick-detach and '+' stay. Focus is indicated by
  frame styling.
- **Boundary semantics and hover** *(ruled 2026-06-11, refinement;
  superseded 2026-06-19 by "Window-centric edge splits")*.
  Frame elements are boundaries of their CONTAINER, not handles of a
  neighboring pane, and every border offers all four directions: the
  cross-axis pair acts at the container level (from the divider between
  stacked panes, "new pane right" spans the whole stack's height), while
  the along-axis pair inserts at the clicked boundary, taking its space
  from the named neighbor ("new pane down — from the pane below"). Corners merge both meeting borders (left/right act at
  the horizontal divider's container, up/down at the vertical border's);
  corner drags resize both axes. A bar that is not a divider is its
  container's top edge ("above", full width — climbing stacked runs the
  pane leads); the outer ring is the root's boundary. Pane-level splits
  (halve THIS pane) live in the `[≡]` pane menu. On terminals reporting
  bare motion (mode 1003; most, but not stock macOS Terminal.app), the
  boundary under the pointer highlights — corners light every border they
  join, previewing what the gesture affects; elsewhere the chrome degrades
  to no highlight, never to broken clicks.
- **Window-centric edge splits — i3-style** *(ruled 2026-06-19;
  supersedes "Boundary semantics and hover")*. Frame elements are the
  EDGES OF THE ADJACENT WINDOW, not boundaries of a container — the model
  is i3's: pick a window, pick a side, split it that way. Every window has
  four reachable edges: its top bar is the top edge; a shared vertical
  border is the right edge of the pane on its left; the outer ring is
  SEGMENTED per window, so the bottom strip under the right pane is that
  pane's own bottom edge (not the whole-width ring). Clicking an edge
  (press+release in place) opens a compact directional menu scoped to that
  one window — the clicked side's direction first (the default), then the
  two perpendicular directions ("↓ New pane below", "← New pane left", …);
  the chosen item runs `Layout.Split(pane, dir)` (i3 semantics: same-axis
  joins the run, perpendicular nests a container). Press+drag still resizes
  where a neighbor exists. Corners (a vertical border meeting a horizontal
  divider) are 2-axis resize handles: hover AND press light both strokes;
  they no longer open a split menu. The five container-level boundary menus
  ("full height/width", "from the pane above/below") are gone, as are the
  `[≡]` menu's four Split items — splitting is spatial now, the `[≡]` menu
  keeps Copy/Paste/Restart/Close. The mouse analog of i3's window MOVE
  (drag a bar to re-drop it in the tree) is the next increment.

- **`teddy` — the editor tool** *(ruled 2026-06-17; working name was
  `tide-edit`)*. A standalone terminal editor binary, a product in its own
  right; tide-integration is an enhancement layer keyed on `TIDE_SESSION`,
  never a requirement to run. Layout (VS Code lineage): a far-left activity
  bar (Browser now; Search/Git present but inert until built), a collapsible
  side panel showing the selected activity, an editor tab strip, and a
  single editor/viewer area — **no tiling inside teddy; tide owns
  splitting** (panes target tide-family tools). Theme and input reuse
  tide's: the 16-color palette and the `internal/input` decoder, so teddy
  speaks one input dialect with tide.
  - **Editing**: a minimal real editor — open, edit, Ctrl+S save,
    undo/redo. LSP and project-wide search & replace are deferred (still
    teddy's, later).
  - **Syntax highlighting**: from the start, via vendored **chroma**
    (pure-Go, MIT) used as a lexer only — teddy maps chroma tokens onto its
    own 16-color styles, never chroma's formatter, so highlighting obeys
    tide's palette.
  - **Markdown**: a `raw ↔ viz` toggle per `.md` buffer; viz renders with
    the terminal's own palette (no truecolor), matching the theme.
  - **Tabs are draggable, labeled by file path.** Standalone: drag reorders
    within teddy's own strip. In a tide session: the drag is **delivered
    through tide** — the first bidirectional tool↔daemon surface.
- **Cross-tide tab drag** *(ruled 2026-06-17; built in the second teddy
  increment)*. When a tab-drag begins in a tide session, teddy hands the
  daemon a payload (`{file, path}`); the daemon owns the drag from there —
  it alone can draw across pane edges — rendering a ghost on the
  compositor's floating layer (sibling of the overlay) and routing the drop:
  - drop back in teddy's own strip → reorder;
  - drop on a tide boundary / empty area → **tear-off**: the file opens in a
    new pane running teddy (needs the new daemon capability
    *spawn-pane-with-command*; today panes only spawn `$SHELL`);
  - drop on another teddy pane → that teddy opens the file;
  - drop on a terminal pane → the file path is typed into that shell.
  New protocol surface (designed; the coherence boundary gains its first
  tool-facing bytes): `tool_attach` (a pane announces it is teddy and what
  it can open), `drag_start`/`drag_drop`, and `open{path}`. Clipboard and
  keymap fetch are designed-for but deferred; teddy mirrors tide's CUA
  defaults locally until then.
- **teddy delivery phasing** *(ruled 2026-06-17)*. **T1** ships teddy
  standalone (chrome, file browser, tabs with in-strip reorder, minimal
  editing, chroma highlighting, markdown viz) — a UX-exploration increment:
  felt before its deep adversarial audit, per the established pattern. **T2**
  adds the cross-tide drag and its protocol surface once T1's feel is
  validated. Unsaved-buffer crash survival (the full-product acceptance
  test) is deferred to a later teddy increment: teddy will self-checkpoint
  dirty buffers, mirroring the daemon.

## Open rulings

None — all v1 rulings are ratified above.

## Out of scope (v1)
- General-purpose VT host (arbitrary third-party full-screen apps,
  vim/htop-class); panes target shells, build output, and tide-family tools
- Multi-workspace UI
- Local-client-to-remote-daemon networking (v2; v1 remote = SSH in, attach)
- Plugin system
- Windows

## Acceptance tests

**Phase 1 (terminal environment):** open project, start a build in a pane,
kill the terminal emulator / GNOME session outright. Reattach from a new
terminal: build output intact, layout intact, mid-keystroke.

**Full product:** open project, edit a file (unsaved), start a build in a
terminal pane, stage a hunk in the git view. Kill the terminal / GUI session.
Reattach: unsaved edit present, build output intact, git state where it was.
