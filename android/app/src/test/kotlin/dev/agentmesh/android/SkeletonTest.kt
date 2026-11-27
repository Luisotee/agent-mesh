package dev.agentmesh.android

import androidx.compose.foundation.layout.Column
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithContentDescription
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.LocalReducedMotion
import dev.agentmesh.android.ui.SkeletonLines
import dev.agentmesh.android.ui.SkeletonRows
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class SkeletonTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))

    @Test fun `a skeleton says what is loading, moving or still`() {
        compose.setContent {
            AgentMeshTheme(theme) {
                Column {
                    SkeletonLines("Loading the conversation")
                    CompositionLocalProvider(LocalReducedMotion provides true) { SkeletonRows("Loading the messages") }
                }
            }
        }
        // A moving shimmer must not keep the test waiting (it draws on frames, never recomposes), and a still one is a flat fill.
        compose.waitForIdle()
        compose.onNodeWithContentDescription("Loading the conversation").assertIsDisplayed()
        compose.onNodeWithContentDescription("Loading the messages").assertIsDisplayed()
    }
}
