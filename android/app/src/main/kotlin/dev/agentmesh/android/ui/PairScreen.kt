package dev.agentmesh.android.ui

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import dev.agentmesh.android.net.PairingException
import dev.agentmesh.android.net.PairingLink
import dev.agentmesh.android.net.PairingPayload
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.pair.QrScanner
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch

/** What the user is shown before they agree to pair: the host the code names and where the app would reach it. */
fun pairingSummary(p: PairingPayload): List<String> = buildList {
    add(p.hostName.ifEmpty { "A host" } + " (" + p.hostId.take(10) + ")")
    for (r in p.routes()) add(when (r) { is Route.Direct -> "Direct: ${r.url}"; is Route.Relay -> "Through your relay: ${r.relayTcp}" })
    add("Pinned certificate: " + p.pinSha256.take(16).chunked(4).joinToString(" ") + "…")
    for (s in p.skipped) add("Not used: $s")
}

/**
 * Pairing: a link from `meshctl pair` or the desktop's pair screen, pasted, opened from the phone (the
 * `agentmesh://pair` link) or scanned from its QR code. Nothing is sent until the user has seen which host the code
 * names and taps Pair; the link's one-time token is held only in this screen's memory (never in saved state), and the
 * link is dropped from the field once the host is saved.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PairScreen(initialLink: String?, onBack: () -> Unit, onPaired: (String) -> Unit) {
    val container = LocalContainer.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    // A plain remember, not rememberSaveable: the token mustn't land in saved instance state.
    var text by remember { mutableStateOf(initialLink.orEmpty()) }
    var deviceName by remember { mutableStateOf(android.os.Build.MODEL.orEmpty().ifEmpty { "Android phone" }) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var scanning by remember { mutableStateOf(false) }
    var cameraError by remember { mutableStateOf<String?>(null) }
    var notice by remember { mutableStateOf<String?>(null) }
    // A link opened from outside while this screen is up (or that brought the user here): taken once, and shown for confirmation.
    val incoming by container.pairLink.collectAsStateWithLifecycleCompat()
    // Not while a pairing is in flight: the link waits in the container, and is taken once the call has finished.
    LaunchedEffect(incoming, busy) {
        if (!busy) incoming?.let { text = it; error = null; notice = null; container.pairLink.value = null }
    }
    val parsed = remember(text) { if (text.isBlank()) null else runCatching { PairingLink.parse(text) } }
    val askCamera = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) { scanning = true; cameraError = null } else cameraError = "Without the camera permission the app can't scan. Paste the link instead."
    }
    fun scan() {
        cameraError = null
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) scanning = true
        else askCamera.launch(Manifest.permission.CAMERA)
    }
    fun pair(p: PairingPayload) {
        if (busy) return
        busy = true; error = null; notice = null
        val link = text
        scope.launch {
            try {
                val host = container.pairing.pair(p, deviceName)
                // A host paired again (a stopped connection with a refused token, new routes) picks up its new token and routes now.
                container.runtimes.value[host.id]?.reconnect()
                if (container.pairLink.value != null) {
                    // Another link came in during the call: it is the user's next step, so stay and show it.
                    text = container.pairLink.value.orEmpty(); container.pairLink.value = null
                    notice = "Paired with ${host.name}. Another pairing link came in: check it and tap Pair."
                } else {
                    if (text == link) text = ""
                    onPaired(host.id)
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: PairingException) {
                error = e.message
            } catch (e: Exception) {
                error = "Couldn't pair: ${e.message ?: e.javaClass.simpleName}"
            } finally {
                busy = false
            }
        }
    }
    Scaffold(topBar = {
        TopAppBar(title = { Text("Pair with a host") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        if (scanning) {
            Column(Modifier.padding(pad).fillMaxSize()) {
                Text("Point the camera at the QR code from meshctl pair.", Modifier.padding(16.dp))
                QrScanner(
                    onCode = { text = it; scanning = false }, onError = { cameraError = it; scanning = false },
                    modifier = Modifier.weight(1f).fillMaxWidth().testTag("scanner"),
                )
                TextButton(onClick = { scanning = false }, Modifier.padding(8.dp)) { Text("Cancel") }
            }
            return@Scaffold
        }
        Column(Modifier.padding(pad).padding(16.dp).fillMaxSize().verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(
                "On the host, run meshctl pair (or open Pair a device in the desktop app). Scan its QR code, or paste the agentmesh://pair link.",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedButton(onClick = ::scan, Modifier.fillMaxWidth().testTag("scan"), enabled = !busy) { Text("Scan the QR code") }
            cameraError?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            OutlinedTextField(
                text, { text = it; error = null }, Modifier.fillMaxWidth().testTag("link"), label = { Text("Pairing link") },
                placeholder = { Text("agentmesh://pair?v=1&…") }, minLines = 2, maxLines = 5, enabled = !busy,
            )
            val failure = parsed?.exceptionOrNull()
            if (failure != null) Text(failure.message.orEmpty(), color = MaterialTheme.colorScheme.error, modifier = Modifier.testTag("link-error"))
            val ok = parsed?.getOrNull()
            if (ok != null) {
                Column(Modifier.fillMaxWidth().testTag("summary"), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                    Text("This code is for", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    for (line in pairingSummary(ok)) Text(line)
                }
                OutlinedTextField(deviceName, { deviceName = it }, Modifier.fillMaxWidth(), label = { Text("Name of this device") }, singleLine = true, enabled = !busy)
            }
            notice?.let { Text(it, modifier = Modifier.testTag("paired-notice")) }
            error?.let { Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.testTag("pair-error")) }
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = androidx.compose.ui.Alignment.CenterVertically) {
                Button(onClick = { ok?.let(::pair) }, enabled = ok != null && !busy && deviceName.isNotBlank(), modifier = Modifier.testTag("pair")) { Text("Pair") }
                if (busy) CircularProgressIndicator(Modifier.height(24.dp))
            }
        }
    }
}
