package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.agentmesh.android.HostRuntime

/** The connection's own log (what it did and why), newest last: where to look when a host won't connect. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun LogScreen(runtime: HostRuntime, onBack: () -> Unit) {
    val state by runtime.state.collectAsStateWithLifecycleCompat()
    Scaffold(topBar = {
        TopAppBar(title = { Text("Connection log") }, navigationIcon = { IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") } })
    }) { pad ->
        if (state.log.isEmpty()) {
            Text("Nothing yet.", Modifier.padding(pad).padding(16.dp))
        } else {
            LazyColumn(Modifier.padding(pad).fillMaxSize(), reverseLayout = true) {
                items(state.log.asReversed()) {
                    Text("${clock(it.atMs)}  ${it.text}", Modifier.padding(horizontal = 16.dp, vertical = 4.dp), style = MonoSmall)
                }
            }
        }
    }
}
