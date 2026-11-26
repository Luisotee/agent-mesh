package dev.agentmesh.android

import android.Manifest
import android.app.Application
import android.app.NotificationManager
import android.app.RemoteInput
import android.content.Intent
import android.os.Bundle
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.net.MeshApi
import dev.agentmesh.android.net.Route
import dev.agentmesh.android.notify.ACTION_APPROVE
import dev.agentmesh.android.notify.ACTION_REPLY
import dev.agentmesh.android.notify.EXTRA_AGENT
import dev.agentmesh.android.notify.EXTRA_HOST
import dev.agentmesh.android.notify.KEY_REPLY
import dev.agentmesh.android.notify.Notice
import dev.agentmesh.android.notify.NoticeActionReceiver
import dev.agentmesh.android.notify.NoticeKind
import dev.agentmesh.android.notify.Notifier
import dev.agentmesh.android.notify.PushPayload
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.store.ConnState
import dev.agentmesh.gen.agentmesh.v1.AgentPhase
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.HelloResponse
import dev.agentmesh.gen.agentmesh.v1.MessageType
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import kotlinx.coroutines.launch
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config

/** The app with no Keystore and a host that records: what a notification's buttons do is checked end to end. */
class TestApp : MeshApp() {
    override fun createContainer(): AppContainer =
        AppContainer(this, Plain, newApi = { _, _ -> NotificationsTest.api }, push = FakePushChannel(), pushStore = MemoryEndpoints())
}

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class)
class NotificationsTest {
    companion object { var api = RecordingApi() }

    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private val container get() = (app as MeshApp).container
    private val manager get() = shadowOf(app.getSystemService(NotificationManager::class.java))

    private fun notice(kind: NoticeKind, agent: String = "w1") = Notice(kind, "shop-fix sent a plan", "Change the typo", agent, Tab.Messages)

    @Test fun `a plan notification has Approve, a question has an inline Reply, and a tap opens the agent`() {
        api = RecordingApi()
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val n = Notifier(app)
        n.post("h1", null, notice(NoticeKind.Plan))
        val plan = manager.allNotifications.single()
        assertEquals("shop-fix sent a plan", plan.extras.getString("android.title"))
        assertEquals(listOf("Approve"), plan.actions.map { it.title.toString() })
        // The tap leads to the agent's messages.
        val tap = shadowOf(plan.contentIntent).savedIntent
        assertEquals("w1", tap.getStringExtra(EXTRA_AGENT)); assertEquals("h1", tap.getStringExtra(EXTRA_HOST)); assertEquals("Messages", tap.getStringExtra("tab"))

        n.post("h1", null, notice(NoticeKind.Question, agent = "w2"))
        val q = manager.allNotifications.first { it.actions?.any { a -> a.title == "Reply" } == true }
        assertEquals(1, q.actions.single().remoteInputs.size)
        assertEquals(KEY_REPLY, q.actions.single().remoteInputs.single().resultKey)

        // A reports notification has no buttons, and a later one for the same agent replaces the earlier.
        n.post("h1", null, notice(NoticeKind.Report))
        assertEquals(2, manager.allNotifications.size)
        assertNull(manager.allNotifications.first { it.extras.getString("android.title") == "shop-fix sent a plan" }.actions)
        n.cancel("h1", "w1")
        assertEquals(1, manager.allNotifications.size)
    }

    @Test fun `without the permission nothing is posted`() {
        shadowOf(app).denyPermissions(Manifest.permission.POST_NOTIFICATIONS)
        Notifier(app).post("h1", null, notice(NoticeKind.Plan))
        assertEquals(0, manager.allNotifications.size)
    }

