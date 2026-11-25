package dev.agentmesh.android.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.theme.ThemeStore
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.SectionHeader
import dev.agentmesh.android.ui.collectAsStateWithLifecycleCompat

/** This device's own preferences. They aren't on a host: a theme here changes only this phone. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DeviceSettingsScreen(onBack: () -> Unit) {
    val themes = LocalContainer.current.themes
    val choice by themes.choice.collectAsStateWithLifecycleCompat()
    Scaffold(topBar = {
        TopAppBar(title = { Text("Settings") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        LazyColumn(Modifier.padding(pad).fillMaxSize()) {
            item { SectionHeader("Theme") }
            item { ThemeRow(ThemeStore.SYSTEM, null, choice == ThemeStore.SYSTEM) { themes.choose(ThemeStore.SYSTEM) } }
            items(themes.available, key = { it.name }) { spec ->
                ThemeRow(spec.name, spec, choice == spec.name) { themes.choose(spec.name) }
            }
        }
    }
}

@Composable
private fun ThemeRow(name: String, spec: dev.agentmesh.android.theme.ThemeSpec?, selected: Boolean, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        RadioButton(selected, onClick = onClick)
        Text(if (name == ThemeStore.SYSTEM) "Follow the system (${Themes.DEFAULT} or Alucard)" else name, Modifier.weight(1f).padding(start = 8.dp))
        if (spec != null) {
            val t = Themes.resolve(spec)
            Row {
                listOf(t.scheme.background, t.scheme.primary, t.states.working, t.states.needs, t.states.done).forEach {
                    Box(Modifier.padding(start = 4.dp).size(16.dp).clip(CircleShape).background(it).border(1.dp, MaterialTheme.colorScheme.outline, CircleShape))
                }
            }
        }
    }
}
