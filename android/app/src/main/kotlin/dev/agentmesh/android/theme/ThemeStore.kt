package dev.agentmesh.android.theme

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/**
 * The themes shipped in `assets/themes/` (the files in the repo's `shared/themes/`, which the desktop reads too), and
 * the one this device uses: its own choice kept in preferences, or by default the host's `app.theme` setting.
 */
class ThemeStore(private val context: Context) {
    private val prefs = context.getSharedPreferences("prefs", Context.MODE_PRIVATE)

    val available: List<ThemeSpec> = (context.assets.list("themes") ?: emptyArray()).sorted()
        .map { Themes.parse(context.assets.open("themes/$it").bufferedReader().use { r -> r.readText() }) }

    private val _choice = MutableStateFlow(prefs.getString(KEY, HOST) ?: HOST)

    /** A theme's name, [SYSTEM] to follow the system's dark or light mode, or [HOST] (the default) for the host's choice. */
    val choice: StateFlow<String> get() = _choice

    private val _host = MutableStateFlow<String?>(null)

    /** The theme the first host that names one has set (`app.theme`), or null. */
    val host: StateFlow<String?> get() = _host

    fun setHostTheme(name: String?) { _host.value = name }

    fun choose(name: String) {
        prefs.edit().putString(KEY, name).apply()
        _choice.value = name
    }

    /** The theme to draw with, given the system's mode. */
    fun resolve(choice: String, systemDark: Boolean, host: String? = null): MeshTheme {
        val wanted = if (choice == HOST) (host ?: Themes.DEFAULT) else if (choice == SYSTEM) (if (systemDark) Themes.DEFAULT else "Alucard") else choice
        val spec = available.firstOrNull { it.name == wanted } ?: available.first { it.name == Themes.DEFAULT }
        return Themes.resolve(spec)
    }

    companion object {
        const val SYSTEM = "System"
        const val HOST = "Host"
        private const val KEY = "theme"
    }
}
