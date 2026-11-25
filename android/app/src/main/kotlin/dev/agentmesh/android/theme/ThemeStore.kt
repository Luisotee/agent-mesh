package dev.agentmesh.android.theme

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** The themes shipped in `assets/themes/`, and the one this device uses (a per-device choice kept in preferences). */
class ThemeStore(private val context: Context) {
    private val prefs = context.getSharedPreferences("prefs", Context.MODE_PRIVATE)

    val available: List<ThemeSpec> = (context.assets.list("themes") ?: emptyArray()).sorted()
        .map { Themes.parse(context.assets.open("themes/$it").bufferedReader().use { r -> r.readText() }) }

    private val _choice = MutableStateFlow(prefs.getString(KEY, Themes.DEFAULT) ?: Themes.DEFAULT)

    /** A theme's name, or [SYSTEM] to follow the system's dark or light mode. */
    val choice: StateFlow<String> get() = _choice

    fun choose(name: String) {
        prefs.edit().putString(KEY, name).apply()
        _choice.value = name
    }

    /** The theme to draw with, given the system's mode. */
    fun resolve(choice: String, systemDark: Boolean): MeshTheme {
        val wanted = if (choice == SYSTEM) (if (systemDark) Themes.DEFAULT else "Alucard") else choice
        val spec = available.firstOrNull { it.name == wanted } ?: available.first { it.name == Themes.DEFAULT }
        return Themes.resolve(spec)
    }

    companion object {
        const val SYSTEM = "System"
        private const val KEY = "theme"
    }
}
