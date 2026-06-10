# tide — current state

*Living document; update at the end of each increment.*
*Last updated 2026-06-10 (HEAD `580b3bc`). Product contract: [tide-spec-v1.md](tide-spec-v1.md).*

## Where we are

Phase 1 (the terminal environment) is roughly half done. The **session
substrate is complete and passes the spec's Phase-1 acceptance test**: open a
project, start a build in the pane, kill the terminal outright, reattach from
a new terminal — build output intact, half-typed command still on the line,
mid-keystroke. What's left of Phase 1 is the interaction layer: layout,
mouse-first chrome, and the single keymap.

| Foundation (spec §Core foundation)        | Status |
|-------------------------------------------|--------|
| §1 Session daemon                          | **Done** |
| §2 Attach protocol                         | **Done** (will grow with the tool render path) |
| §3 Terminal pane (scoped VT)               | **Done** — one full-screen pane per session |
| §4 Pane/tab layout + input routing         | Not started — next increment |
| Tabs/tiles, mouse-first chrome, session bar | Not started |
| Single keymap incl. Ctrl+C ruling          | Not started (lands with input routing) |
| `tide-edit`, `tide-git`                    | Phase 2 |

## Architecture as built

```
cmd/tide            CLI: attach/ls/kill/restart, raw-mode client, --daemon entry
internal/client     dial, on-demand daemon spawn, seq-correlated rpc, stream helpers
internal/protocol   the wire contract (JSON-lines over unix socket); tools target this
internal/daemon     daemon: locks, serve loop, session/pane orchestration
internal/daemon/pane.go  PTY + VT + client fan-out + checkpoints, per session
internal/vt         vt10x port + tide extensions (snapshot renderer, scrollback, wide glyphs)
internal/session    registry + on-disk checkpoints (identity file + per-pane files)
internal/project    root resolution (.git walk, worktrees, symlinks, --here)
internal/paths      runtime/state dirs, 0700/owner-verified
internal/version    binary + protocol version pins
```

**Daemon.** Central, tmux-style, one per user. `tide` re-execs itself as
`--daemon` when no one is listening; spawn races are settled by an
inode-verified flock (tmp cleaners unlinking lock files cannot produce two
daemons), and a second flock on the state file guarantees a single
checkpoint writer even if runtime dirs diverge. The runtime dir is a fixed
per-uid path (`$XDG_RUNTIME_DIR/tide` or `/tmp/tide-<uid>`), never `$TMPDIR`,
so GUI and SSH logins share one daemon. Shutdown is explicit (protocol
request) or SIGTERM — the version-independent path `tide restart` uses
against a protocol-mismatched daemon, found via the pid in the lock file
behind a flock liveness probe.

**Sessions.** Identity = canonical project root. Created on attach, ended
only by `tide kill` (the prime rule has exactly one removal code path).
Client death of any kind is a detach. Kill resolution tries exact session
roots before the `.git` walk, so `--here` sessions are always reachable.

**Pane.** One full-screen pane per session: the user's shell on a PTY,
output parsed into a daemon-side VT grid and fanned out to every attached
client through per-client outboxes (slow clients are evicted with a typed
`dropped` notice rather than stalling the pane). Shell death is detected by
a Wait-owning reaper — not PTY EIO, so background jobs holding the slave
can't hide an exit — and the shell respawns on the next attach. Input flows
through a bounded queue so a non-reading foreground app can never wedge a
connection. `TIDE_SESSION=<pane-uuid>:<socket-path>` is injected into every
pane shell with scrubbed terminal context and `TERM=xterm-256color`
(spec: capability model).

**VT (`internal/vt`).** Ported from vt10x (MIT, attribution retained),
extended with what the crash-survival promise needs:

- A **snapshot renderer** serializing the complete terminal state — grid,
  scrollback, cursor, pen, charset, modes, scroll region, saved-cursor state
  bits, wrap-pending, palette overrides, and any in-flight (partially
  received) escape sequence or split UTF-8 rune — as an ANSI stream that
  recreates the state exactly. The roundtrip property (snapshot → fresh
  terminal → identical continuation) is pinned by tests across ~30
  scenarios, including snapshots taken mid-escape-sequence.
