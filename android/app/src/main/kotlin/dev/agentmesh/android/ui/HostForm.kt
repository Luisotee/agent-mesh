package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.net.DefaultRouteConnector
import dev.agentmesh.android.net.Fingerprint
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.net.RouteException

/** What a form field set says is wrong with a host being added; null when it can be saved. */
fun validateHost(name: String, url: String, token: String, pin: String, tokenRequired: Boolean, addressOptional: Boolean = false): String? {
    if (name.isBlank()) return "Give the host a name"
    val trimmed = url.trim()
    if (addressOptional && isNoAddress(trimmed)) return if (tokenRequired && token.isBlank()) "Paste the device token" else null
    if (!trimmed.startsWith("http://") && !trimmed.startsWith("https://")) return "The address starts with https:// (or http:// for this device)"
    if (tokenRequired && token.isBlank()) return "Paste the device token"
    if (pin.isNotBlank() && !Fingerprint.isValid(pin)) return "The pinned fingerprint is 64 hex digits (SHA-256)"
    return try {
        DefaultRouteConnector().connect(Route.Direct(trimmed, pin.ifBlank { null })) { "" }
        null
    } catch (e: RouteException) {
        e.message
    }
}

/** An address field left empty, or holding only the scheme the form starts with. */
private fun isNoAddress(url: String) = url.isBlank() || url == "https://" || url == "http://"

/**
 * The routes a save gives a host: the address replaces its first direct route (or goes first when it has none), and every other route (a
 * relay) is kept. An empty address on a host that has a relay route keeps its routes as they are, so a relay-only host can be renamed.
 */
fun editedRoutes(existing: List<Route>?, url: String, pin: String): List<Route> {
    val trimmed = url.trim()
    val others = existing.orEmpty().toMutableList()
    if (isNoAddress(trimmed) && others.any { it is Route.Relay }) return others
    val edited = Route.Direct(trimmed.trimEnd('/'), pin.ifBlank { null })
    val at = others.indexOfFirst { it is Route.Direct }
    if (at >= 0) others[at] = edited else others.add(0, edited)
    return others
}

/** Adds a host, or edits one (when [hostId] is given). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostFormScreen(hostId: String?, onDone: () -> Unit) {
    val container = LocalContainer.current
    val existing = hostId?.let { container.hosts.get(it) }
    val direct = existing?.routes?.firstOrNull { it is Route.Direct } as? Route.Direct
    var name by rememberSaveable { mutableStateOf(existing?.name.orEmpty()) }
    // A host reached through a relay needs no address: empty keeps its routes.
    val relayed = existing?.routes.orEmpty().any { it is Route.Relay }
    var url by rememberSaveable { mutableStateOf(direct?.url ?: if (relayed) "" else "https://") }
    // Not saveable: the plain token must not go into the activity's saved state.
    var token by remember { mutableStateOf("") }
    var pin by rememberSaveable { mutableStateOf(direct?.pinSha256.orEmpty()) }
    var error by rememberSaveable { mutableStateOf<String?>(null) }
    Scaffold(topBar = {
        TopAppBar(
            title = { Text(if (existing == null) "Add a host" else "Edit ${existing.name}") },
            navigationIcon = { IconButton(onClick = onDone) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
        )
    }) { pad ->
        Column(Modifier.inScaffoldWithKeyboard(pad).padding(16.dp).verticalScroll(rememberScrollState()), verticalArrangement = androidx.compose.foundation.layout.Arrangement.spacedBy(12.dp)) {
            OutlinedTextField(name, { name = it }, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(
                url, { url = it }, label = { Text(if (relayed) "Address (optional: it has a relay route)" else "Address") }, singleLine = true, modifier = Modifier.fillMaxWidth(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                supportingText = { Text("https://… for a host on your tailnet (its 100.x address or its name.ts.net: the Tailscale app must be on), behind your own proxy, or on your network. http:// works only for localhost and the emulator's 10.0.2.2.") },
            )
            OutlinedTextField(
                token, { token = it }, label = { Text(if (existing == null) "Device token" else "Device token (empty keeps it)") }, singleLine = true,
                modifier = Modifier.fillMaxWidth(), visualTransformation = PasswordVisualTransformation(),
                supportingText = { Text("The contents of device.token in the host's state dir.") },
            )
            OutlinedTextField(
                pin, { pin = it }, label = { Text("Pinned certificate SHA-256 (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth(),
                supportingText = { Text("For a host with its own certificate: only that certificate is accepted.") },
            )
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            Button(onClick = {
                val problem = validateHost(name, url, token, pin, tokenRequired = existing == null, addressOptional = relayed)
                if (problem != null) { error = problem; return@Button }
                val routes = editedRoutes(existing?.routes, url, pin)
                if (existing == null) container.hosts.add(name, routes, token) else {
                    container.hosts.update(existing.id, name, routes, token.ifBlank { null })
                    container.runtimes.value[existing.id]?.reconnect()
                }
                onDone()
            }, modifier = Modifier.fillMaxWidth()) { Text(if (existing == null) "Add and connect" else "Save") }
        }
    }
}
