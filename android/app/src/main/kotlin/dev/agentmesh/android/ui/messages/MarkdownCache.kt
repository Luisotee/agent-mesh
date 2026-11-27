package dev.agentmesh.android.ui.messages

import com.mikepenz.markdown.model.State
import com.mikepenz.markdown.model.parseMarkdown
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext

/**
 * Parsed Markdown by its text, so a row of a list has its final height the moment it is composed and keeps it when it
 * scrolls out and back. The library's own `Markdown(text)` parses off the main thread and lays out empty until it has (a row
 * that grows from 0 as it scrolls into view), and forgets the parse when the row leaves composition.
 *
 * Bodies are parsed when they arrive ([warm], off the main thread), so composition is a hit; a body composed before its
 * parse finished is parsed in place when short ([SYNC_PARSE_MAX]) and shows a placeholder otherwise. Least recently used
 * goes first, by count and by the text it holds, so a long conversation does not keep every parse it ever showed.
 */
object MarkdownCache {
    const val MAX_ENTRIES = 300
    /** The parse of a text is several times its size: a cap by characters keeps a long conversation within a few MB. */
    const val MAX_CHARS = 300_000

    /** A body longer than this is not parsed ahead (one parse cannot be cut short): its row shows a placeholder until its own parse lands. */
    const val WARM_MAX_CHARS = 12_000
    const val WARM_BUDGET_MS = 250L

    private val lock = Any()
    private val map = LinkedHashMap<String, State.Success>(64, 0.75f, true)
    private var chars = 0

    fun get(text: String): State.Success? = synchronized(lock) { map[text] }

    /** The parse of [text], from the cache or made now (on the caller's thread); null when the parser failed. */
    fun parse(text: String): State.Success? {
        get(text)?.let { return it }
        val made = (try { parseMarkdown(text) } catch (e: Exception) { null }) as? State.Success ?: return null
        synchronized(lock) {
            if (map.put(text, made) == null) chars += text.length
            val it = map.entries.iterator()
            while ((map.size > MAX_ENTRIES || chars > MAX_CHARS) && map.size > 1 && it.hasNext()) {
                val eldest = it.next()
                chars -= eldest.key.length
                it.remove()
            }
        }
        return made
    }

    /**
     * Parses what has Markdown in it and isn't held yet, off the main thread, in the order given (what is read first goes first)
     * and for at most [budgetMs]: what is left is parsed when its row comes up, so showing a page never waits long on its parses.
     */
    suspend fun warm(texts: Collection<String>, budgetMs: Long = WARM_BUDGET_MS) = withContext(Dispatchers.Default) {
        val until = System.nanoTime() + budgetMs * 1_000_000
        for (t in texts) {
            ensureActive()
            if (System.nanoTime() > until) break
            if (t.length <= WARM_MAX_CHARS && hasMarkdown(t) && get(t) == null) parse(t)
        }
    }

    internal fun size() = synchronized(lock) { map.size }
    internal fun clear() = synchronized(lock) { map.clear(); chars = 0 }
}