- The reset prefix uses explicit sequences, **never RIS** (VTE-family
  terminals wipe scrollback on RIS, which would both defeat the history
  replay and destroy the user's own scrollback).
- A scrollback ring fed by full-screen scrolls *and* by rows discarded when
  a smaller client resizes the pane — shrinking narrows the view, never
  destroys content.
- Double-width glyphs (CJK/emoji) modeled as lead+dummy cell pairs with
  torn-pair repair on every mutation path.
- Upstream vt10x bugs fixed in the port: `?1049l` on the main screen no
  longer swaps into the alt screen; OSC 11 default-background overrides no
  longer rejected; bracketed paste (DECSET 2004) tracked.

**Protocol.** Hello-first version handshake both ways; mismatch never kills
anything — the client is pointed at `tide restart`. Requests carry a `seq`
echoed in replies; stream frames (`input`, `resize`, `output`, `exit`,
`killed`, `dropped`) interleave freely on attached connections and rpc
skips them by seq-mismatch. The attach reply travels through the client's
outbox as its first frame, so no stream frame can ever precede it on the
wire. All client RPCs and the handshake are deadline-bounded.

**Persistence.** `sessions.json` holds identities only; each pane
checkpoints its content (screen snapshot + rendered scrollback, debounced
to once per second) into its own `pane-<hash>.json`, so a busy pane never
rewrites other sessions' data. All writes are temp+fsync+rename atomic.
Corrupt state files are quarantined aside with a loud log line, never
allowed to brick the daemon. Stale checkpoints from killed/replaced panes
are rejected, so a dying pane cannot resurrect content the user ended.

**Crash-survival guarantees as implemented:**
- *Client/terminal/GUI death* → exact recovery, mid-keystroke
  (`TestAcceptanceCrashSurvival` pins the spec's acceptance scenario).
- *Daemon death* → recovery to the last checkpoint: content and scrollback
  restored into a fresh pane with a notice, shell restarted on attach;
  sessions never lost.

**Security posture.** Runtime dir 0700 with ownership re-assertion; socket
and state files 0600; roots validated as absolute clean paths at the
protocol boundary; pane sizes clamped; no network listeners; no telemetry.

## Development

- `./cli.sh {build,test,check,ci,shell}` — pinned Docker toolchain
  (`golang:1.26-bookworm`); build/test containers run `--network=none`,
  re-proving the spec's offline-build constraint on every run.
- 41 tests, `-race` clean, including PTY integration tests against real
  shells and the acceptance test.
- Dependencies vendored and pinned: `creack/pty`, `golang.org/x/term`,
  `mattn/go-runewidth` (all permissive).
- `reference/vt10x` submodule: porting baseline, study/test extraction only.

## Known v0 placeholders and gaps (all flagged in code)

- **Detach key is Ctrl+\\** until the session bar ('-' button, Ctrl+Shift+E)
  lands; it is the one key the client steals from the pane, and it fires on
  0x1c inside pasted content.
- **Resize is latest-wins** across multiple attached clients of different
  sizes (tmux uses min-of-clients); discarded rows go to scrollback.
- The alt-screen path cannot carry saved-cursor origin/wrap state bits
  through `?1049h` (documented parser-level gap, exotic).
- Output framing is JSON + base64 (~33% inflation); acceptable now,
  revisit before the protocol freezes for tools.
- Open rulings from the spec: session-bar placement (top vs bottom) and
  daemon lifecycle after the last session is killed.

## Next increment (Phase 1 remainder)

Layout and input routing (foundation §4): pane/tab layout, the shared
mouse/keyboard routing layer, clickable chrome, the session bar (retires
the detach placeholder), and the single CUA keymap — where the ratified
selection-aware Ctrl+C ruling (see [research](research-ctrl-c-prior-art.md))
gets implemented.
