# Security: the abandoned-input guard

*Threat model and invariants for tide's handling of a connection that drops
while the user is typing.*

## The problem

When you attach a remote session with `tide -r user@host`, the interactive
client runs on your **local** machine and bridges to the host's daemon over
ssh. Your keystrokes are forwarded raw to the remote; the remote decides
whether to echo them. That means you routinely type **blind** into a remote
prompt that has turned echo off — an ssh/sudo password, a `read -s`, a 2FA
code.

If the connection drops at that moment, the naive behavior is catastrophic:

1. tide notices the stream is gone, restores the terminal to normal (cooked)
   mode, and exits back to your **local** shell.
2. You don't necessarily notice — a no-echo prompt gives no visual feedback,
   so you keep typing the password and press Enter.
3. Your local shell reads `hunter2` as a command line, fails to run it, and
   records it in `~/.bash_history` / `~/.zsh_history` **in plaintext**.

The secret you typed into a remote host is now sitting in your local shell
history. This is the leak the guard exists to prevent.

The same shape occurs during connection setup: ssh reads a password straight
from `/dev/tty` with echo off. If the handshake stalls (a slow 2FA push) and
tide has to kill ssh, or ssh dies for any other reason mid-prompt, a
half-typed password can be left queued in the terminal or continue to be typed
into the returning shell.

## The invariant

> From the moment a connection is lost until tide hands the terminal back to
> the shell, **tide — not the shell — owns the terminal input**, and every
> keystroke read during that window is discarded, up to and including the Enter
> that would have submitted the secret.

While tide holds the tty fd and keeps reading it, the shell physically cannot
read those bytes. That is the whole mechanism.

## How it works

### Mid-session drop (`streamSession`, `cmd/tide/main.go`)

`streamSession` owns stdin for the entire attach via a single "stdin pump"
goroutine (`stdinPump`). While connected it forwards keystrokes to the daemon.
The instant the connection is lost — a `Recv` error on the output goroutine, or
a `SendInput` error on the pump — an atomic `lost` flag latches and the pump
switches from **forwarding** to **guarding**: it keeps reading and throws every
byte away until it sees Enter (CR/LF), via `drainUntilEnter`.

Meanwhile the main loop:

1. closes the connection and waits for the output goroutine to stop writing the
   screen,
2. leaves the alt screen (so the notice lands on the shell's screen) but keeps
   the terminal **raw** — no echo, because the bytes may be a secret,
3. prints `connLostNotice` ("connection lost … press Enter to continue"),
4. blocks until the pump releases (the user pressed Enter), then restores
   cooked mode.

Because the pump owns the fd the whole time, nothing the user types between the
drop and that Enter can reach the shell. Whatever preceded the Enter — the
secret — is read by tide and dropped.

### Handshake / auth failure (`remoteAttach`, `cmd/tide/remote.go`)

Before spawning ssh, tide snapshots the terminal's pristine (cooked, echo-on)
state with `term.GetState`. If `ClientHandshake` fails:

- **Timeout** (`os.ErrDeadlineExceeded`) — ssh was still alive, most likely
  stalled on an interactive auth prompt the user is answering. tide kills ssh
  and runs `guardAbandonedInput`: raw mode, a notice, discard until Enter,
  then restore pristine. Same guarantee as the mid-session path.
- **Any other error** — ssh exited on its own (auth refused, `tide` missing on
  the host, remote closed). A partial password can still sit queued in the tty,
  so tide runs `flushTTYInput`: it briefly makes the tty non-canonical (so a
  newline-less partial line becomes readable), non-blocking-drains the input
  queue, and restores pristine. No forced Enter, because these failures are
  visible and usually involve no secret.

The handshake deadline (`remoteHandshakeTimeout`, 60s) is deliberately generous
so legitimate slow auth (password typing, a 2FA push approved on a phone) is
**not** guillotined mid-entry — killing ssh while it holds a half-read password
is itself the dangerous case.

## Deliberate decisions

- **Indefinite hold.** After a drop the guard waits for Enter with no timeout.
  If the user walks away, tide keeps holding the terminal rather than restoring
  it — restoring early would let a later keystroke leak. Pressing Enter (which
  is also what submits a password) always releases it cleanly.
- **SIGTERM/SIGHUP are not serviced during the hold.** While blocked waiting
  for Enter, tide is out of its signal-select loop, so a bare `kill <pid>` or
  `kill -HUP` is buffered and ignored; only Enter, the terminal actually
  closing (stdin EOF, which self-heals), or `SIGKILL` releases the hold. This
  is intentional: honoring a signal there would return through the normal
  teardown, restore cooked mode, and let any still-queued secret fall through
  to the shell — the exact leak this guard prevents.
- **Applies to local attach too.** The same `streamSession` path backs a plain
  local `tide` attach, so a daemon that vanishes mid-`sudo` is guarded
  identically.

## Tests

- `cmd/tide/guard_test.go` — unit tests for `drainUntilEnter` and the
  `stdinPump` forward→guard state machine (a secret is never forwarded after a
  drop; the guard stops at the first Enter and leaves the next line for the
  shell).
- `cmd/tide/guard_integration_test.go` — drives the real `streamSession` over a
  PTY: on a drop it paints the notice, holds the terminal, swallows a password
  the "user" keeps typing, and releases only on Enter.
