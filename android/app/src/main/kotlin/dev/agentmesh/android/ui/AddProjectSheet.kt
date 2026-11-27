package dev.agentmesh.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime
import dev.agentmesh.android.chat.say
import dev.agentmesh.android.derive.repoName
import kotlinx.coroutines.CancellationException

/**
 * Adds a project to the drawer before any run works in it: the host's own projects (`ListProjects`) matching what is
 * typed, or a path typed whole. It is pinned on this device (the host keeps nothing), and stays until unpinned.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AddProjectSheet(runtime: HostRuntime, onDismiss: () -> Unit) {
    val prefs = LocalContainer.current.prefs
    val pinned = prefs.pins.collectAsStateWithLifecycleCompat().value[runtime.hostId].orEmpty()
    var query by remember { mutableStateOf("") }
    var projects by remember { mutableStateOf(emptyList<Pair<String, String>>()) }
    var error by remember { mutableStateOf("") }
    LaunchedEffect(Unit) {
        try { projects = runtime.awaitApi().listProjects().map { it.path to it.name.ifEmpty { repoName(it.path) } }.distinctBy { it.first } } catch (e: CancellationException) { throw e } catch (e: Exception) { error = say(e) }
    }
    val words = query.lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }
    val shown = projects.filter { (path, name) -> path !in pinned && words.all { it in name.lowercase() || it in path.lowercase() } }.take(30)
    // Only an absolute path: a project is matched to its runs by the exact path, and the host doesn't expand `~`.
    val typed = normalPath(query.trim()).takeIf { it.startsWith("/") || Regex("^[A-Za-z]:[\\\\/].*").matches(it) }
    fun add(path: String) { prefs.pin(runtime.hostId, path); prefs.unhide(dev.agentmesh.android.derive.projectKey(runtime.hostId, path)); onDismiss() }
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(Modifier.fillMaxWidth().navigationBarsPadding().imePadding().verticalScroll(androidx.compose.foundation.rememberScrollState()).padding(horizontal = 16.dp).testTag("add-project")) {
            Text("Add a project on ${runtime.host?.name ?: "the host"}", style = MaterialTheme.typography.titleMedium)
            Text("It stays in the drawer, pinned, before any run works in it.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(bottom = 8.dp))
            OutlinedTextField(query, { query = it }, Modifier.fillMaxWidth().testTag("project-query"), singleLine = true, label = { Text("A name, or an absolute path on the host") })
            if (error.isNotEmpty() && projects.isEmpty()) Text("Couldn't list the host's projects: $error", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(top = 8.dp))
            if (typed != null && typed !in pinned) Text("Add $typed", Modifier.fillMaxWidth().clickable { add(typed) }.padding(vertical = 12.dp).testTag("add-typed-path"), color = MaterialTheme.colorScheme.primary)
            LazyColumn(Modifier.padding(top = 8.dp)) {
                items(shown, key = { it.first }) { (path, name) ->
                    Column(Modifier.fillMaxWidth().clickable { add(path) }.padding(vertical = 8.dp).testTag("project-option")) {
                        Text(name)
                        Text(path, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                }
            }
        }
    }
}

/** A typed path as the runs name it: trimmed first, no trailing separator, no doubled one, no `.` segment (a drive path is only trimmed). */
internal fun normalPath(typed: String): String = typed.trim().let { p ->
    if (p.startsWith("/")) "/" + p.split('/').filter { it.isNotEmpty() && it != "." }.joinToString("/") else p.trimEnd('/', '\\')
}
