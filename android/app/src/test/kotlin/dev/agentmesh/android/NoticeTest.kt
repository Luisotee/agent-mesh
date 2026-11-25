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
    private val lead = agent("lead", role = AgentRole.AGENT_ROLE_ROOT, name = "shop-lead")
    private val worker = agent("w1", parent = "lead", name = "shop-fix")
    private val agents = listOf(lead, worker).associateBy { it.id }
    private val now = 100_000L

    private fun notice(f: SubscribeEventsResponse, skew: Long = 0, at: Long = now) = noticeFor(f.event, agents, at, skew)

    @Test fun `an agent stopped at a prompt notifies once, at the transition`() {
        val n = notice(stateChanged(1, "w1", AgentState.AGENT_STATE_WORKING, AgentState.AGENT_STATE_NEEDS_INPUT, "Bash", 1, at = now))!!
        assertEquals(NoticeKind.Input, n.kind); assertEquals("shop-fix needs you", n.title); assertEquals("Bash", n.body); assertEquals(Tab.Terminal, n.tab)
        // Still waiting, or a different state: nothing.
        assertNull(notice(stateChanged(2, "w1", AgentState.AGENT_STATE_NEEDS_INPUT, AgentState.AGENT_STATE_NEEDS_INPUT, at = now)))
        assertNull(notice(stateChanged(3, "w1", AgentState.AGENT_STATE_IDLE, AgentState.AGENT_STATE_WORKING, at = now)))
    }

    @Test fun `what reaches the lead notifies, with the kind of report`() {
        fun kind(t: MessageType) = notice(messageSent(1, message("m", from = "w1", to = "lead", type = t, body = "# Plan\n- **do** `x`"), at = now))
        assertEquals(NoticeKind.Plan, kind(MessageType.MESSAGE_TYPE_PLAN)!!.kind)
        assertEquals("Plan do x", kind(MessageType.MESSAGE_TYPE_PLAN)!!.body)
        assertEquals(NoticeKind.Question, kind(MessageType.MESSAGE_TYPE_QUESTION)!!.kind)
        assertEquals(NoticeKind.Report, kind(MessageType.MESSAGE_TYPE_DONE)!!.kind)
        assertEquals(NoticeKind.Failed, kind(MessageType.MESSAGE_TYPE_FAILED)!!.kind)
        assertEquals(Tab.Messages, kind(MessageType.MESSAGE_TYPE_DONE)!!.tab)
        assertNull(kind(MessageType.MESSAGE_TYPE_INFO))
    }

    @Test fun `talk among workers and what the user did themselves are not news`() {
        assertNull(notice(messageSent(1, message("m", from = "lead", to = "w1", type = MessageType.MESSAGE_TYPE_PLAN), at = now)))
        val byDevice = message("m", from = "me", to = "lead", type = MessageType.MESSAGE_TYPE_QUESTION, at = now)
            .toBuilder().setFrom(Actor.newBuilder().setClass_(ActorClass.ACTOR_CLASS_DEVICE).setId("w1")).build()
        assertNull(notice(messageSent(1, byDevice, at = now)))
        assertNull(notice(messageSent(1, message("m", from = "ghost", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN), at = now)))
    }

    @Test fun `history replayed after a reconnect stays quiet, judged by the host's clock`() {
        val e = stateChanged(1, "w1", AgentState.AGENT_STATE_WORKING, AgentState.AGENT_STATE_NEEDS_INPUT, at = 30_000)
        assertNull(notice(e))
        // The host's clock is 80 s behind ours: by its own clock the event is only 10 s old, so it is live.
        assertEquals(NoticeKind.Input, noticeFor(e.event, agents, 100_000, clockSkewMs = -80_000)!!.kind)
    }

    @Test fun `a death is news unless the user stopped the agent`() {
        assertEquals("shop-fix exited with code 2", noticeFor(exited(1, "w1", 2).event.toBuilder().setTime(ts(now)).build(), agents, now)!!.title)
        assertEquals("shop-fix was lost", noticeFor(exited(1, "w1", 0, "lost").event.toBuilder().setTime(ts(now)).build(), agents, now)!!.title)
        assertEquals(NoticeKind.Exited, noticeFor(exited(1, "w1", 0).event.toBuilder().setTime(ts(now)).build(), agents, now)!!.kind)
        assertNull(noticeFor(exited(1, "w1", -1, "killed").event.toBuilder().setTime(ts(now)).build(), agents, now))
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
