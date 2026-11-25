package com.termux.terminal;

/**
 * The interface for communication between the emulator and its client: cursor state, and logs.
 *
 * agent-mesh change: the callbacks that took a {@code TerminalSession} (text, title, finished, clipboard, bell,
 * colours) are gone, with TerminalSession itself: this copy has no local-process session.
 */
public interface TerminalSessionClient {








    void onTerminalCursorStateChange(boolean state);



    Integer getTerminalCursorStyle();



    void logError(String tag, String message);

    void logWarn(String tag, String message);

    void logInfo(String tag, String message);

    void logDebug(String tag, String message);

    void logVerbose(String tag, String message);

    void logStackTraceWithMessage(String tag, String message, Exception e);

    void logStackTrace(String tag, Exception e);

}
