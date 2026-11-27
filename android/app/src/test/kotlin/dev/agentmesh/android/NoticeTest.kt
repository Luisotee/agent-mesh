package dev.agentmesh.android

import dev.agentmesh.android.notify.NoticeKind
import dev.agentmesh.android.notify.Tab
import dev.agentmesh.android.notify.brief
import dev.agentmesh.android.notify.noticeFor
import dev.agentmesh.android.notify.notifies
import dev.agentmesh.android.store.HostState
import dev.agentmesh.android.net.toValue
import dev.agentmesh.gen.agentmesh.v1.Actor
import dev.agentmesh.gen.agentmesh.v1.ActorClass
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.Setting
import dev.agentmesh.gen.agentmesh.v1.SubscribeEventsResponse
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class NoticeTest {
    private val now = 100_000L

    private fun notice(f: SubscribeEventsResponse, skew: Long = 0, at: Long = now) = noticeFor(f.event, at, skew)

    @Test fun `only the host decision notifies, a UserNotice for the user and live`() {
        val n = notice(userNotice(1, "plan", title = "shop-fix sent a plan", body = "Change the typo", at = now))!!
        assertEquals(NoticeKind.Plan, n.kind); assertEquals("shop-fix sent a plan", n.title); assertEquals("Change the typo", n.body)
        assertEquals("w1", n.agentId); assertEquals(Tab.Chat, n.tab); assertEquals(1L, n.seq); assertEquals("w1", n.tag)
        assertEquals(Tab.Terminal, notice(userNotice(2, "input", tab = "terminal", at = now))!!.tab)
        assertEquals("run-1", notice(userNotice(3, "finished", tag = "run-1", at = now))!!.tag)
    }

    @Test fun `a notice that is not for the user, one of a kind this app does not know, and one about no agent are the Activity list's alone`() {
        assertNull(notice(userNotice(1, "plan", forUser = false, at = now)))
        assertNull(notice(userNotice(2, "party", at = now)))
        assertNull(notice(userNotice(3, "plan", agentId = "", at = now).let { f -> SubscribeEventsResponse.newBuilder().setEvent(f.event.toBuilder().setUserNotice(f.event.userNotice.toBuilder().setAgentId(""))).build() }))
    }

    @Test fun `the kinds the daemon added, finished and ready, notify and are on by default`() {
        assertEquals(NoticeKind.Finished, notice(userNotice(1, "finished", at = now))!!.kind)
        assertEquals(NoticeKind.Ready, notice(userNotice(2, "ready", at = now))!!.kind)
        assertTrue(notifies(HostState(), NoticeKind.Finished)); assertTrue(notifies(HostState(), NoticeKind.Ready))
    }

    @Test fun `what the app used to work out itself from states and messages is the host's to say now`() {
        // An agent at a prompt, a plan to a lead, a death: events of their own, no notice here (the host sends a UserNotice).
        assertNull(notice(stateChanged(1, "w1", AgentState.AGENT_STATE_WORKING, AgentState.AGENT_STATE_NEEDS_INPUT, "Bash", 1, at = now)))
        assertNull(notice(messageSent(2, message("m", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, body = "go"), at = now)))
        assertNull(noticeFor(exited(3, "w1", 2).event.toBuilder().setTime(ts(now)).build(), now))
    }

    @Test fun `history replayed after a reconnect stays quiet, judged by the host's clock`() {
        val e = userNotice(1, "input", at = 30_000)
        assertNull(notice(e))
        // The host's clock is 80 s behind ours: by its own clock the event is only 10 s old, so it is live.
        assertEquals(NoticeKind.Input, noticeFor(e.event, 100_000, clockSkewMs = -80_000)!!.kind)
    }

    @Test fun `this phone's own list of kinds decides, the host's silencing still wins`() {
        val host = HostState(settings = mapOf("app.notify.events" to setting("app.notify.events", "[\"question\"]")))
        assertTrue(notifies(host, NoticeKind.Question)); assertTrue(!notifies(host, NoticeKind.Plan))
        // Its own list replaces the host's.
        assertTrue(notifies(host, NoticeKind.Plan, setOf("plan", "limited"))); assertTrue(notifies(host, NoticeKind.Limited, setOf("plan", "limited")))
        assertTrue(!notifies(host, NoticeKind.Question, setOf("plan")))
        // No list (or an empty one) follows the host.
        assertTrue(notifies(host, NoticeKind.Question, null)); assertTrue(notifies(host, NoticeKind.Question, emptySet()))
        // The host's own off switch silences everything.
        val off = host.copy(settings = host.settings + ("app.notify.enabled" to setting("app.notify.enabled", "false")))
        assertTrue(!notifies(off, NoticeKind.Plan, setOf("plan")))
        // The default list, before the host's settings arrive, is the daemon's (it has error, not limited).
        assertTrue(notifies(HostState(), NoticeKind.Error)); assertTrue(!notifies(HostState(), NoticeKind.Limited)); assertTrue(!notifies(HostState(), NoticeKind.Exited))
    }

    @Test fun `bodies are plain text, cut to a notification's size`() {
        assertEquals("a b c", brief("## a\n\n> b\n1. c"))
        assertEquals(200, brief("x".repeat(500)).length)
        // Paired marks go; a lone `*` or backtick is the author's own character.
        assertEquals("Change the typo in main.go", brief("Change **the typo** in `main.go`"))
        assertEquals("does 2*3 equal 6?", brief("does 2*3 equal 6?"))
        assertEquals("2*3 and 4*5", brief("2*3 and 4*5"))
        assertEquals("a *star and a ` tick", brief("a *star and a ` tick"))
        assertEquals("run it", brief("```sh\nrun it\n```"))
        // A huge body isn't scanned whole.
        val t0 = System.nanoTime()
        assertEquals(200, brief("a *b* ".repeat(500_000)).length)
        assertTrue("took ${(System.nanoTime() - t0) / 1_000_000} ms", System.nanoTime() - t0 < 2_000_000_000L)
    }

    private fun setting(key: String, json: String) =
        Setting.newBuilder().setKey(key).setValue(Json.parseToJsonElement(json).toValue()).build()

    @Test fun `the host's notify settings decide, with the same defaults before they arrive`() {
        val bare = HostState()
        assertTrue(notifies(bare, NoticeKind.Plan)); assertTrue(notifies(bare, NoticeKind.Failed))
        assertEquals(false, notifies(bare, NoticeKind.Exited))
        val off = HostState(settings = mapOf("app.notify.enabled" to setting("app.notify.enabled", "false")))
        assertEquals(false, notifies(off, NoticeKind.Plan))
        val only = HostState(settings = mapOf("app.notify.events" to setting("app.notify.events", """["question","exited"]""")))
        assertTrue(notifies(only, NoticeKind.Question)); assertTrue(notifies(only, NoticeKind.Exited)); assertEquals(false, notifies(only, NoticeKind.Plan))
    }
}
