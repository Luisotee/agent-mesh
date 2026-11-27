package dev.agentmesh.android.term

/** The keys the key row and the IME's Enter and Backspace send that are not text. */
enum class Key { Esc, Tab, ShiftTab, Enter, Backspace, Delete, Up, Down, Left, Right, Home, End, PageUp, PageDown }

/**
 * What a program in a terminal reads for a key or for text (xterm's encodings, as Termux's `KeyHandler` has them for the
 * keys it covers). Pure: the cursor keys' application mode (DECCKM) is the program's, asked of the emulator by the caller.
 * [ctrl] and [alt] are the key row's sticky modifiers; Ctrl with a letter or one of `@ [ \ ] ^ _` is a control code,
 * Alt prefixes ESC.
 */
object KeyEncoder {
    private const val ESC = "\u001B"

    /** xterm's modifier parameter for a key with modifiers: 1 + Shift 1 + Alt 2 + Ctrl 4 (Shift isn't offered here). */
    private fun mod(ctrl: Boolean, alt: Boolean) = 1 + (if (alt) 2 else 0) + (if (ctrl) 4 else 0)

    fun key(k: Key, ctrl: Boolean = false, alt: Boolean = false, cursorApplication: Boolean = false): ByteArray {
        val m = ctrl || alt
        val s = when (k) {
            Key.Esc -> ESC
            Key.Tab -> if (alt) "$ESC\t" else "\t"
            Key.ShiftTab -> "$ESC[Z"
            Key.Enter -> if (alt) "$ESC\r" else "\r"
            // The DEL a terminal sends for the Backspace key (what the host's terminals expect: Backspace is not ^H).
            Key.Backspace -> if (alt) "$ESC\u007f" else "\u007f"
            Key.Delete -> if (m) "$ESC[3;${mod(ctrl, alt)}~" else "$ESC[3~"
            Key.PageUp -> if (m) "$ESC[5;${mod(ctrl, alt)}~" else "$ESC[5~"
            Key.PageDown -> if (m) "$ESC[6;${mod(ctrl, alt)}~" else "$ESC[6~"
            Key.Up -> cursor('A', ctrl, alt, cursorApplication)
            Key.Down -> cursor('B', ctrl, alt, cursorApplication)
            Key.Right -> cursor('C', ctrl, alt, cursorApplication)
            Key.Left -> cursor('D', ctrl, alt, cursorApplication)
            Key.Home -> cursor('H', ctrl, alt, cursorApplication)
            Key.End -> cursor('F', ctrl, alt, cursorApplication)
        }
        return s.toByteArray(Charsets.UTF_8)
    }

    private fun cursor(code: Char, ctrl: Boolean, alt: Boolean, application: Boolean): String = when {
        ctrl || alt -> "$ESC[1;${mod(ctrl, alt)}$code"
        application -> "${ESC}O$code"
        else -> "$ESC[$code"
    }

    /** Typed text: each character as UTF-8, with Ctrl making a control code of a letter (and `@ [ \ ] ^ _ ?`), and Alt an ESC before it. */
    fun text(text: String, ctrl: Boolean = false, alt: Boolean = false): ByteArray {
        val out = java.io.ByteArrayOutputStream()
        var i = 0
        while (i < text.length) {
            val cp = text.codePointAt(i)
            i += Character.charCount(cp)
            if (alt) out.write(0x1b)
            val code = if (ctrl) control(cp) else null
            if (code != null) out.write(code) else out.write(String(Character.toChars(cp)).toByteArray(Charsets.UTF_8))
        }
        return out.toByteArray()
    }

    /** The control code Ctrl makes of a character, or null when it makes none (the character is sent as it is). */
    fun control(cp: Int): Int? = when (cp) {
        in 'a'.code..'z'.code -> cp - 'a'.code + 1
        in 'A'.code..'Z'.code -> cp - 'A'.code + 1
        '@'.code, ' '.code -> 0
        in '['.code..'_'.code -> cp - '@'.code // [ \ ] ^ _ -> 27..31
        '?'.code -> 0x7f
        else -> null
    }
}

/**
 * What the soft keyboard types into a hidden text field, as terminal bytes. A text field that keeps no text: it always
 * holds [SENTINEL], so a backspace (the sentinel deleted) and typing (characters after it) both show in the change, and a
 * selection, a composition or autocorrect never leaves anything to sync. [reset] is what the field holds again after each change.
 */
object ImeBuffer {
    const val SENTINEL = "​"

    /**
     * The bytes for a change of the field from [old] to [new], and what it should hold now. Characters added after the old
     * text are typed; a shorter text is a Backspace for each character removed. A newline is an Enter.
     */
    fun change(old: String, new: String, ctrl: Boolean = false, alt: Boolean = false): Pair<ByteArray, String> {
        val common = old.commonPrefixWith(new)
        val removed = old.length - common.length
        val added = new.substring(common.length)
        val out = java.io.ByteArrayOutputStream()
        // A replaced composition (autocorrect swapped a word) is its deletions, then its text.
        repeat(removed) { out.write(KeyEncoder.key(Key.Backspace, alt = alt)) }
        for (part in added.split('\n').withIndex()) {
            if (part.index > 0) out.write(KeyEncoder.key(Key.Enter, alt = alt))
            if (part.value.isNotEmpty()) out.write(KeyEncoder.text(part.value, ctrl, alt))
        }
        return out.toByteArray() to SENTINEL
    }
}
