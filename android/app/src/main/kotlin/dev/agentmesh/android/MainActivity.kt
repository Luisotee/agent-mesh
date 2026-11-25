package dev.agentmesh.android

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.runtime.CompositionLocalProvider
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.AppNav
import dev.agentmesh.android.ui.LocalContainer
import dev.agentmesh.android.ui.rememberTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val container = (application as MeshApp).container
        setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                AgentMeshTheme(rememberTheme(container)) { AppNav() }
            }
        }
    }
}
