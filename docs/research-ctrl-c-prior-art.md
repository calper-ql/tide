# Ctrl+C in terminal panes — prior-art research

*Compiled 2026-06-10 by a four-agent research workflow against official docs,
source code, and issue trackers; anything weaker is flagged in the finding's
caveats. Evidence base for the "Ctrl+C in terminal panes → selection-aware"
ruling in [tide-spec-v1.md](tide-spec-v1.md).*

## Ruling this supports

Selection active → copy and clear the selection; no selection → 0x03 to the
pty. A second Ctrl+C therefore always interrupts. The full guardrail list
lives in the spec's ratified decisions.

## Cross-cutting takeaways

- Exactly one resolution has shipped at scale: selection-conditional Ctrl+C —
  Windows conhost (2014), Windows Terminal (default), JetBrains/JediTerm,
  VS Code on Windows; kitty (`copy_or_interrupt`) and Zed offer it opt-in.
  KDE's Konsole has the same design sitting in a never-merged MR (!520).
- It works because of two clearing rules: copy clears the selection, and any
  keystroke that sends input clears the selection — so a stale selection can
  only exist between mouse-up and the very next key.
- The one production failure was structural selection (JetBrains' block
  terminal, IJPL-102573): clicking a block counted as "selection" and ate the
  interrupt. Terminal-pane selections must stay transient drag-selections.
- Every Linux-native product keeps plain Ctrl+C = SIGINT by default and puts
  copy on Ctrl+Shift+C; only Windows Terminal ships CUA Ctrl+C as a default.
- On macOS the collision does not exist: CUA is Cmd-based (Cmd+C/V), Ctrl+C
  is always SIGINT, and iTerm2 even defaults to copy-on-select safely.
- The refusers (mintty's `ShootFoot` option, PuTTY's FAQ) argue a terminal
  must deliver every control byte to arbitrary inner apps — an argument
  tide's pane scope (shells, build output, tide-family tools; no
  vim/htop-class hosts) deliberately weakens.
- Mechanically, SIGINT is made by the inner pty's line discipline (ISIG), not
  the keyboard: a host that swallows 0x03 starves cooked-mode shells of their
  interrupt and raw-mode TUIs of the byte they handle themselves.
