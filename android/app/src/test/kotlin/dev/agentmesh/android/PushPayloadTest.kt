package dev.agentmesh.android

import dev.agentmesh.android.notify.NoticeKind
import dev.agentmesh.android.notify.PushPayload
import dev.agentmesh.android.notify.SavedEndpoint
import dev.agentmesh.android.notify.Tab
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PushPayloadTest {
    private fun parse(json: String) = PushPayload.parse(json.toByteArray())

    @Test fun `a host's push becomes the notice the live stream would have posted`() {
        val p = parse("""{"v":1,"daemon":"d1","seq":42,"kind":"plan","title":"shop-fix sent a plan","body":"Plan do x","agent":"w1","tab":"messages","at":1,"new":"ignored"}""")!!
        val n = p.toNotice()!!
        assertEquals(NoticeKind.Plan, n.kind); assertEquals("shop-fix sent a plan", n.title); assertEquals("Plan do x", n.body)
        assertEquals("w1", n.agentId); assertEquals(Tab.Messages, n.tab); assertEquals(42L, n.seq)
        assertEquals(Tab.Terminal, parse("""{"v":1,"kind":"input","agent":"w1","tab":"terminal"}""")!!.toNotice()!!.tab)
        assertFalse(p.isTest)
    }

    @Test fun `a test push, an unknown kind and what isn't ours`() {
        assertTrue(parse("""{"v":1,"kind":"test","title":"agent-mesh","body":"works"}""")!!.isTest)
        assertNull(parse("""{"v":1,"kind":"party","agent":"w1"}""")!!.toNotice())
        assertNull(parse("""{"v":1,"kind":"plan"}""")!!.toNotice())
        assertNull(parse("""{"v":2,"kind":"plan","agent":"w1"}"""))
        assertNull(parse("not json"))
    }

    @Test fun `an endpoint never shows in a log`() {
        assertEquals("SavedEndpoint(…)", SavedEndpoint("https://ntfy.sh/upSecret", "k", "a").toString())
    }
}
