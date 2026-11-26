package dev.agentmesh.android

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.assertIsNotEnabled
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.assertCountEquals
import com.connectrpc.Code
import dev.agentmesh.android.hosts.UnpairResult
import dev.agentmesh.android.hosts.unpairFromHost
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.RelayRefused
import java.io.IOException
import dev.agentmesh.android.theme.Themes
import dev.agentmesh.android.ui.AgentMeshTheme
import dev.agentmesh.android.ui.RemoveHostDialog
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.io.File

/** Unpairing a phone from its host before the app forgets the host (`RevokeSelf`). */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [34])
class UnpairTest {
    @get:Rule val compose = createComposeRule()
    private val theme = Themes.resolve(Themes.parse(File("../../shared/themes/dracula.json").readText()))

    @Test fun `asking the host to revoke this device is done when it answers or already refuses the token`() = runBlocking {
        val api = RecordingApi()
        assertEquals(UnpairResult.Done, unpairFromHost(api))
        assertEquals(1, api.revokedSelf)
        // The host no longer knows the token (revoked before, or by its owner): nothing is left to revoke.
        api.failRevokeSelf = ApiException(Code.UNAUTHENTICATED, "bad token")
        assertEquals(UnpairResult.Done, unpairFromHost(api))
    }

    @Test fun `a retry the relay refuses because the relay token is already revoked is done`() = runBlocking {
        val api = RecordingApi()
        // OkHttp and ConnectRPC wrap what the socket threw: the refusal is found anywhere in the causes.
        api.failRevokeSelf = ApiException(Code.UNAVAILABLE, "transport", IOException("closed", RelayRefused("auth", "the relay refused this device's token")))
        assertEquals(UnpairResult.Done, unpairFromHost(api, afterFailedTry = true))
        // On a first try the relay's word is no proof (it says the same for a token it doesn't know): the user decides.
        assertEquals(UnpairResult.Failed("transport"), unpairFromHost(api))
        // Any other relay refusal is not proof the token is dead.
        api.failRevokeSelf = ApiException(Code.UNAVAILABLE, "transport", RelayRefused("busy", "try later"))
        assertEquals(UnpairResult.Failed("transport"), unpairFromHost(api, afterFailedTry = true))
    }

    @Test fun `a host that can't be reached or doesn't know the call is a failure the user decides about`() = runBlocking {
        assertEquals(UnpairResult.Failed("not connected to the host"), unpairFromHost(null))
        val api = RecordingApi()
        api.failRevokeSelf = ApiException(Code.UNAVAILABLE, "down")
        assertEquals(UnpairResult.Failed("down"), unpairFromHost(api))
        api.failRevokeSelf = ApiException(Code.UNIMPLEMENTED, "unknown method")
        assertEquals(UnpairResult.Failed("unknown method"), unpairFromHost(api))
        // A refusal of the call itself doesn't show the token is dead.
        api.failRevokeSelf = ApiException(Code.PERMISSION_DENIED, "not allowed")
        assertEquals(UnpairResult.Failed("not allowed"), unpairFromHost(api))
    }

    private fun show(canUnpair: Boolean, unpair: suspend (Boolean) -> UnpairResult, removed: () -> Unit, dismissed: () -> Unit = {}) =
        compose.setContent { AgentMeshTheme(theme) { RemoveHostDialog("Home", canUnpair, unpair, removed, dismissed) } }

    @Test fun `a connected host is unpaired first, then removed`() {
        var asked = 0
        var removed = 0
        show(true, { asked++; UnpairResult.Done }, { removed++ })
        compose.onNodeWithText("Also unpair this phone from the host").assertIsDisplayed()
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        assertEquals(1, asked)
        assertEquals(1, removed)
    }

    @Test fun `unticking the box removes the host here only, without asking it`() {
        var asked = 0
        var removed = 0
        show(true, { asked++; UnpairResult.Done }, { removed++ })
        compose.onNodeWithTag("also-unpair").performClick()
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        assertEquals(0, asked)
        assertEquals(1, removed)
    }

    @Test fun `a failed unpair keeps the host and offers to try again or to remove it here only`() {
        var asked = 0
        var removed = 0
        show(true, { asked++; UnpairResult.Failed("down") }, { removed++ })
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        compose.onNodeWithTag("unpair-error").assertIsDisplayed()
        compose.onNode(hasText("Couldn't unpair: down")).assertIsDisplayed()
        assertEquals(0, removed)
        compose.onNodeWithText("Try again").performClick()
        compose.waitForIdle()
        assertEquals(2, asked)
        compose.onNodeWithText("Remove here only").performClick()
        compose.waitForIdle()
        assertEquals(1, removed)
        assertEquals(2, asked)
    }

    @Test fun `a host that isn't connected can only be removed here, and the dialog says the token stays valid`() {
        var asked = 0
        var removed = 0
        show(false, { asked++; UnpairResult.Done }, { removed++ })
        compose.onAllNodesWithTag("also-unpair").assertCountEquals(0)
        compose.onNode(hasText("stays valid", substring = true)).assertIsDisplayed()
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        assertEquals(0, asked)
        assertEquals(1, removed)
    }

    @Test fun `after a failure and a dropped connection, try again does not remove the host, and remove here only does`() {
        var asked = 0
        var removed = 0
        var connected by mutableStateOf(true)
        compose.setContent { AgentMeshTheme(theme) { RemoveHostDialog("Home", connected, { asked++; UnpairResult.Failed("down") }, { removed++ }, {}) } }
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        assertEquals(1, asked)
        connected = false
        compose.waitForIdle()
        compose.onNodeWithText("Try again").assertIsNotEnabled()
        compose.onNodeWithText("Try again").performClick()
        compose.waitForIdle()
        assertEquals(0, removed)
        assertEquals(1, asked)
        compose.onNodeWithText("Remove here only").performClick()
        compose.waitForIdle()
        assertEquals(1, removed)
    }

    @Test fun `once a try failed there is no box to untick, so try again cannot remove without unpairing`() {
        var removed = 0
        show(true, { UnpairResult.Failed("down") }, { removed++ })
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        compose.onAllNodesWithTag("also-unpair").assertCountEquals(0)
        compose.onNodeWithText("Try again").performClick()
        compose.waitForIdle()
        assertEquals(0, removed)
    }

    @Test fun `a retry tells the call it follows a failed try, and the choices stay on screen while it runs`() {
        val tries = mutableListOf<Boolean>()
        val gate = kotlinx.coroutines.CompletableDeferred<UnpairResult>()
        show(true, { retry -> tries += retry; if (tries.size == 1) UnpairResult.Failed("down") else gate.await() }, {})
        compose.onNodeWithText("Remove").performClick()
        compose.waitForIdle()
        compose.onNodeWithText("Try again").performClick()
        compose.waitForIdle()
        assertEquals(listOf(false, true), tries)
        // The retry is in flight: still "Try again" and "Remove here only" (both disabled), no box to untick.
        compose.onNodeWithText("Try again").assertIsNotEnabled()
        compose.onNodeWithText("Remove here only").assertIsNotEnabled()
        compose.onAllNodesWithTag("also-unpair").assertCountEquals(0)
        gate.complete(UnpairResult.Done)
    }
}