    private val plan = message("p1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "Change the typo", at = 10_000)
    private val question = message("q1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_QUESTION, body = "Which branch?", at = 10_000)

    /** A connected host whose snapshot is in and whose stored history (what [api] pages) is loaded. */
    private fun readyHost(): Pair<String, HostRuntime> {
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = run {
            var r: HostRuntime? = null
            val deadline = System.currentTimeMillis() + 5_000
            while (r == null && System.currentTimeMillis() < deadline) { r = container.runtimes.value[host.id]; Thread.sleep(20) }
            r!!
        }
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT)
        val worker = agent("w1", parent = "lead", name = "shop-fix").toBuilder().setPhase(AgentPhase.AGENT_PHASE_PLANNING).build()
        rt.onFrame(snapshot(listOf(run("r1")), listOf(lead, worker)))
        waitFor("the history to load") { "r1" in rt.state.value.messagesLoaded }
        return host.id to rt
    }

    private fun waitFor(what: String, cond: () -> Boolean) {
        val deadline = System.currentTimeMillis() + 5_000
        while (!cond()) { check(System.currentTimeMillis() < deadline) { "timed out: $what" }; shadowOf(android.os.Looper.getMainLooper()).idle(); Thread.sleep(20) }
    }

    @Test fun `the Approve button approves the plan through the registry and clears the notification`() {
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(plan))) }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, _) = readyHost()
        container.notifier.post(hostId, null, notice(NoticeKind.Plan))
        app.sendBroadcast(Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_APPROVE).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, "w1"))
        waitFor("ApproveAgent") { api.approved.isNotEmpty() }
        assertEquals(listOf("w1"), api.approved)
        waitFor("the notification to clear") { manager.allNotifications.isEmpty() }
    }

    @Test fun `typing into Reply answers the question`() {
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(question))) }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, _) = readyHost()
        val reply = Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_REPLY).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, "w1")
        RemoteInput.addResultsToIntent(arrayOf(RemoteInput.Builder(KEY_REPLY).build()), reply, Bundle().apply { putCharSequence(KEY_REPLY, "use main") })
        app.sendBroadcast(reply)
        waitFor("the answer") { api.sent.isNotEmpty() }
        assertEquals(listOf(Triple("w1", MessageType.MESSAGE_TYPE_ANSWER, "use main")), api.sent)
    }

    @Test fun `a failed action says so in the notification instead of failing silently`() {
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(question))); failSend = IllegalStateException("agent cap reached") }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, _) = readyHost()
        val reply = Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_REPLY).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, "w1")
        RemoteInput.addResultsToIntent(arrayOf(RemoteInput.Builder(KEY_REPLY).build()), reply, Bundle().apply { putCharSequence(KEY_REPLY, "x") })
        app.sendBroadcast(reply)
        waitFor("the error notification") { manager.allNotifications.isNotEmpty() }
        val shown = manager.allNotifications.single()
        assertTrue(shown.extras.getString("android.title")!!.startsWith("Couldn't answer"))
        assertEquals("agent cap reached", shown.extras.getString("android.text"))
    }

    @Test fun `live events become notices on the runtime, and the host's settings can silence a kind`() {
        api = RecordingApi()
        val (_, rt) = readyHost()
        val got = mutableListOf<Notice>()
        val job = kotlinx.coroutines.CoroutineScope(kotlinx.coroutines.Dispatchers.Unconfined).launch { rt.notices.collect { got += it } }
        rt.onFrame(messageSent(1, message("m", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "go", at = System.currentTimeMillis()), at = System.currentTimeMillis()))
        waitFor("the notice") { got.isNotEmpty() }
        assertEquals(NoticeKind.Plan, got.single().kind)
        job.cancel()
    }

    /** The process was just started for the tap: the host answers Hello, but the snapshot and history come later. */
    @Test fun `Approve on a cold start waits for the snapshot and the history, then sends ApproveAgent`() {
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(plan))) }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = run {
            var r: HostRuntime? = null
            val deadline = System.currentTimeMillis() + 5_000
            while (r == null && System.currentTimeMillis() < deadline) { r = container.runtimes.value[host.id]; Thread.sleep(20) }
            r!!
        }
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        app.sendBroadcast(Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_APPROVE).putExtra(EXTRA_HOST, host.id).putExtra(EXTRA_AGENT, "w1"))
        // Connected, nothing known yet: a plain "approved" would be wrong, so nothing is sent.
        Thread.sleep(500); shadowOf(android.os.Looper.getMainLooper()).idle()
        assertEquals(emptyList<String>(), api.approved)
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("lead", role = AgentRole.AGENT_ROLE_ROOT), agent("w1", parent = "lead", name = "shop-fix").toBuilder().setPhase(AgentPhase.AGENT_PHASE_PLANNING).build())))
        waitFor("ApproveAgent") { api.approved.isNotEmpty() }
        assertEquals(listOf("w1"), api.approved)
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
    }

    @Test fun `a Reply to a question that was answered elsewhere sends nothing and clears the notification`() {
        val answered = message("a1", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_ANSWER, body = "main", at = 11_000)
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(question, answered))) }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, _) = readyHost()
        container.notifier.post(hostId, null, notice(NoticeKind.Question))
        val reply = Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_REPLY).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, "w1")
        RemoteInput.addResultsToIntent(arrayOf(RemoteInput.Builder(KEY_REPLY).build()), reply, Bundle().apply { putCharSequence(KEY_REPLY, "use dev") })
        app.sendBroadcast(reply)
        waitFor("the notification to clear") { manager.allNotifications.isEmpty() }
        assertEquals(emptyList<Triple<String, MessageType, String>>(), api.sent)
    }

    @Test fun `nothing is posted for the agent whose screen the user is looking at`() {
        api = RecordingApi()
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, rt) = readyHost()
        container.viewing.value = hostId to "w1"
        val now = System.currentTimeMillis()
        rt.onFrame(messageSent(1, message("p9", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "go", at = now), at = now))
        Thread.sleep(400); shadowOf(android.os.Looper.getMainLooper()).idle()
        assertEquals(0, manager.allNotifications.size)
        container.viewing.value = null
        rt.onFrame(messageSent(2, message("p10", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "go", at = now), at = now))
        waitFor("the notification") { manager.allNotifications.isNotEmpty() }
    }

    @Test fun `a timeout on a host that is up says the messages didn't load, not that the host is unreachable`() {
        api = RecordingApi().apply { pages = mapOf("r1" to listOf(listOf(plan))); failListings = 1_000 }
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t")
        val rt = run {
            var r: HostRuntime? = null
            val deadline = System.currentTimeMillis() + 5_000
            while (r == null && System.currentTimeMillis() < deadline) { r = container.runtimes.value[host.id]; Thread.sleep(20) }
            r!!
        }
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        rt.onFrame(snapshot(listOf(run("r1")), listOf(agent("lead", role = AgentRole.AGENT_ROLE_ROOT), agent("w1", parent = "lead", name = "shop-fix"))))
        val was = NoticeActionReceiver.timeoutMs
        NoticeActionReceiver.timeoutMs = 600
        try {
            app.sendBroadcast(Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_APPROVE).putExtra(EXTRA_HOST, host.id).putExtra(EXTRA_AGENT, "w1"))
            waitFor("the error notification") { manager.allNotifications.isNotEmpty() }
            val shown = manager.allNotifications.single()
            assertEquals("Couldn't approve the plan", shown.extras.getString("android.title"))
            assertTrue(shown.extras.getString("android.text")!!, shown.extras.getString("android.text")!!.contains("messages"))
        } finally { NoticeActionReceiver.timeoutMs = was }
        assertEquals(emptyList<String>(), api.approved)
    }

    @Test fun `Approve with nothing waiting says nothing was sent, instead of vanishing`() {
        api = RecordingApi()
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val (hostId, _) = readyHost()
        app.sendBroadcast(Intent(app, NoticeActionReceiver::class.java).setAction(ACTION_APPROVE).putExtra(EXTRA_HOST, hostId).putExtra(EXTRA_AGENT, "w1"))
        waitFor("the error notification") { manager.allNotifications.isNotEmpty() }
        val shown = manager.allNotifications.single()
        assertTrue(shown.extras.getString("android.text")!!.endsWith("nothing was sent."))
        assertEquals(emptyList<String>(), api.approved)
    }

    // ── Push ──

    private fun pushed(seq: Long, kind: String = "plan", agent: String = "w1", daemon: String = "") =
        PushPayload(v = 1, daemon = daemon, seq = seq, kind = kind, title = "shop-fix sent a plan", body = "Change the typo", agent = agent, tab = "messages", at = 1)

    @Test fun `a push posts the notice with its actions, once even when the live stream brings it too`() {
        api = RecordingApi()
        shadowOf(app).grantPermissions(Manifest.permission.POST_NOTIFICATIONS)
        val host = container.hosts.add("Home", listOf(Route.Direct("http://127.0.0.1:1")), "t", daemonId = "d1")
        // Push off: a push the server still held posts nothing.
        container.onPush(host.id, pushed(6, daemon = "d1"))
        assertEquals(0, manager.allNotifications.size)
        container.prefs.setPush(true)
        container.onPush(host.id, pushed(7, daemon = "d1"))
        val n = manager.allNotifications.single()
        assertEquals("shop-fix sent a plan", n.extras.getString("android.title"))
        assertEquals(listOf("Approve"), n.actions.map { it.title.toString() })
        // The same event again (the live stream's copy, or the push service's retry) alerts no second time.
        app.getSystemService(NotificationManager::class.java).cancelAll()
        container.notifier.post(host.id, null, Notice(NoticeKind.Plan, "shop-fix sent a plan", "x", "w1", Tab.Messages, seq = 7))
        container.onPush(host.id, pushed(7, daemon = "d1"))
        assertEquals(0, manager.allNotifications.size)
        // A push from another daemon than this host's, and one for the agent on screen, post nothing.
        container.onPush(host.id, pushed(8, daemon = "someone-else"))
        container.viewing.value = host.id to "w1"
        container.onPush(host.id, pushed(9, daemon = "d1"))
        assertEquals(0, manager.allNotifications.size)
        container.viewing.value = null
        container.onPush(host.id, pushed(0, kind = "test", agent = "", daemon = "d1"))
        assertEquals("Change the typo", manager.allNotifications.single().extras.getString("android.text"))
    }

    @Test fun `push registers each host's endpoint on every ready connection, and forgets a removed host`() {
        api = RecordingApi()
        val (hostId, rt) = readyHost()
        val endpoint = dev.agentmesh.android.notify.SavedEndpoint("https://ntfy.example/upA?up=1", "pub", "auth")
        // Off: an endpoint is kept but nothing is sent.
        container.pushRegistrar.onEndpoint(hostId, endpoint)
        Thread.sleep(100)
        assertTrue(api.push.isEmpty())
        // On (through the distributor the user picked): every host registers, and the endpoint reaches the host.
        val channel = container.push as FakePushChannel
        val activity = org.robolectric.Robolectric.buildActivity(android.app.Activity::class.java).get()
        var ok: Boolean? = null
        container.enablePush(activity) { ok = it }
        assertEquals(true, ok)
        assertEquals(listOf(hostId), channel.registered.toList())
        waitFor("RegisterPush") { api.push.toList() == listOf("register https://ntfy.example/upA?up=1") }
        assertEquals(true, container.pushRegistrar.status.value[hostId]?.ok)
        // A reconnect registers again (a host that lost it heals).
        rt.onState(ConnState.Reconnecting(1, 0, "gone"))
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        waitFor("RegisterPush again") { api.push.size == 2 }
        // A refusal shows on the settings screen.
        api.failPush = dev.agentmesh.android.net.ApiException(com.connectrpc.Code.INVALID_ARGUMENT, "endpoint must be https")
        rt.onState(ConnState.Reconnecting(1, 0, "gone"))
        rt.onState(ConnState.Ready(HelloResponse.getDefaultInstance()))
        waitFor("the refusal") { container.pushRegistrar.status.value[hostId]?.ok == false }
        assertTrue(container.pushRegistrar.status.value[hostId]!!.text.contains("endpoint must be https"))
        // Removing the host drops its instance at the distributor.
        container.hosts.remove(hostId)
        waitFor("the unregister") { hostId in channel.unregistered }
        assertNull(container.pushRegistrar.status.value[hostId])
    }

    @Test fun `turning push off tells the host and the distributor`() {
        api = RecordingApi()
        val (hostId, _) = readyHost()
        val channel = container.push as FakePushChannel
        container.enablePush(org.robolectric.Robolectric.buildActivity(android.app.Activity::class.java).get()) {}
        container.disablePush()
        waitFor("UnregisterPush") { "unregister" in api.push && hostId in channel.unregistered }
        assertEquals(false, container.prefs.push.value)
    }

    @Test fun `with no distributor installed push stays off`() {
        api = RecordingApi()
        val channel = container.push as FakePushChannel
        channel.installed = emptyList()
        var ok: Boolean? = null
        container.enablePush(org.robolectric.Robolectric.buildActivity(android.app.Activity::class.java).get()) { ok = it }
        assertEquals(false, ok)
        assertEquals(false, container.prefs.push.value)
    }
}