- Windows Terminal mistakes to avoid: Enter bound to copy-when-selected, and
  a Ctrl+Shift+C that falls through with no selection and reaches the shell
  encoded as ^C, killing the foreground process (WT #10253).

## Windows Terminal & VS Code — the shipping context-sensitive defaults

**Section summary.** Both products converge on the same resolution, relevant to tide's CUA-keybinding spec: make Ctrl+C conditional on selection state, with fall-through to the shell. Windows Terminal binds Ctrl+C (plus Ctrl+Shift+C, Ctrl+Insert, and Enter) to a copy action whose documented contract is "if no selection exists, the key chord is sent directly to the terminal," copy dismisses the selection so a repeat press interrupts, copyOnSelect defaults to false, and any non-modifier keystroke clears a selection (ControlCore::TrySendKeyEvent). VS Code does the same only on Windows via a dedicated copyAndClearSelection command gated on a textSelectedInFocused when-clause, while Linux keeps Ctrl+C as pure SIGINT (copy = Ctrl+Shift+C) and macOS uses Cmd+C/Cmd+V; xterm.js clears the selection on any user input, and sendKeybindingsToShell (default false) / allowChords (default true) / commandsToSkipShell govern which chords ever reach the shell. The verified real-world failure modes are: stale/fresh selections silently turning Ctrl+C into copy (vscode#166697 — fixed by clearing selection on copy; duplicates #207915/#184837), the inverse surprise where a "copy-only" Ctrl+Shift+C falls through with no selection and kills the running program (terminal#10253, still open), and paste bindings hijacking Ctrl+V from TUI apps like vim with no per-profile opt-out (terminal#11267, #5790, both open). A 2026 regression (vscode#295372, CSI-u "[99;5u" leaking instead of SIGINT) shows the interrupt path can also break at the keyboard-protocol-encoding layer. For tide: bind Ctrl+C to copy-and-clear-selection only when a selection exists, fall through to 0x03 otherwise, clear selections on any input keystroke, keep copyOnSelect opt-in, and decide explicitly what an unselected Ctrl+Shift+C should encode.

### Windows Terminal — default Ctrl+C/Ctrl+V bindings and fall-through

The `copy` action (id Terminal.CopyToClipboard, default args {"singleLine": false}) is bound by default to ctrl+c, ctrl+shift+c, ctrl+insert, AND enter. The `paste` action (Terminal.PasteFromClipboard) is bound to ctrl+v, ctrl+shift+v, and shift+insert. The documented copy semantics are: "This copies the selected terminal content to your clipboard. If no selection exists, the key chord is sent directly to the terminal." So Ctrl+C with a selection = copy; Ctrl+C without a selection = raw 0x03 sent to the conpty/shell (SIGINT). The fall-through-on-no-selection design is the resolution of the original conflict reported in issue #2210 ("Binding Ctrl-C to copy prevents using Ctrl-C for cancelling a command", 2019); the bindings became default via issue #3058, closed by PR #5217. Copying also dismisses the selection, so a second Ctrl+C interrupts (confirmed indirectly by issue #3884, "Option to not clear selection on copy", which requests an opt-out of this clearing; copy has no parameter to retain the selection — only singleLine, withControlSequences, copyFormatting).

*Caveats:* Versions before the fall-through fix really did break SIGINT entirely (#2210/#2285). The Enter-bound-to-copy default (conhost parity, issues #4839 and #9177) means pressing Enter with a selection copies and does NOT send Enter to the shell — a surprise of its own. Exact release in which the defaults shipped (~v1.0, spring 2020 via PR #5217) was not precisely verified.

*Sources:* https://learn.microsoft.com/en-us/windows/terminal/customize-settings/actions ; https://github.com/microsoft/terminal/issues/2210 ; https://github.com/microsoft/terminal/issues/3058 ; https://github.com/microsoft/terminal/issues/3884 ; https://github.com/microsoft/terminal/issues/9177

### Windows Terminal — copyOnSelect and selection clearing

`copyOnSelect` (global setting, default false): when true, "a selection is immediately copied to your clipboard upon creation" and right-click always pastes; when false, "the selection persists and awaits further action" and right-click copies the selection. Selection clearing on keyboard input is in ControlCore::TrySendKeyEvent (verified from main-branch source): on key-down, if the key isn't a mark-mode selection-update key, the terminal calls _terminal->ClearSelection() before sending input — except when the Windows key is part of the combination (comment: "GH#8791 - don't dismiss selection if Windows key was also pressed as a key-combination"). Bound key chords (like Ctrl+C→copy) are intercepted at the keybinding layer before TrySendKeyEvent, so they don't hit this path; copy clears the selection itself after copying. Net effect: any ordinary keystroke that produces input clears the selection, so stale selections mostly arise only when Ctrl+C is the very next key after a mouse selection.

*Caveats:* That a fresh mouse click also replaces/clears the selection is standard behavior but I did not verify it from source — mark as assumed. Mark mode (keyboard selection, v1.16+) adds more keys that update rather than clear the selection.

*Sources:* https://learn.microsoft.com/en-us/windows/terminal/customize-settings/interaction ; https://github.com/microsoft/terminal/blob/main/src/cascadia/TerminalControl/ControlCore.cpp (TrySendKeyEvent)

### Windows Terminal — real-world failure modes from the issue tracker

Documented user complaints about the collision: (1) Issue #10253 (OPEN, 2021): "Shift-ctrl-C sends interrupt if no text is selected" — because copy falls through when there is no selection, Ctrl+Shift+C is forwarded to the terminal, which encodes it as ^C and kills the running program; the user expected a copy-only chord to be a no-op. (2) Issue #11267 (OPEN): "Vim.exe can disable ctrl+v to paste in conhost, but not in Windows Terminal" — the default Ctrl+V→paste binding hijacks vim's visual-block Ctrl+V with no per-app escape hatch. (3) Issue #5790 (OPEN): users want to suppress the default Ctrl+C/Ctrl+V mappings only for WSL profiles; keybindings are global, not per-profile. (4) Issues #1837/#5316/#10687: discoverability/reliability complaints about copy chords (#10687: ctrl+shift+c bound to copy 'intermittently' sends SIGINT instead).

*Caveats:* I did not find an open WT issue specifically about a *stale* selection making Ctrl+C copy instead of interrupt — plausibly because any intervening keystroke clears the selection, narrowing the window; absence of such an issue is not proof it never happens.

*Sources:* https://github.com/microsoft/terminal/issues/10253 ; https://github.com/microsoft/terminal/issues/11267 ; https://github.com/microsoft/terminal/issues/5790 ; https://github.com/microsoft/terminal/issues/10687

### VS Code integrated terminal — per-platform copy/paste defaults (verified from source)

From src/vs/workbench/contrib/terminalContrib/clipboard/browser/terminal.clipboard.contribution.ts: (1) workbench.action.terminal.copySelection — primary Ctrl+Shift+C (Win/Linux), mac override Cmd+C; when-clause requires (terminal textSelected AND terminal focus) OR textSelectedInFocused; copies and RETAINS the selection. (2) workbench.action.terminal.copyAndClearSelection — bound ONLY on Windows: win primary Ctrl+C, same selection-required when-clause; no mac/linux binding. (3) workbench.action.terminal.paste — primary Ctrl/Cmd+V; Windows secondary Ctrl+Shift+V; Linux primary Ctrl+Shift+V; (4) pasteSelection — Linux Shift+Insert. Net per-platform behavior: WINDOWS — Ctrl+C with selection copies and clears the selection (so the next Ctrl+C reaches the shell as SIGINT); Ctrl+C without selection goes to the shell; Ctrl+V pastes. LINUX — Ctrl+C ALWAYS goes to the shell (copy is Ctrl+Shift+C only); plain Ctrl+V goes to the shell as 0x16; paste is Ctrl+Shift+V. MACOS — Ctrl+C always goes to the shell; Cmd+C copies when a selection exists; Cmd+V pastes. Selection-state hygiene comes from xterm.js: SelectionService registers this._coreService.onUserInput(() => { if (this.hasSelection) { this.clearSelection(); } }) — any user keystroke that sends data clears the selection (verified verbatim from xterm.js master).

*Caveats:* Because the Ctrl+C copy binding is gated on a when-clause (textSelectedInFocused), a mouse selection made moments earlier still redirects the very next Ctrl+C to copy instead of SIGINT on Windows — the clear-on-typing only narrows, not eliminates, the stale-selection window. macOS Cmd+C without a selection simply doesn't match the when-clause (no copy, no signal).

*Sources:* https://github.com/microsoft/vscode/blob/main/src/vs/workbench/contrib/terminalContrib/clipboard/browser/terminal.clipboard.contribution.ts ; https://github.com/xtermjs/xterm.js/blob/master/src/browser/services/SelectionService.ts ; https://code.visualstudio.com/docs/terminal/basics

### VS Code integrated terminal — keybinding-vs-shell settings

Mechanism (Terminal Advanced docs + terminalConfiguration.ts source): when the terminal has focus, only keybindings whose commands are on the commandsToSkipShell allowlist are handled by the workbench; everything else is sent to the shell. terminal.integrated.commandsToSkipShell — user value defaults to [] but is merged with a hard-coded default list of commands "integral to the VS Code experience" (the terminal copy/paste commands are intercepted this way); a '-' prefix removes a default entry, e.g. "-workbench.action.quickOpen" sends that chord to the shell. terminal.integrated.sendKeybindingsToShell — default false; when true it overrides commandsToSkipShell and dispatches most keybindings to the shell (docs warn this breaks shortcuts like Ctrl+F find). terminal.integrated.allowChords — default true: chord keybindings (e.g. Ctrl+K Ctrl+C) always skip the shell unless this is disabled. terminal.integrated.copyOnSelection — default false. terminal.integrated.rightClickBehavior — defaults: 'selectWord' on macOS, 'copyPaste' on Windows (copy if selection exists, else paste), 'default' (context menu) on Linux. terminal.integrated.enableMultiLinePasteWarning — default 'auto' (warn only when the shell lacks bracketed-paste mode).

*Caveats:* On macOS, Cmd+K is taken by 'clear terminal', so Cmd+K-prefixed chords don't reach the terminal until that binding is removed — an example of chord interception fighting shell usage.

*Sources:* https://code.visualstudio.com/docs/terminal/advanced ; https://github.com/microsoft/vscode/blob/main/src/vs/workbench/contrib/terminal/common/terminalConfiguration.ts

### VS Code integrated terminal — real-world failure modes from the issue tracker

(1) Issue #166697 (filed by terminal lead Tyriar, fixed milestone January 2023, v1.75): before the fix, Ctrl+C-copy on Windows RETAINED the selection, so the next Ctrl+C copied again instead of interrupting — "This has bugged me for years". The fix introduced exactly the current split: Ctrl+Shift+C copies and retains the selection; Ctrl+C (Windows) copies and clears it "such that the next ctrl+c will send an interrupt". A maintainer comment confirms the design intent: "if there is a selection it will copy... The proposal here is to copy and clear the selection, such that the next ctrl+c sends SIGINT." (2) Issue #207915 and #184837: recurring "Ctrl+C doesn't interrupt in terminal" reports, closed as duplicates — users repeatedly rediscover the selection-dependent behavior. (3) Issue #177576 (Linux, closed): user reported text selection in the terminal getting their process interrupted while trying to copy — the inverse surprise on the platform where Ctrl+C always goes to the shell. (4) Issue #147339 (closed): rebinding "Terminal: Copy Selection" from Ctrl+Shift+C to Ctrl+C didn't behave as expected — custom rebinds interact badly with when-clauses and the skip-shell list. (5) Issue #295372 (2026, macOS + fish, labeled bug/upstream/important): Ctrl+C stopped interrupting entirely and the terminal printed "[99;5u" — a kitty-keyboard/CSI-u progressive-enhancement encoding regression upstream in xterm.js, showing the SIGINT path can also break at the keyboard-protocol-encoding layer, independent of copy bindings.

*Caveats:* For #177576 GitHub rendered only the issue body, not maintainer comments, so its exact resolution is unverified. #295372 details beyond the body (root-cause PR) were not pulled.

*Sources:* https://github.com/microsoft/vscode/issues/166697 ; https://github.com/microsoft/vscode/issues/207915 ; https://github.com/microsoft/vscode/issues/177576 ; https://github.com/microsoft/vscode/issues/147339 ; https://github.com/microsoft/vscode/issues/295372

## Linux & cross-platform terminal emulators

**Section summary.** On Linux, Ctrl+Shift+C/V is the unanimous clipboard convention — GNOME Terminal, Konsole, kitty, foot, and alacritty all ship it by default, while xterm ships no keyboard clipboard bindings at all and PuTTY ships copy-on-select + right-click/Shift-Ins; GNOME's docs and PuTTY's docs both explicitly state that plain Ctrl+C cannot be used because it is the interrupt character. Every surveyed Linux terminal auto-copies mouse selections to the PRIMARY selection (middle-click paste) while leaving CLIPBOARD alone; copy-on-select-to-CLIPBOARD is default only in Windows PuTTY and macOS iTerm2, ships off in kitty/Konsole/alacritty/foot/Windows Terminal, and the verified objections (KDE Discuss, kitty docs) are clipboard clobbering when selecting-to-highlight, clipboard-manager pollution, and clipboard-snooping security. On macOS the collision does not exist: Terminal.app, iTerm2, kitty, and alacritty all use Cmd+C/Cmd+V, leaving Ctrl+C as SIGINT. The only shipped resolution of tide's exact problem — CUA Ctrl+C/Ctrl+V as primary bindings over a live shell — is Windows Terminal's conditional fall-through ("copies the selected terminal content... If no selection exists, the key chord is sent directly to the terminal"), a design Konsole has had pending as an unmerged, off-by-default MR (!520) since 2021. For paste safety, bracketed paste (DECSET 2004) is now enabled by default at the readline 8.1/bash 5.1 and zsh 5.1 layers, and Windows Terminal (multi-line/large-paste warnings, default true), kitty (paste_actions confirm on control codes), and iTerm2 (at-prompt multi-line confirmation) add terminal-side guards on top.

### GNOME Terminal

Default Copy = Shift+Ctrl+C, Paste = Shift+Ctrl+V. Official help states verbatim: "The standard keyboard shortcuts, such as Ctrl+C, cannot be used to copy and paste text." Underlying VTE widget automatically places any mouse selection into the X11/Wayland PRIMARY selection (vte.cc calls widget_copy(ClipboardType::PRIMARY) on selection change), so middle-click paste works; explicit Ctrl+Shift+C is required to reach CLIPBOARD.

*Caveats:* No built-in copy-on-select-to-CLIPBOARD option exists in GNOME Terminal's UI (verified by absence in docs and settings, not by a citable statement). Shortcuts are user-remappable via Preferences > Shortcuts, including binding plain Ctrl+C if the user insists.

*Sources:* https://help.gnome.org/users/gnome-terminal/stable/txt-copy-paste.html.en ; https://gitlab.gnome.org/GNOME/vte/-/blob/master/src/vte.cc (widget_copy PRIMARY call sites ~lines 7269, 7305, 11018)

### Konsole

Default Copy = Ctrl+Shift+C, Paste = Ctrl+Shift+V (Edit menu); Paste Selection = Ctrl+Shift+Ins. Copy-on-select exists as a per-profile Mouse option but ships disabled: src/profile/Profile.cpp defines {AutoCopySelectedText, "AutoCopySelectedText", INTERACTION_GROUP, false}.

*Caveats:* Directly relevant prior art: KDE MR !520 'copy selected text to clipboard with ^C' (Ctrl+C copies when selection is non-empty, otherwise sends SIGINT; proposed as user-configurable and disabled by default) was opened Nov 2021 and is STILL in 'opened' state per GitLab API on 2026-06-10 — never merged. A KDE Discuss thread proposing copy-on-highlight as default got strong pushback (middle-click/PRIMARY already covers it; conflicts with clipboard managers; 'has this been user tested? is it possible this is for the 1%?').

*Sources:* https://docs.kde.org/trunk_kf6/en/konsole/konsole/commandreference.html ; https://invent.kde.org/utilities/konsole/-/merge_requests/520 ; https://invent.kde.org/utilities/konsole/-/blob/master/src/profile/Profile.cpp ; https://discuss.kde.org/t/konsole-copy-text-on-highlight-enabled-by-default/37591

### kitty

Defaults (kitty/options/definition.py): map ctrl+shift+c copy_to_clipboard, map ctrl+shift+v paste_from_clipboard; on macOS additionally map cmd+c copy_to_clipboard and map cmd+v paste_from_clipboard (only='macos'). copy_on_select default 'no'. Selection paste: shift+insert and kitty_mod+s paste_from_selection; middle-button release pastes selection ('paste_selection middle release ungrabbed paste_from_selection'). Paste guard: paste_actions default 'quote-urls-at-prompt,confirm' — 'Confirm the paste if the text to be pasted contains any terminal control codes as this can be dangerous'; optional confirm-if-large (>16KB), replace-dangerous-control-codes, filter. strip_trailing_spaces default 'never'.

*Caveats:* kitty's docs actively discourage copy_on_select: 'copying to the clipboard is a security risk, as all programs, including websites open in your browser can read the contents of the system clipboard.' Users can route copy-on-select to a private buffer (e.g. 'a1') instead of the clipboard.

*Sources:* https://sw.kovidgoyal.net/kitty/conf/ ; https://github.com/kovidgoyal/kitty/blob/master/kitty/options/definition.py ; https://sw.kovidgoyal.net/kitty/actions/

### foot

Defaults in foot.ini(5): clipboard-copy = 'Control+Shift+c XF86Copy', clipboard-paste = 'Control+Shift+v XF86Paste', primary-paste = 'Shift+Insert' plus middle-click mouse binding. selection-target default 'primary' — selecting text auto-copies to the Wayland primary selection only (options: none, primary, clipboard, both), so the system clipboard is never clobbered by selection unless the user opts in.

*Caveats:* Wayland-only terminal; no macOS build, so no Cmd-key story. The foot.ini man page section fetched did not document bracketed-paste options (foot supports bracketed paste as a terminal feature; application-side, not a config knob I could verify in that page).

*Sources:* https://man.archlinux.org/man/foot.ini.5.en

### Alacritty

Default bindings (extra/man/alacritty-bindings.5.scd): Linux/BSD/Windows Copy = Control+Shift+C, Paste = Control+Shift+V, PasteSelection = Shift+Insert. macOS: Copy = Command+C, Paste = Command+V. Copy-on-select to clipboard exists but is off: alacritty.5 man page, selection.save_to_clipboard — 'When set to true, selected text will be copied to the primary clipboard. Default: false' (selection otherwise lives in PRIMARY only).

*Caveats:* No PasteSelection binding ships in the macOS defaults (no PRIMARY concept there). The man page wording 'primary clipboard' for save_to_clipboard is Alacritty's term for the system clipboard, which is mildly confusing vs X11 PRIMARY.

*Sources:* https://github.com/alacritty/alacritty/blob/master/extra/man/alacritty-bindings.5.scd ; https://github.com/alacritty/alacritty/blob/master/extra/man/alacritty.5.scd

### xterm

Pure PRIMARY/cut-buffer model; no Ctrl+Shift+C/V in default translations. Verified in source (charproc.c VTInitTranslations): 'Shift <KeyPress> Insert:insert-selection(SELECT, CUT_BUFFER0)', '~Meta <Btn1Down>:select-start()', '~Ctrl ~Meta <Btn2Up>:insert-selection(SELECT, CUT_BUFFER0)' (middle-click paste), '<BtnUp>:select-end(SELECT, CUT_BUFFER0)'. The SELECT token resolves to PRIMARY because resource selectToClipboard defaults to False ('Bres(XtNselectToClipboard, ..., screen.selectToClipboard, False)'); it can be flipped at runtime via menu or DECSET (srm_SELECT_TO_CLIPBOARD). xterm is also the origin of bracketed paste: ctlseqs documents 'Ps = 2004 -> Set bracketed paste mode' wrapping pastes in ESC[200~ ... ESC[201~.

*Caveats:* Reaching the CLIPBOARD requires user-supplied translations (e.g. insert-selection(CLIPBOARD)) or selectToClipboard:true — the xterm FAQ has a dedicated entry on why pasting to/from other programs fails. I found no Ctrl+Shift+C/V in current master defaultTranslations, contradicting some third-party claims that modern xterm added them; treat 'no keyboard clipboard bindings at all' as the verified default.

*Sources:* https://github.com/ThomasDickey/xterm-snapshots/blob/master/charproc.c (VTInitTranslations, ~line 14970; Bres selectToClipboard ~line 472) ; https://invisible-island.net/xterm/ctlseqs/ctlseqs.html ; https://invisible-island.net/xterm/xterm.faq.html

### PuTTY

The strongest copy-on-select precedent: on Windows, left-drag selection IS the copy — 'When you let go of the button, the text is automatically copied to the clipboard.' Paste is right-click (default 'Compromise' mouse mode: right button pastes, middle extends selection) or Shift+Ins. The docs explicitly call out the SIGINT collision: 'if you do press Ctrl-C, PuTTY will send a Ctrl-C character down your session to the server where it will probably cause a process to be interrupted.' On Unix PuTTY, selection always goes to PRIMARY ('as is conventional') and mirroring to CLIPBOARD ('Auto-copy selected text to system clipboard') is opt-in. Ctrl+Shift+C/V exist only as configurable actions since 0.71 ('More choices of user interface for clipboard handling') — Ctrl-Shift-V is 'not enabled by default'. A config option can force-disable honoring bracketed paste mode requests from the server.

*Caveats:* Copy-on-select means any selection (including selecting just to highlight/read) silently destroys the Windows clipboard since Windows has a single clipboard; PuTTY 0.71's addition of configurable clipboard targets and Ctrl+Shift+C/V was the project's response, but I did not locate a citable individual complaint thread — the complaint pattern is inferred from the 0.71 feature additions and the KDE Discuss objections to the same design. Platform asymmetry note: the 'not enabled by default' auto-copy wording in Chapter 4 is clearest for the Unix build; on Windows, Chapter 3 documents auto-copy as the default behavior.

*Sources:* https://the.earth.li/~sgtatham/putty/0.83/htmldoc/Chapter3.html ; https://the.earth.li/~sgtatham/putty/0.83/htmldoc/Chapter4.html#config-selection ; https://www.chiark.greenend.org.uk/~sgtatham/putty/changes.html (0.71)

### macOS terminals (Terminal.app, iTerm2; kitty/alacritty on macOS)

The collision does not exist on macOS because CUA chords use Command, not Control: Terminal.app ships Copy = Command-C, Paste = Command-V (plus Shift-Cmd-V 'Paste the selection', Ctrl-Cmd-V 'Paste escaped text'), leaving Ctrl+C free as SIGINT. kitty and alacritty both add cmd+c/cmd+v defaults on macOS on top of (kitty) or instead of (alacritty) ctrl+shift bindings. iTerm2 additionally enables copy-on-select by default — source iTermPreferences.m: kPreferenceKeySelectionCopiesText (@"CopySelection") defaults to @YES ('If enabled, text is copied to the clipboard immediately upon selection').

*Caveats:* macOS has no PRIMARY selection; Terminal.app emulates the workflow with explicit 'Paste the selection' instead. iTerm2's copy-on-select default shows the behavior is uncontroversial on macOS only because selection-copy and Cmd+C never fight with SIGINT — it does not validate copy-on-select for a Ctrl-based scheme.

*Sources:* https://support.apple.com/guide/terminal/keyboard-shortcuts-trmlshtcts/mac ; https://github.com/gnachman/iTerm2/blob/master/sources/Settings/iTermPreferences.m (lines 69, 641) ; https://iterm2.com/documentation-preferences-general.html ; kitty definition.py and alacritty-bindings.5.scd as above

### Windows Terminal (additional prior art — the canonical resolution of the exact Ctrl+C collision)

Ships plain Ctrl+C and Ctrl+V as default copy/paste bindings, resolved by conditional fall-through. Official actions doc on 'copy': 'This copies the selected terminal content to your clipboard. If no selection exists, the key chord is sent directly to the terminal.' Default copy bindings: ctrl+c, ctrl+shift+c, ctrl+insert, enter; paste: ctrl+v, ctrl+shift+v, shift+insert. copyOnSelect default false. Paste guards on by default: multiLinePasteWarning true ('one or more command(s) might be executed automatically upon paste'), largePasteWarning true (>5 KiB), trimPaste true.

*Caveats:* Not in the user's requested product list, but it is the only major terminal that ships CUA Ctrl+C/Ctrl+V by default while hosting shells — i.e., verified prior art for tide's exact spec collision. Edge case: with the conditional design, Ctrl+C while a selection happens to exist does NOT interrupt the foreground process (selection must be cleared first); Enter is also bound to copy-when-selection.

*Sources:* https://learn.microsoft.com/en-us/windows/terminal/customize-settings/actions (Clipboard integration commands) ; https://learn.microsoft.com/en-us/windows/terminal/customize-settings/interaction

### X11/Wayland selection model (PRIMARY vs CLIPBOARD, middle-click)

Two independent channels: PRIMARY ('the last selected text', pasted with middle-click, no explicit copy step) and CLIPBOARD (explicit copy/paste, the Ctrl-C/Ctrl-V analogue) — PuTTY's docs state selected terminal text 'will always be automatically placed in the PRIMARY selection, as is conventional'. Wayland replicates this via the primary-selection-unstable-v1 (zwp_primary_selection) protocol, described as 'X primary selection emulation', noting 'the de facto way to perform this action is the middle mouse button'. Every Linux terminal surveyed (GNOME Terminal/VTE, Konsole, kitty, foot, alacritty, xterm, Unix PuTTY) auto-fills PRIMARY on selection while leaving CLIPBOARD untouched by default.

*Caveats:* This dual-channel model is the main reason Linux users reject copy-on-select-to-CLIPBOARD defaults (it already exists, on a separate buffer) — the KDE Discuss thread's top objection. A tide design that only implements one clipboard would regress middle-click-paste muscle memory on Linux.

*Sources:* https://wayland.app/protocols/primary-selection-unstable-v1 ; https://the.earth.li/~sgtatham/putty/0.83/htmldoc/Chapter4.html#config-selection ; per-terminal sources above

### Paste safety: bracketed paste and newline guards

Terminal side: bracketed paste mode is DECSET 2004 (xterm ctlseqs): pasted text arrives wrapped in ESC[200~ ... ESC[201~ 'so that the program can differentiate pasted text from typed-in text'. Shell side: GNU Readline's enable-bracketed-paste is 'On' by default (readline 8.1 / bash 5.1+), inserting pastes as a single string instead of executing embedded newlines; zsh's ZLE has bracketed paste since 5.1 'to avoid interpreting pasted newlines as accept-line'. Terminal-side newline guards on by default: Windows Terminal multiLinePasteWarning=true and largePasteWarning=true; kitty paste_actions='quote-urls-at-prompt,confirm' (confirms pastes containing terminal control codes); iTerm2 warns 'OK to paste N lines at shell prompt?' by default when shell-integration knows you're at a prompt (suppressible per-warning; promptForPasteWhenNotAtPrompt defaults NO, size warning alwaysWarnBeforePastingOverSize defaults -1/disabled).

*Caveats:* Bracketed paste only protects when the receiving application opts in — a bare shell without readline 8.1+/zsh 5.1+, or a program that never enables mode 2004, still executes pasted newlines, which is why Windows Terminal/iTerm2/kitty layer UI confirmation on top. PuTTY exposes a config to refuse bracketed-paste even when the server requests it. GNOME Terminal, Konsole, foot, alacritty, and xterm ship no multi-line paste confirmation by default (verified by absence in their docs/options, not a citable statement).

*Sources:* https://invisible-island.net/xterm/ctlseqs/ctlseqs.html (mode 2004, 'Bracketed Paste Mode' section) ; https://tiswww.case.edu/php/chet/readline/rluserman.html (enable-bracketed-paste, 'The default is On') ; https://github.com/zsh-users/zsh/blob/master/NEWS (5.1) ; https://github.com/gnachman/iTerm2/blob/master/sources/Pasting/iTermPasteHelper.m and sources/Settings/iTermAdvancedSettingsModel.m (line 774, 778) ; Windows Terminal interaction doc above

## Multiplexers & terminal-adjacent IDEs

**Section summary.** There is exactly one widely-shipped resolution to the Ctrl+C copy-vs-SIGINT collision, pioneered by JetBrains/JediTerm and made famous by Windows Terminal: Ctrl+C means "copy if a selection exists, otherwise fall through to the shell," combined with clearing the selection after copy so a second Ctrl+C always interrupts — Windows Terminal bakes the fallthrough into the copy action, while VS Code and Zed (opt-in, via PR #33491's `selection` context predicate) express it as a selection-conditional keybinding, which is the cleaner architecture for tide. Every Linux-native product surveyed (Zed, VS Code, Warp, JediTerm standalone, plus GNOME-convention terminals) keeps plain Ctrl+C as SIGINT by default and uses Ctrl+Shift+C for copy; only Windows Terminal ships Ctrl+C-as-copy as the default, and user reaction there was positive because the fallthrough makes it nearly invisible. The documented failure mode is persistent/structural selection: JetBrains' block-based terminal rewrite made clicking a block count as "selection," so Ctrl+C copied instead of interrupting (YouTrack IJPL-102573, Major usability bug, fixed 2024.1) — directly relevant if tide has block-style or sticky selections. For mouse copy, auto-copy-on-release is the norm (tmux copy-pipe-and-cancel on MouseDragEnd1Pane, Zellij copy_on_select=true, available as opt-in everywhere else), with OSC 52 / external copy-command as the clipboard transport. Zellij is the cautionary tale on the broader keybinding question: its bare-Ctrl mode keys collided with hosted apps badly enough that 0.41 shipped a "non-colliding" Unlock-First preset and a first-run wizard, versus tmux/screen which avoid the problem entirely with a single prefix key.

### tmux

Keyboard model: all keys pass through to the inner program except the single prefix (C-b by default); copy keys exist only inside the separate copy-mode key tables, entered explicitly (prefix+[). Mouse: 'mouse' option defaults to OFF (options-table.c default_num=0). When enabled, the root-table binding for MouseDrag1Pane is `if -F '#{||:#{pane_in_mode},#{mouse_any_flag}}' { send -M } { copy-mode -M }` — i.e. if the inner application has requested mouse events (mouse_any_flag), the drag is forwarded to it; otherwise tmux enters copy mode. Finishing a drag DOES auto-copy: in both copy-mode and copy-mode-vi tables, MouseDragEnd1Pane is bound by default to `send -X copy-pipe-and-cancel` (copies selection and exits copy mode); DoubleClick1Pane/TripleClick1Pane select word/line then copy-pipe-and-cancel after a 0.3s delay. Clipboard: 'set-clipboard' defaults to 'external' (since 2.6) — tmux pushes copies to the outer terminal via OSC 52 (requires Ms terminfo capability and terminal support); inner apps cannot set the clipboard unless set-clipboard=on. tmux 3.2+ adds 'copy-command' to pipe copies to xclip/wl-copy/pbcopy.

*Caveats:* Auto-copy lands in tmux's paste buffer always, but reaches the SYSTEM clipboard only if OSC 52 works in the hosting terminal (a perennial support headache; many terminals disable OSC 52 reads/writes). The mouse_any_flag passthrough means selection behavior differs depending on whether the inner app (vim, htop) enabled mouse mode — users must hold Shift to get the outer terminal's native selection.

*Sources:* https://raw.githubusercontent.com/tmux/tmux/master/key-bindings.c ; https://raw.githubusercontent.com/tmux/tmux/master/options-table.c ; https://github.com/tmux/tmux/wiki/Clipboard

### GNU screen

Pure prefix model: every key goes to the inner program unless preceded by the command character (C-a). Copy is a keyboard-driven modal flow: C-a [ (or C-a ESC) enters copy/scrollback mode with vi-like movement; space sets two marks; text between marks goes to screen's internal paste buffer; C-a ] writes the buffer to the current window's stdin. No mouse-drag copy model and no system-clipboard integration by default — mouse selection is left entirely to the hosting terminal emulator; the paste buffer can be exchanged with a file via readbuf/writebuf ('bufferfile').

*Caveats:* Represents the most conservative end of the spectrum: zero CUA affordances, zero key-stealing beyond the prefix. Its copy buffer is invisible to the OS clipboard, which is the standard complaint about it.

*Sources:* https://www.gnu.org/software/screen/manual/html_node/Copy.html ; https://www.gnu.org/software/screen/manual/html_node/Paste.html ; https://www.gnu.org/software/screen/manual/html_node/Copy-and-Paste.html

### Zellij

Modal keybindings with bare Ctrl+letter mode entries by default (Ctrl+p pane mode, Ctrl+o session mode, etc.) plus a 'locked' mode (Ctrl+g) that passes everything through. Mouse: mouse_mode defaults to true and copy_on_select defaults to true — releasing a mouse selection auto-copies, via OSC 52 by default ('copy_clipboard' chooses system vs primary; 'copy_command' can pipe to xclip/wl-copy/pbcopy instead). Holding Shift bypasses Zellij and lets the host terminal handle selection. The key-stealing problem is officially acknowledged: the docs have a dedicated 'Dealing with Colliding Keyboard Shortcuts' tutorial (example: Zellij's Ctrl+o vs vim's jumplist), and Zellij 0.41 (Oct 2024) shipped an 'Unlock-First (non-colliding)' preset — all mode keys require pressing Ctrl+g first — offered in a first-run setup wizard alongside the classic default.

*Caveats:* Zellij is the clearest documented case of a multiplexer shipping bare-Ctrl bindings and then having to engineer its way out: the collision complaints were significant enough to warrant a release-headline fix and a setup wizard. Note it still never bound Ctrl+C to copy — Ctrl+C remains SIGINT; copy is mouse-release or copy-mode 'y'. Its OSC 52 default means copy silently fails on terminals without OSC 52 support (FAQ-documented; copy_command is the workaround).

*Sources:* https://zellij.dev/documentation/options.html ; https://zellij.dev/tutorials/colliding-keybindings/ ; https://zellij.dev/news/colliding-keybinds-plugin-manager/ ; https://zellij.dev/documentation/faq.html

### Zed (built-in terminal)

Default keymaps (verified in repo): Linux Terminal context binds ctrl-shift-c → terminal::Copy, ctrl-shift-v → terminal::Paste, and explicitly binds "ctrl-c": ["terminal::SendKeystroke", "ctrl-c"] so Ctrl+C always reaches the shell. macOS binds cmd-c/cmd-v for copy/paste (no SIGINT conflict since Ctrl-C is distinct from Cmd-C) and likewise sends ctrl-c through. Issue #21262 requested 'Ctrl-C copies when text is highlighted, SIGINT otherwise'; resolved by merged PR #33491 (July 3, 2025) which added a `selection` keybinding context predicate and a keep_selection_on_copy setting — users can now opt in with {"context": "Terminal && selection", "bindings": {"ctrl-c": "terminal::Copy"}} — but Zed did NOT make this the default.

*Caveats:* A copy_on_select terminal setting also exists. The PR discussion notes a default-behavior change around selection clearing was later reverted in #39814, so exact keep_selection_on_copy default may have shifted; the opt-in conditional-Ctrl+C mechanism itself is stable. Zed's choice: ship the safe binding, provide the conditional machinery for users who want CUA Ctrl+C.

*Sources:* https://raw.githubusercontent.com/zed-industries/zed/main/assets/keymaps/default-linux.json ; https://raw.githubusercontent.com/zed-industries/zed/main/assets/keymaps/default-macos.json ; https://github.com/zed-industries/zed/issues/21262 ; https://github.com/zed-industries/zed/pull/33491

### Warp

Blocks model: every command + its output is one selectable Block, so copying usually targets block granularity (Copy Command, Copy Output, Copy Prompt) via block context menu/command palette rather than character-level selection — this sidesteps most Ctrl+C ambiguity because you click a block and use a dedicated shortcut. Linux default Copy is Ctrl+Shift+C (per docs); Ctrl+C remains SIGINT. Copy actions are rebindable, and binding plain Ctrl+C to Copy is unconditional: issue #3611 ('Keyboard shortcut for Copy should only trigger if there is selected text') documents that doing so breaks process termination entirely; it was closed not-planned. Issue #3558 documents related confusion where a custom Copy shortcut copies command output rather than the selected command text.

*Caveats:* docs.warp.dev was rate-limited/bot-blocked during research, so the exact current Linux shortcut table (e.g. Ctrl+Shift+Alt+C for copy_outputs) is sourced from search snippets of the official docs, not a full page fetch — treat specific block-shortcut combos as medium confidence. The verified takeaway stands: Warp never made Ctrl+C copy, and its issue tracker shows users who tried hit the SIGINT collision with no conditional-binding escape hatch.

*Sources:* https://docs.warp.dev/getting-started/keyboard-shortcuts/ ; https://docs.warp.dev/terminal/blocks/ ; https://github.com/warpdotdev/Warp/issues/3611 ; https://github.com/warpdotdev/Warp/issues/3558

### JetBrains IDEs (classic terminal / JediTerm)

JediTerm source (TerminalPanel.java, handleCopy(KeyEvent)) implements exactly the conditional model: if the copy action is invoked by plain Ctrl+C and there is NO selection, the handler returns 'not handled' and Ctrl+C is sent to the shell (SIGINT); if a selection exists, Ctrl+C copies it AND clears the selection (so the very next Ctrl+C interrupts). Code: `boolean sendCtrlC = ctrlC && mySelection == null; handleCopy(ctrlC, false); return !sendCtrlC;`. Standalone JediTerm default copy keystroke is Cmd+C on macOS and Ctrl+Shift+C on Linux/Windows, with the in-source comment 'CTRL + C is used for signal; use CTRL + SHIFT + C instead'; inside IntelliJ IDEs the IDE keymap's Ctrl+C copy action is honored in the terminal through the conditional fallthrough above. The IDE also has 'Override IDE shortcuts' (use shell shortcuts when terminal is focused) and 'Copy to clipboard on selection' settings.

*Caveats:* So JetBrains is the longest-standing 'Ctrl+C means copy-if-selection inside a terminal' implementation, and it works because copy clears the selection. The failure mode is documented in their own tracker: IJPL-102573 ('New Terminal: command interrupt (Ctrl+C) is not working when block is selected', priority Major, type Usability Problem) — in the 2024 block-based terminal rewrite, clicking a block selected it, making Ctrl+C copy block text instead of interrupting `sleep 100`; reporter wrote 'Ctrl+C is one of the main terminal shortcuts... It is very confusing when it is not interrupting the command.' Fixed in 2024.1. Lesson: persistent/structural selection (blocks) breaks the selection-conditional heuristic that works fine for transient drag-selections.

*Sources:* https://raw.githubusercontent.com/JetBrains/jediterm/master/ui/src/com/jediterm/terminal/ui/TerminalPanel.java ; https://raw.githubusercontent.com/JetBrains/jediterm/master/ui/src/com/jediterm/terminal/ui/settings/DefaultSettingsProvider.java ; https://youtrack.jetbrains.com/issue/IJPL-102573 ; https://www.jetbrains.com/help/idea/settings-tools-terminal.html

### Windows Terminal

The flagship product that made COPY the default meaning of Ctrl+C in a terminal pane. Official docs (Actions page, 'Copy' action): 'This copies the selected terminal content to your clipboard. If no selection exists, the key chord is sent directly to the terminal.' Default bindings: ctrl+c, ctrl+shift+c, and ctrl+insert all map to Terminal.CopyToClipboard; ctrl+v/ctrl+shift+v/shift+insert paste. The conditional fallthrough is built into the copy action itself rather than the keybinding layer. This default came from issue #3058 ('Include Ctrl+C, Ctrl+V keybindings by default' — 'It's quite puzzling for a Windows terminal to not come with the most commonly used Windows shortcuts ever'), implemented in PR #5217; issue #2285 ('Ctrl+C, when text is highlighted, should copy not cancel') shows the prior user expectation.

*Caveats:* User reaction has been broadly positive (it became the canonical pattern others cite, e.g. VS Code feature request #141073 'Make CTRL-C in terminal copy if text is selected like Windows Terminal'), but the tracker shows edge cases: #10687 (rebinding copy to ctrl+shift+c intermittently reverts to sending SIGINT), and copyOnSelect-related confusion (#5754, #3053, #19942) where selection state lingering or right-click semantics interact badly with copy. Mitigations Windows Terminal uses: copy dismisses the selection, and copyOnSelect defaults to false.

*Sources:* https://learn.microsoft.com/en-us/windows/terminal/customize-settings/actions (Copy action section) ; https://github.com/microsoft/terminal/issues/3058 ; https://github.com/microsoft/terminal/issues/2285 ; https://github.com/microsoft/terminal/issues/10687

### VS Code (integrated terminal) — bonus prior art

Source-verified (terminal.clipboard.contribution.ts): CopySelection is bound to ctrl+shift+c (Linux/Windows primary) and cmd+c on macOS, with when-clause requiring terminal focus AND text selected. Separately, a Windows-only action CopyAndClearSelection binds plain ctrl+c with the same selection-required when-clause and clears the selection after copying — so on Windows, Ctrl+C copies iff selection exists, otherwise the keybinding doesn't match and Ctrl+C reaches the shell; on Linux, plain Ctrl+C is never a copy key by default. Also has terminal.integrated.copyOnSelection (default false) and rightClickBehavior copyPaste on Windows.

*Caveats:* Implemented via the keybinding when-clause (declarative) rather than inside the action like Windows Terminal — an architecture worth copying for tide: a 'terminalTextSelected' context key makes the conditional binding user-visible and user-overridable. Note VS Code deliberately did NOT enable conditional Ctrl+C on Linux, keeping Ctrl+Shift+C there — consistent with every Linux-native terminal surveyed.

*Sources:* https://raw.githubusercontent.com/microsoft/vscode/main/src/vs/workbench/contrib/terminalContrib/clipboard/browser/terminal.clipboard.contribution.ts ; https://github.com/microsoft/vscode/issues/141073

## Mechanics & CUA-in-terminal prior art

**Section summary.** Mechanically, SIGINT does not exist at the keyboard: Ctrl+C is byte 0x03, converted to a signal only by the inner pty's line discipline when ISIG is set, so a host that swallows Ctrl+C starves cooked-mode shells of their interrupt and raw-mode TUIs (vim, less, REPLs) of the byte they handle themselves — unconditional CUA Ctrl+C over a terminal pane is therefore not a viable default, and Ctrl+V has a parallel cost (readline quoted-insert, vim blockwise visual, whose documented fallback Ctrl+Q is itself eaten by IXON). The industry has converged on exactly one compromise, context-sensitive Ctrl+C (selection → copy, else pass through), shipped by default in Windows conhost (2014, which additionally stands down when the inner app disables processed input — the strongest precedent for termios-aware interception), VS Code's terminal (Windows-only, selection-gated when-clause verified in source; Linux keeps Ctrl+Shift+C/V), and JetBrains IDEs (Linux/Windows), with kitty offering it as the opt-in copy_or_interrupt action; the documented costs, best cataloged in JetBrains' trackers, are muscle-memory breakage, look-before-you-press cognitive load, and stale selections eating interrupts. Terminal-emulator authors who refused (mintty, whose override option is literally named ShootFoot; PuTTY's FAQ) argue a terminal must deliver every control character to the inner app, and zellij's retreat to an 'Unlock-First (non-colliding)' preset shows what happens when a host squats on bare Ctrl chords. The closest product-shape to tide, the micro editor (CUA Ctrl+S/C/V/Z in buffers, well received, ~28.8k stars), is decisive prior art: inside its own terminal panes it abandons CUA completely and passes every key to the shell, reserving only double-tap chords (Ctrl-q Ctrl-q, Ctrl-e Ctrl-e, Ctrl-w Ctrl-w). Net ruling for tide: CUA in editor panes is proven; in shell panes choose between micro-style full pass-through with a leader/double-tap escape (safe, precedented) or VS Code/conhost-style selection-gated Ctrl+C with copy-and-clear-selection semantics (precedented but with documented complaint classes), and if intercepting anything, consider conhost's trick of checking the pane's termios state — noting that the GNOME rebind-passthrough detail and the Unix tcgetattr-on-master transfer remain unverified at primary sources.

### termios / tty line discipline (the mechanical ground truth)

Ctrl+C is just byte 0x03 (ETX) stored in the VINTR cc slot: 'Send a SIGINT signal. Recognized when ISIG is set, and then not passed as input.' SIGINT is generated by the kernel line discipline of the pty the inner process sits on — not by the terminal emulator. cfmakeraw() clears ECHO|ECHONL|ICANON|ISIG|IEXTEN from c_lflag and IXON (among others) from c_iflag, so full-screen TUIs in raw mode receive 0x03 as a plain byte and implement interrupt semantics themselves. Also defined there: VLNEXT (Ctrl+V, 026) 'Quotes the next input character... Recognized when IEXTEN is set'; VSTOP/VSTART (Ctrl+S/Ctrl+Q) under IXON. Implication for an outer host that swallows Ctrl+C: the 0x03 never reaches the pty master, so (a) cooked-mode shells never get SIGINT — no way to kill a runaway foreground process; (b) raw-mode TUIs (vim's interrupt/abort key, less, REPLs whose KeyboardInterrupt depends on SIGINT or on seeing 0x03) silently lose their interrupt mechanism. The host must write 0x03 to the pty (or not intercept) for inner programs to behave.

*Caveats:* The man page is the verified part; the breakage enumeration for specific TUIs (vim, htop, REPLs) is my derivation from it — directionally solid but per-app details vary (e.g., vim can run with ISIG still on and field SIGINT itself; either way the byte/signal must reach vim's tty, which interception prevents). Not separately verified per app.

*Sources:* https://man7.org/linux/man-pages/man3/termios.3.html

### Ctrl+V / lnext (what stealing paste costs)

Two distinct losses. Cooked mode: termios VLNEXT (default Ctrl+V) quotes the next input char; in bash/readline emacs mode the bash(1) man page defines 'quoted-insert (C-q, C-v): Add the next character typed to the line verbatim' and 'tab-insert (C-v TAB)'. Raw-mode apps: in vim, Normal-mode Ctrl+V is blockwise visual and Insert-mode CTRL-V is verbatim-insert; vim's docs codify the escape hatch: 'When CTRL-V is mapped (e.g., to paste text) you can often use CTRL-Q instead' — but immediately warn 'Some terminal connections may eat CTRL-Q' (IXON flow control claims Ctrl+Q in cooked mode). Notably, readline's C-q alias for quoted-insert has the same IXON problem, so if a host steals Ctrl+V, the fallback key is itself unreliable unless the host/shell disables IXON. VS Code's platform split is instructive prior art: it binds terminal paste to Ctrl+V on Windows but deliberately Ctrl+Shift+V on Linux, leaving Ctrl+V untouched for Linux shells/vim.

*Caveats:* No quantitative data exists on how often real users press C-v quoted-insert in shells — 'commonly used' is unmeasurable; the loud constituency is vim users (blockwise visual), not shell quoted-insert. The 'host should disable IXON in panes it controls' move is standard (micro-style raw mode does it) but I did not find a doc spelling it out for a multiplexer host.

*Sources:* https://man7.org/linux/man-pages/man1/bash.1.html (quoted-insert); https://vimhelp.org/insert.txt.html (i_CTRL-V / CTRL-Q alternative); https://man7.org/linux/man-pages/man3/termios.3.html (VLNEXT/IEXTEN, IXON)

### micro editor (closest product-shape to tide: CUA editor + terminal panes)

Default buffer bindings are full CUA: "Ctrl-s": "Save", "Ctrl-c": "Copy|CopyLine", "Ctrl-x": "Cut|CutLine", "Ctrl-v": "Paste", "Ctrl-z": "Undo", "Ctrl-a": "SelectAll", "Ctrl-q": "Quit" (runtime/help/keybindings.md). This works because micro puts the hosting terminal in raw mode, so Ctrl+C/S/Q/Z arrive as bytes (confirmed as the answer to the first question asked on the HN launch thread: 'How do those not get blocked by terminal emulators?' → raw mode). The decisive prior art is its terminal pane (`term` command, commands.md): when a terminal pane is focused micro abandons CUA entirely — the documented default bindings for the 'terminal' pane scope are ONLY three double-tap chords: "<Ctrl-q><Ctrl-q>": "Exit", "<Ctrl-e><Ctrl-e>": "CommandMode", "<Ctrl-w><Ctrl-w>": "NextSplit"; every other keypress, including Ctrl+C/V/S, passes through to the shell. So the one shipping CUA terminal editor resolved the collision by mode-switching per pane type, not by context-sensitive Ctrl+C.

*Caveats:* Reception of CUA-in-terminal is broadly positive (28,810 GitHub stars as of 2026-06-10; HN: 'Micro is more suited for people that are used to the Windows/Notepad-CUA workflow... and want to use that workflow inside the terminal'), but it costs conventions: Ctrl-z is Undo so no suspend by default; macOS terminals need reconfiguration to forward Alt (README tells users to switch to iTerm2 and set 'Use Option key as Meta key'); some chords are physically unbindable because the hosting terminal sends ambiguous escapes (keybindings.md documents this class of problem). Double-tap chords mean a single literal Ctrl+Q/Ctrl+E/Ctrl+W press is delayed/withheld from the inner shell — an inherent cost of the scheme that micro's docs don't discuss.

*Sources:* https://github.com/zyedidia/micro/blob/master/runtime/help/keybindings.md (terminal-pane defaults at the 'command and terminal panes' section); https://github.com/zyedidia/micro/blob/master/runtime/help/commands.md (`term`); https://news.ycombinator.com/item?id=12388654 ; https://news.ycombinator.com/item?id=23334190

### nano

nano avoids the clipboard collision by not being CUA: Ctrl+S 'Save current file' (modern nano), but Ctrl+C is 'Report cursor position', Ctrl+V is 'One page down'; cut/copy/paste are Ctrl+K (cut line), Alt+6 (copy line), Ctrl+U (paste cutbuffer) — internal cutbuffer, not system clipboard. Where nano does take over termios keys it documents the escape hatch: by default it claims ^S/^Q (raw mode kills IXON), and the nanorc 'preserve' option restores flow control: 'Preserve the XOFF and XON sequences (^S and ^Q) so that they are caught by the terminal.' Ctrl+Z suspend likewise requires explicit 'bind ^Z suspend main'.

*Caveats:* nano is prior art for 'take Ctrl keys in raw mode and offer an opt-out', not for CUA clipboard — its copy/paste is deliberately non-CUA, so it sidesteps the exact collision tide faces.

*Sources:* https://www.nano-editor.org/dist/latest/cheatsheet.html ; https://www.nano-editor.org/dist/latest/nanorc.5.html

### Windows conhost (Windows 10 console) — context-sensitive Ctrl+C, pre-dating Windows Terminal

The 2014 'Enable Ctrl key shortcuts' feature: Ctrl+C copies when text is selected; 'When no text is selected, it will still send the break signal to the running application. The first CTRL-C copies the text and clears the selection, and the second one signals the break.' Ctrl+V pastes. The crucial mechanic for tide: conhost inspects the inner app's input mode and stands down — 'Our code tries to avoid interfering with console applications that may also use those keys. These programs typically disable line input, processed input and echo input modes via the SetConsoleMode() API.' I.e., the host's CUA interception is conditioned on the inner app being in cooked mode; raw-mode apps get their keys untouched. Disable via CtrlKeyShortcutsDisabled registry value or properties UI.

*Caveats:* Initially gated behind 'Enable experimental console features'. The Unix analog — outer host reading the pty's termios (tcgetattr on the pty master reflects slave settings on Linux) to decide whether ISIG-style interception is safe — is plausible and is effectively what conhost does on Windows, but I did not find a Unix terminal emulator documented as doing this; treat that design transfer as unverified/novel.

*Sources:* https://blogs.windows.com/windowsdeveloper/2014/10/07/console-improvements-in-the-windows-10-technical-preview/

### VS Code integrated terminal — context-sensitive Ctrl+C, verified in source

In src/vs/workbench/contrib/terminalContrib/clipboard/browser/terminal.clipboard.contribution.ts: TerminalCommandId.CopyAndClearSelection is bound to win: Ctrl+C with when: (textSelected && focus) — selection present → copy+clear; no selection → the keybinding's when-clause fails, the keystroke falls through to xterm.js and the shell gets 0x03/SIGINT. CopySelection is Ctrl+Shift+C (and Cmd+C on mac, also selection-gated). Paste: Ctrl+V on Windows (secondary Ctrl+Shift+V), Cmd+V mac, but only Ctrl+Shift+V on Linux — Ctrl+V is deliberately left to the shell on Linux. Docs confirm the platform split (Linux: Ctrl+Shift+C/V; Windows: Ctrl+C/V). So VS Code ships context-sensitive Ctrl+C by default only on Windows, and never steals plain Ctrl+C/Ctrl+V on Linux.

*Caveats:* The failure mode is on record: issue #29773 'Ctrl-C doesn't break in terminal' — a context-key bug made Ctrl+C attempt copy with no selection, killing SIGINT until fixed; the whole scheme's reliability hangs on selection-state tracking being exactly right. Issue #147339 shows users struggling to rebind copy to Ctrl+C themselves. The copy-and-CLEAR-selection choice on Windows is deliberate: it makes a second Ctrl+C press send the interrupt (same two-press pattern as conhost).

*Sources:* https://github.com/microsoft/vscode/blob/main/src/vs/workbench/contrib/terminalContrib/clipboard/browser/terminal.clipboard.contribution.ts ; https://code.visualstudio.com/docs/terminal/basics ; https://github.com/microsoft/vscode/issues/29773

### JetBrains IDE terminal (IntelliJ et al.) — context-sensitive Ctrl+C with documented backlash

The IntelliJ terminal on Linux/Windows handles Ctrl+C context-dependently — YouTrack IJPL-112941 states it precisely: 'Currently the IntelliJ terminal handles Ctrl+C context-dependent: if there is selected text, it copies it; if there is no selected text, it breaks. I guess this was a conscious design decision, but it also creates some problems.' IJPL-159963 ('New Terminal: Ctrl+C works differently if text selected (Linux/Windows)', still Open as a bug) reproduces it: select text during `sleep 100`, Ctrl+C copies; deselect, Ctrl+C interrupts — filed with 'Expected result: The unified and/or predictable behaviour.'

*Caveats:* This is the best-documented catalog of arguments AGAINST context-sensitive Ctrl+C: (1) muscle memory — 'it's different from the system terminal... This is quite nasty if one uses both... since it ruins muscle memory. This alone forces me to not use IntelliJ's terminal'; (2) cognitive load — 'I now need to look at the terminal to see if there is some text selected or not, to know what's going to happen'; (3) stale/accidental selection eats the interrupt (typed text + selection → Ctrl+C copies instead of sending SIGINT); (4) the adjacent 'Copy to clipboard on selection' setting does not disable it, confusing even JetBrains support in-thread. Note one tracker classifies the behavior itself as a bug while support elsewhere calls it by-design — JetBrains is internally inconsistent about it.

*Sources:* https://youtrack.jetbrains.com/issue/IJPL-112941 ; https://youtrack.jetbrains.com/issue/IJPL-159963

### kitty — context-sensitive Ctrl+C as a named, opt-in primitive

kitty ships first-class actions for exactly this ruling: copy_or_interrupt — 'Copy the selected text from the active window to the clipboard, if no selection, send SIGINT (aka ctrl+c)' — and copy_and_clear_or_interrupt — same 'and clear selection'. Users opt in with `map ctrl+c copy_or_interrupt`. Default copy remains ctrl+shift+c (copy_to_clipboard).

*Caveats:* Not default — kitty's author exposes it as a primitive but keeps the safe Ctrl+Shift+C default. Implementation nuance: kitty synthesizes/forwards the interrupt byte itself rather than 'not handling' the key; functionally equivalent for the inner pty.

*Sources:* https://sw.kovidgoyal.net/kitty/actions/

### mintty — the articulated refusal (swap, don't overload)

Default copy is Ctrl+Ins / Ctrl+Shift+C (with CtrlShiftShortcuts=yes). mintty's answer to CUA demand is a swap, not context-sensitivity: 'If both settings CtrlShiftShortcuts and CtrlExchangeShift are enabled, copy & paste functions are assigned to plain (unshifted) Ctrl+C and Ctrl+V' — the literal control characters move to Ctrl+Shift+C/V, so every control byte remains typeable. Directly remapping control chars via KeyFunctions (e.g., C+v:paste) requires an option literally named ShootFoot=on; the man page: 'The usage of this option is discouraged because disabling basic control functions, like the interrupt character Control+C, can be considered as shooting oneself in the foot.' Maintainer's argument in issue #602: 'mintty is a terminal application and as such it needs to be able to provide all control characters to the application running in it, like any other terminal application' and 'the request to exchange only the copy/paste keys creates an inconsistency.'

*Caveats:* The swap approach preserves a path to every control byte but still breaks inner-app expectations (Ctrl+C no longer interrupts; you must learn Ctrl+Shift+C for SIGINT) — it trades collision for relearning, which is why it's buried as a 'hidden setting... I consider it exotic' (maintainer, issue #602).

*Sources:* https://github.com/mintty/mintty/issues/602 ; https://raw.githubusercontent.com/mintty/mintty/master/docs/mintty.1 (ShootFoot, KeyFunctions)

### PuTTY — refusal on the same grounds

FAQ A.6.6: selection auto-copies ('there is no need to press Ctrl-Ins or Ctrl-C or anything else'), paste is Shift+Ins/right-click, and 'pressing Ctrl-C will send a Ctrl-C character to the other end of your connection (just like it does the rest of the time), which may have unpleasant effects.' Ctrl+C is never intercepted by default; newer versions allow reconfiguring clipboard keys.

*Caveats:* PuTTY is remote-only, which sharpens its stance: it cannot know the remote tty's mode, so unconditional pass-through is the only safe default — the same epistemic problem an outer host has for any pane whose termios it doesn't inspect.

*Sources:* https://www.chiark.greenend.org.uk/~sgtatham/putty/faq.html (A.6.6)

### GNOME Terminal — selection-gated accelerator on rebind

Default is Ctrl+Shift+C/V. Reported current behavior when a user rebinds Copy to Ctrl+C: it copies only when a selection exists and passes Ctrl+C through to the foreground process otherwise (i.e., the Copy action/accelerator is insensitive without a selection, so the key falls through). A past Ubuntu/GNOME regression that bound 'ctrl-c to copy to clipboard unconditionally' — silently breaking SIGINT — was treated as a bug and fixed.

*Caveats:* UNVERIFIED AT PRIMARY SOURCE: I could not locate the GNOME GitLab/Bugzilla ticket in this pass; the passthrough-on-rebind behavior comes from a secondary write-up and search corroboration only. Verify against gnome-terminal source or its GitLab before citing in the spec. The regression story is still useful evidence that 'Ctrl+C copies unconditionally' is consensus-bug territory even in CUA-friendly desktop projects.

*Sources:* https://matt-helps.com/post/ubuntu-ctrl-c-no-longer-works-in-terminal/ (secondary)

### zellij — what happens when a terminal host claims bare Ctrl keys

zellij's default modal keybindings sit on bare Ctrl chords (Ctrl+o session mode, etc.) and collide with inner applications (their own docs use vim's Ctrl+o jumplist as the example). The backlash was large enough that 0.41 (2024) shipped the 'Unlock-First (non-colliding)' keybinding preset: a single unlock chord (Ctrl+g) gates all host bindings, and modes are reached by plain characters after unlock — i.e., they retreated from resident Ctrl interception to a prefix/leader model, converging with tmux's design and micro's double-tap chords.

*Caveats:* Even the 'non-colliding' preset still collides (issue #3907: its Alt+F collides with shell navigation), and the fix is yet another configurable 'secondary modifier' — evidence that for a host pane containing arbitrary TUIs, the only collision-free reserved set is a single escape/leader key, not a CUA family.

*Sources:* https://zellij.dev/tutorials/colliding-keybindings/ ; https://zellij.dev/news/colliding-keybinds-plugin-manager/ ; https://github.com/zellij-org/zellij/issues/1399 ; https://github.com/zellij-org/zellij/issues/3907
