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
fun validateHost(name: String, url: String, token: String, pin: String, tokenRequired: Boolean): String? {
    if (name.isBlank()) return "Give the host a name"
    val trimmed = url.trim()
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

/** Adds a host, or edits one (when [hostId] is given). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HostFormScreen(hostId: String?, onDone: () -> Unit) {
    val container = LocalContainer.current
    val existing = hostId?.let { container.hosts.get(it) }
    val direct = existing?.routes?.firstOrNull() as? Route.Direct
    var name by rememberSaveable { mutableStateOf(existing?.name.orEmpty()) }
    var url by rememberSaveable { mutableStateOf(direct?.url ?: "https://") }
    var token by rememberSaveable { mutableStateOf("") }
    var pin by rememberSaveable { mutableStateOf(direct?.pinSha256.orEmpty()) }
    var error by rememberSaveable { mutableStateOf<String?>(null) }
    Scaffold(topBar = {
        TopAppBar(
            title = { Text(if (existing == null) "Add a host" else "Edit ${existing.name}") },
            navigationIcon = { IconButton(onClick = onDone) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } },
        )
    }) { pad ->
        Column(Modifier.padding(pad).padding(16.dp).verticalScroll(rememberScrollState()), verticalArrangement = androidx.compose.foundation.layout.Arrangement.spacedBy(12.dp)) {
            OutlinedTextField(name, { name = it }, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(
                url, { url = it }, label = { Text("Address") }, singleLine = true, modifier = Modifier.fillMaxWidth(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                supportingText = { Text("https://… for a host over Tailscale (tailscale serve) or behind your own proxy. http:// works only for localhost and the emulator's 10.0.2.2.") },
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
                val problem = validateHost(name, url, token, pin, tokenRequired = existing == null)
                if (problem != null) { error = problem; return@Button }
                val routes = listOf(Route.Direct(url.trim().trimEnd('/'), pin.ifBlank { null }))
                if (existing == null) container.hosts.add(name, routes, token) else {
                    container.hosts.update(existing.id, name, routes, token.ifBlank { null })
                    container.runtimes.value[existing.id]?.nudge()
                }
                onDone()
            }, modifier = Modifier.fillMaxWidth()) { Text(if (existing == null) "Add and connect" else "Save") }
        }
    }
}
