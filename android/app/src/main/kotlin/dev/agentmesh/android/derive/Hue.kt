package dev.agentmesh.android.derive

import dev.agentmesh.android.theme.HUES

/**
 * A project's identity hue, 1 to 8: FNV-1a over the UTF-16 units of its repo path, the same function as the desktop's
 * `hueOf` (`desktop/src/hue.ts`), so a project keeps its colour across restarts, devices and both apps, and two
 * projects rarely share one. An empty path gets the first. `HueTest` pins vectors computed with the desktop's code.
 */
fun hueOf(repo: String): Int {
    if (repo.isEmpty()) return 1
    var h = 0x811c9dc5.toInt()
    for (c in repo) {
        h = h xor c.code
        h *= 0x01000193
    }
    return ((h.toLong() and 0xFFFFFFFFL) % HUES).toInt() + 1
}
