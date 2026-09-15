# Attach

## CLI

attach <id> [--mode <mode>] [--ssh <host>] [--show-detach-instructions] [--via
<cmd...>]

## Requirements

- Supported both in cli and via the library
- When initiated, detects if the session has already ended or doesn't exist,
  outputting the appropriate error for each case
- Attaches to session, outputs "current" rendered terminal state (specifically:
  the most recent available ground and rune boundary state) and continues to
  update it by streaming raw output since that state
- Clients that cannot keep up with consuming the stream (resulting in full
  buffers) result in the attached client gracefully skipping forward by
  re-attaching at a later point, getting the latest rendered terminal state and
  resuming from that point.
- Writes all concurrently connecting clients' input to the session PTY
- Client resizes are forwarded to the session PTY. When there are multiple, the
  smallest column size, and smallest row size across all clients (even if
  different clients) is forwarded, ensuring non-broken rendering
- Detach with `ctrl+\`. Prints "Detached from session".
- Closes automatically when the session ends or disconnects and prints a
  one-line outcome message.
- In isolated presentation, detach restores the pre-attach screen and position.
  End/disconnect additionally appends the final session screen, omitting
  trailing rows never touched, before printing the outcome.
- Showing detach instructions means a line of the terminal is reserved by the
  client for showing how to detach.
  - This line is cleared when session ends or disconnects.
  - The instruction line is styled with a separate subtle background color.
- `--via` is the unsugared form of `--ssh`, allowing more arbitrary commands to
  be used. providing both is an error.

### Modes

- Attach supports `auto` (default), `isolated`, and `native` presentation, both
  in the cli and via the library.
- Detaching does not clear prior scroll history in any mode
- On native detach/end/disconnect, bgx restores local tty settings and leaves
  the outer terminal usable, exiting any session alternate screen and disabling
  session input-reporting modes. Cleanup does not clear normal-buffer history.
  Native application controls may themselves alter that history.
- Isolated mode retains its existing detach/end/disconnect behavior.
- When detach hint is enabled, isolated mode shows it in a persistent reserved
  row, while native mode shows a one-time hint, with any title indication
  best-effort only.
- Auto follows the session’s active buffer:
  - Primary uses native presentation; alternate uses isolated presentation.
  - This applies at initial attach, during live output, and on
    resynchronization, with transitions in either direction.
  - TUIs request alternate-screen entry/exit normally; auto follows those
    transitions by switching presentation.
  - Lifecycle, hint, and sizing behavior follow the current presentation. Each
    transition updates any reserved hint row and the advertised terminal
    dimensions.
  - Primary-buffer TUIs are supported with SSH-like behavior.

## Implementation

See [./attach.md#implementation]

- The attach protocol uses tagged, length-prefixed frames for terminal input,
  terminal output, resize, detach, and session end.
- Initial and skip-forward terminal states are delivered as ordinary output
  frames. Snapshots do not use a separate frame type.
- Initial attach and resynchronization restore the active buffer and state
  needed for subsequent output without clearing pre-existing outer scrollback.

### Modes implementation

- Isolated presentation uses a protected alternate screen.
- Native presentation uses the normal terminal buffer, allowing native
  scrollback and SSH-like application control
- Exact pre-attach viewport restoration is not guaranteed in native presentation

## Verification

The [attach requirements](#requirements) are strongly validated via
golang-driven blackbox tests for each mode and terminal rendering technique of
the command within the session.