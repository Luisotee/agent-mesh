# terminal-emulator

Termux's terminal emulator, copied for the Android app's terminal view (`docs/apps.md`, "Android"): it parses a byte stream of escape sequences into a screen buffer with scrollback, and nothing else.

- **Origin:** `terminal-emulator/src/main/java/com/termux/terminal/` of [termux/termux-app](https://github.com/termux/termux-app) at tag `v0.118.3`. Termux's own `LICENSE.md` puts this module (and `terminal-view`) under the Apache License 2.0, as an exception to the GPLv3 the rest of the repository is under: its code comes from jackpal's Android-Terminal-Emulator. The licence is `LICENSE` here, kept with the files; the rest of Termux is not used.
- **Removed:** `TerminalSession.java` (forks a local process through JNI) and `JNI.java`, with the native code and the Termux resources. The app feeds the emulator the bytes of a remote terminal instead (`dev.agentmesh.android.term`).
- **Changed:** `TerminalSessionClient.java` lost the callbacks that took a `TerminalSession` (the emulator itself uses only the cursor-state and logging ones). `TerminalBuffer.java` has a free-running `getHistoryScrolls()` counter (a few lines in `scrollDownOneLine`), which the app uses to keep a reader on the lines they scroll back to: the emulator's own `getScrollCounter()` also counts scrolls inside horizontal margins, which push nothing into the history. Nothing else is edited, so updating means copying the same files again and re-adding those lines.
- Run the app's tests (`android/scripts/check.sh`) after an update: `TerminalEmulatorTest` feeds it real escape sequences.
