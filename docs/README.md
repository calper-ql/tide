# tide docs

- **[DESIGN.md](DESIGN.md)** — how tide is built, as it stands: the daemon/client split, the
  per-pane terminal, sessions and persistence, the input and clipboard model, the mouse-first
  split UI, themes, remote attach, teddy, the wire protocol, and the security model.
- **[security-input-guard.md](security-input-guard.md)** — the guard that keeps a password you
  were typing from leaking to your shell history when a connection drops.
- **images/** — screenshots used by the top-level README.

New here? Start with the [top-level README](../README.md) for what tide is, install, and
usage, then read [DESIGN.md](DESIGN.md) for the design.

The mental model in three lines:

- A **daemon** owns every byte on screen (your shells, scrollback, layout, clipboard); a
  **client** is pure glass — raw input up, composed frames down.
- So a client can die anytime and lose nothing: detach and reattach, locally or over SSH,
  exactly where you left off.
- Every pane is framed and everything is clickable — the compositor records a hitmap as it
  draws, and the daemon's one router turns clicks and keys into actions.
