package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.term.CloseCode
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import dev.agentmesh.gen.agentmesh.term.v1.AttachMode
import dev.agentmesh.gen.agentmesh.term.v1.TermFrame
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class TermClientTest {
    private class Rig(scope: TestScope) {
        val api = RecordingApi()
        val screen = TermScreen()
        val client = TermClient(scope.backgroundScope, "a1", { api }, screen, baseMs = 500, maxMs = 10_000)
        fun socket() = FakeSocket().also { api.sockets += it }
    }

    private fun first(s: FakeSocket) {
        s.host(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, epoch = 1, offset = 5, cols = 40, rows = 10))
        s.host(Frames.screen("hello", epoch = 1, offset = 5))
        s.host(Frames.replayDone(5))
    }

    @Test fun `it attaches with a repaint and no size, and shows the host's screen`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent()
        val attach = s.sent.single().attach
        assertEquals(AttachMode.ATTACH_MODE_REPAINT, attach.mode)
        // It asks for no size: a phone must not shrink the terminal the desktop is using.
        assertEquals(0, attach.cols); assertEquals(0, attach.rows)
        first(s); runCurrent()
        assertEquals(TermState.Live, r.client.state.value)
        assertEquals("hello", r.screen.visibleText()); assertEquals(40, r.screen.cols); assertEquals(10, r.screen.rows)
        assertEquals(listOf("a1"), r.api.tickets)
    }

    @Test fun `it never sends anything but the attach`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent()
        first(s); s.host(Frames.output(" world", 5)); runCurrent()
        assertEquals(listOf(TermFrame.FrameCase.ATTACH), s.sent.map { it.frameCase })
        assertEquals("hello world", r.screen.visibleText())
    }

    @Test fun `output that overlaps what is on screen is cut, one already shown is skipped, a gap repaints`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        s.host(Frames.output("lo wor", 3)) // 3..9: only "lo wor"[2..] is new
        runCurrent()
        assertEquals("hellowor", r.screen.visibleText().replace(" ", ""))
        s.host(Frames.output("zzz", 0)) // wholly old
        runCurrent()
        assertEquals("hellowor", r.screen.visibleText().replace(" ", ""))
        val second = r.socket()
        s.host(Frames.output("far", 500)) // a gap
        runCurrent()
        assertEquals("repainted from scratch", AttachMode.ATTACH_MODE_REPAINT, second.sent.single().attach.mode)
    }

    @Test fun `a dropped stream reattaches from where it was, with a fresh ticket and a growing wait`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s); s.host(Frames.output("!", 5)); runCurrent()
        val s2 = r.socket(); val s3 = r.socket()
        s.fail(java.io.IOException("reset")); runCurrent()
        assertTrue(r.client.state.value is TermState.Reconnecting)
        assertEquals("not before the wait", 1, r.api.tickets.size)
        advanceTimeBy(501); runCurrent()
        val attach = s2.sent.single().attach
        assertEquals(AttachMode.ATTACH_MODE_EXACT, attach.mode); assertEquals(1, attach.epoch); assertEquals(6, attach.offset)
        assertEquals(2, r.api.tickets.size)
        // It dropped again before ReplayDone: the wait doubles.
        s2.fail(java.io.IOException("reset")); runCurrent()
        advanceTimeBy(900); runCurrent()
        assertEquals(2, r.api.tickets.size)
        advanceTimeBy(200); runCurrent()
        assertEquals(3, r.api.tickets.size)
        assertEquals(AttachMode.ATTACH_MODE_EXACT, s3.sent.single().attach.mode)
    }

    @Test fun `a slow-client close reattaches by offset at once`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        val s2 = r.socket()
        s.close(CloseCode.SLOW, "slow"); runCurrent()
        assertEquals(AttachMode.ATTACH_MODE_EXACT, s2.sent.single().attach.mode)
        assertEquals(5, s2.sent.single().attach.offset)
    }

    @Test fun `output of another epoch means somebody resized, so it repaints and takes the new size`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        val s2 = r.socket()
        s.host(Frames.output("x", 5, epoch = 2)); runCurrent()
        assertEquals(AttachMode.ATTACH_MODE_REPAINT, s2.sent.single().attach.mode)
        s2.host(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, epoch = 2, offset = 9, cols = 30, rows = 8))
        s2.host(Frames.screen("fresh", epoch = 2, offset = 9)); s2.host(Frames.replayDone(9)); runCurrent()
        assertEquals(30, r.screen.cols); assertEquals("fresh", r.screen.visibleText())
        assertEquals(TermState.Live, r.client.state.value)
    }

    @Test fun `when the host can't continue exactly it repaints, and the old screen is dropped`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        val s2 = r.socket()
        s.fail(java.io.IOException("reset")); runCurrent(); advanceTimeBy(501); runCurrent()
        s2.host(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, epoch = 3, offset = 50, reanchored = true))
        s2.host(Frames.screen("new screen", epoch = 3, offset = 50)); s2.host(Frames.replayDone(50)); runCurrent()
        assertEquals("new screen", r.screen.visibleText())
    }

    @Test fun `a host that keeps asking for an immediate reattach gets the growing wait, not a tight loop`() = runTest {
        val r = Rig(this)
        val socks = (1..6).map { r.socket() }
        r.client.start(); runCurrent()
        socks[0].close(CloseCode.SLOW, "slow"); runCurrent()
        assertEquals("the first one is at once", 2, r.api.tickets.size)
        socks[1].close(CloseCode.SLOW, "slow"); runCurrent()
        assertEquals("then it waits", 2, r.api.tickets.size)
        advanceTimeBy(501); runCurrent()
        assertEquals(3, r.api.tickets.size)
        socks[2].close(CloseCode.SLOW, "slow"); runCurrent(); advanceTimeBy(600); runCurrent()
        assertEquals("the wait doubled", 3, r.api.tickets.size)
        advanceTimeBy(500); runCurrent()
        assertEquals(4, r.api.tickets.size)
    }

    @Test fun `a refused upgrade is retried with a fresh ticket, but a refused token is final`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent()
        val s2 = r.socket()
        s.fail(ApiException(Code.UNAUTHENTICATED, "the host refused the terminal ticket")); runCurrent()
        assertTrue(r.client.state.value is TermState.Reconnecting)
        advanceTimeBy(501); runCurrent()
        assertEquals(2, r.api.tickets.size); assertEquals(1, s2.sent.size)

        val r2 = Rig(this)
        r2.api.terminalError = ApiException(Code.UNAUTHENTICATED, "bad token")
        r2.client.start(); runCurrent()
        assertEquals(TermState.Failed("bad token"), r2.client.state.value)
    }

    @Test fun `a repaint asked for is kept until the host answers, across a drop`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        val s2 = r.socket(); val s3 = r.socket()
        s.host(Frames.output("x", 5, epoch = 2)); runCurrent() // somebody resized: repaint asked
        assertEquals(AttachMode.ATTACH_MODE_REPAINT, s2.sent.single().attach.mode)
        s2.fail(java.io.IOException("dropped before Attached")); runCurrent(); advanceTimeBy(501); runCurrent()
        assertEquals("still a repaint, not an exact reattach with the old epoch", AttachMode.ATTACH_MODE_REPAINT, s3.sent.single().attach.mode)
    }

    @Test fun `an exit close without the exit frame says only that it ended`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        s.close(CloseCode.EXITED, "exited"); runCurrent()
        assertEquals(TermState.Ended, r.client.state.value)
    }

    @Test fun `an exit ends it with the last screen and no more attaches`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        s.host(Frames.exited(code = 2)); runCurrent()
        assertEquals(TermState.Exited(2, 0), r.client.state.value)
        assertEquals("hello", r.screen.visibleText())
        advanceTimeBy(60_000); runCurrent()
        assertEquals(1, r.api.tickets.size)
    }

    @Test fun `a host that refuses, or has no such terminal, stops it with the reason`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent()
        s.close(CloseCode.BAD_REQUEST, "the first frame must be Attach"); runCurrent()
        assertEquals(TermState.Failed("the first frame must be Attach"), r.client.state.value)

        val r2 = Rig(this)
        r2.api.terminalError = ApiException(Code.NOT_FOUND, "no terminal for agent a1")
        r2.client.start(); runCurrent()
        assertEquals(TermState.Failed("no terminal for agent a1"), r2.client.state.value)
        advanceTimeBy(60_000); runCurrent()
        assertEquals(0, r2.api.tickets.size)
    }

    @Test fun `letting go closes the stream`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        r.client.stop(); runCurrent()
        assertTrue(s.closed)
    }

    @Test fun `a drop before any replay resets the immediate-reattach streak`() = runTest {
        val r = Rig(this)
        val socks = (1..6).map { r.socket() }
        r.client.start(); runCurrent()
        socks[0].close(CloseCode.SLOW, "slow"); runCurrent()   // at once: streak 1
        socks[1].fail(java.io.IOException("reset")); runCurrent() // a drop before any replay: its own backoff, and the streak is over
        advanceTimeBy(501); runCurrent()
        assertEquals(3, r.api.tickets.size)
        socks[2].close(CloseCode.SLOW, "slow"); runCurrent()
        assertEquals("a slow close right after is the first of a new streak: reattached at once", 4, r.api.tickets.size)
    }

    @Test fun `a refusal of the upgrade other than a stale ticket is final, with the reason`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent()
        s.fail(ApiException(Code.NOT_FOUND, "the host has no such terminal")); runCurrent()
        assertEquals(TermState.Failed("the host has no such terminal"), r.client.state.value)
        advanceTimeBy(60_000); runCurrent()
        assertEquals(1, r.api.tickets.size)

        val r2 = Rig(this); val s2 = r2.socket()
        r2.client.start(); runCurrent()
        s2.fail(javax.net.ssl.SSLPeerUnverifiedException("pin")); runCurrent()
        assertTrue(r2.client.state.value is TermState.Failed)
    }

    @Test fun `retry after a failure attaches again, and does nothing otherwise`() = runTest {
        val r = Rig(this)
        r.api.terminalError = ApiException(Code.FAILED_PRECONDITION, "agent a1 has no terminal")
        r.client.start(); runCurrent()
        assertEquals(TermState.Failed("agent a1 has no terminal"), r.client.state.value)
        r.api.terminalError = null
        val s = r.socket()
        r.client.retry(); runCurrent()
        first(s); runCurrent()
        assertEquals(TermState.Live, r.client.state.value)
        r.client.retry(); runCurrent()
        assertEquals("a live one is left alone", 1, r.api.tickets.size)
    }

    @Test fun `it says it is waiting while the host isn't ready, then attaches`() = runTest {
        var ready = false
        val api = RecordingApi()
        val gate = kotlinx.coroutines.CompletableDeferred<Unit>()
        val client = TermClient(backgroundScope, "a1", { gate.await(); api }, TermScreen(), hostReady = { ready })
        client.start(); runCurrent()
        assertEquals(TermState.WaitingForHost, client.state.value)
        ready = true; gate.complete(Unit); runCurrent()
        assertEquals(TermState.Attaching, client.state.value)
        assertEquals(1, api.tickets.size)
    }
}
