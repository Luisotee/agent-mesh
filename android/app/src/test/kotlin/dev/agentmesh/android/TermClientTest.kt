package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.RelayRefused
import dev.agentmesh.android.term.CloseCode
import dev.agentmesh.android.term.TermClient
import dev.agentmesh.android.term.TermScreen
import dev.agentmesh.android.term.TermState
import dev.agentmesh.android.term.pasteBytes
import dev.agentmesh.android.term.glyphRange
import dev.agentmesh.android.term.selectedText
import dev.agentmesh.android.term.wordRange
import dev.agentmesh.android.term.CellPos
import dev.agentmesh.android.term.Selection
import dev.agentmesh.android.term.absoluteRow
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

    @Test fun `a Resized frame moves the screen to the new size and epoch in place, with no reattach`() = runTest {
        val r = Rig(this); val s = r.socket()
        r.client.start(); runCurrent(); first(s)
        s.host(Frames.resized(epoch = 2, offset = 5, cols = 30, rows = 8))
        s.host(Frames.output("x", 5, epoch = 2)); runCurrent()
        assertEquals(30, r.screen.cols); assertEquals(8, r.screen.rows)
        assertEquals(1, s.sent.size) // only the first Attach: no repaint was asked for
        assertEquals(TermState.Live, r.client.state.value)
        assertEquals("hellox", r.screen.visibleText())
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

    @Test fun `a relay that refuses the client token, or a certificate that isn't the pinned one, is final and says so, and an offline relay is retried`() = runTest {
        val refused = RelayRefused("auth", "the relay refused this device's client token")
        val pinned = javax.net.ssl.SSLHandshakeException("handshake failed").also { it.initCause(java.security.cert.CertificateException("the host's certificate ab is not the pinned one")) }
        fun failed(r: Rig) = (r.client.state.value as TermState.Failed).message
        // From the ticket request (ConnectRPC wraps what the socket threw)...
        val a = Rig(this); a.api.terminalError = ApiException(Code.UNAVAILABLE, "x", refused)
        a.client.start(); runCurrent()
        assertTrue(failed(a), "relay refused" in failed(a) && "Pair with the host again" in failed(a))
        val b = Rig(this); b.api.terminalError = ApiException(Code.UNAVAILABLE, "x", pinned)
        b.client.start(); runCurrent()
        assertTrue(failed(b), "is not the pinned one" in failed(b))
        // ...and from the WebSocket's own failure.
        val c = Rig(this); val sc = c.socket(); c.client.start(); runCurrent()
        sc.fail(java.io.IOException("x", refused)); runCurrent()
        assertTrue(failed(c), "relay refused" in failed(c))
        val d = Rig(this); val sd = d.socket(); d.client.start(); runCurrent()
        sd.fail(pinned); runCurrent()
        assertTrue(failed(d), "is not the pinned one" in failed(d))
        advanceTimeBy(60_000); runCurrent()
        assertEquals("no retry after a final failure", listOf(0, 1, 1, 1), listOf(a, b, c, d).map { it.api.tickets.size }.let { listOf(it[0], it[1] + 1, it[2], it[3]) })
        // A host that is offline at the relay may come back: retried, in the relay's words.
        val e = Rig(this); e.api.terminalError = ApiException(Code.UNAVAILABLE, "x", RelayRefused("offline", "the host is offline at the relay"))
        e.client.start(); runCurrent()
        val st = e.client.state.value as TermState.Reconnecting
        assertEquals("the host is offline at the relay", st.why)
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

    // ── Typing ──

    private class TypingRig(scope: TestScope, input: Boolean = true, answer: Boolean = false) {
        val api = RecordingApi()
        val screen = TermScreen()
        val client = TermClient(scope.backgroundScope, "a1", { api }, screen, baseMs = 500, maxMs = 10_000, input = input, answerQueries = answer)
        fun socket() = FakeSocket().also { api.sockets += it }
    }

    private fun granted(s: FakeSocket, input: Boolean = true) {
        s.host(TermFrame.newBuilder().setAttached(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, epoch = 1, offset = 5, cols = 40, rows = 10).attached.toBuilder().setInput(input)).build())
        s.host(Frames.screen("hello", epoch = 1, offset = 5))
    }

    @Test fun `with the input right it asks for it, sends Ready after the replay, and only then types`() = runTest {
        val r = TypingRig(this); val s = r.socket()
        r.client.start(); runCurrent()
        assertEquals(listOf(true), r.api.ticketInputs)
        granted(s); runCurrent()
        // Until the replay is done input is dropped by the host, so none is sent, and nothing says it is typing.
        assertTrue(!r.client.type("a".toByteArray())); assertTrue(!r.client.typing.value)
        s.host(Frames.replayDone(5)); runCurrent()
        assertEquals(listOf(TermFrame.FrameCase.ATTACH, TermFrame.FrameCase.READY), s.sent.map { it.frameCase })
        assertTrue(r.client.typing.value)
        assertTrue(r.client.type("ls\r".toByteArray()))
        assertEquals("ls\r", s.sent.last().input.data.toStringUtf8())
        assertTrue(!r.client.type(ByteArray(0)))
    }

    @Test fun `a phone that types never asks for a size, and never sends a resize`() = runTest {
        val r = TypingRig(this); val s = r.socket()
        r.client.start(); runCurrent(); granted(s); s.host(Frames.replayDone(5)); runCurrent()
        r.client.type("x".toByteArray()); runCurrent()
        val attach = s.sent.first().attach
        // Size 0 is "unknown": the host leaves out of its sizing a client that gave none, so the desktop's terminal stays as it is.
        assertEquals(0, attach.cols); assertEquals(0, attach.rows)
        assertTrue(s.sent.none { it.frameCase == TermFrame.FrameCase.RESIZE })
        // Even when the host's own size changes (another epoch), nothing of a size is sent back.
        s.host(Frames.output("y", 5, epoch = 2)); runCurrent()
        assertTrue(s.sent.none { it.frameCase == TermFrame.FrameCase.RESIZE })
    }

    @Test fun `a client that is the only one that types answers the program's queries once the replay is done, a viewer and the replay never do`() = runTest {
        val r = TypingRig(this, answer = true); val s = r.socket()
        r.client.start(); runCurrent()
        // The replay itself holds a query (the program asked before this client came): its answer is dropped, as the host's input is before Ready.
        s.host(TermFrame.newBuilder().setAttached(Frames.attached(AttachMode.ATTACH_MODE_REPAINT, epoch = 1, offset = 5, cols = 40, rows = 10).attached.toBuilder().setInput(true)).build())
        s.host(Frames.screen("hello\u001B[c", epoch = 1, offset = 5)); s.host(Frames.replayDone(5)); runCurrent()
        assertEquals(listOf(TermFrame.FrameCase.ATTACH, TermFrame.FrameCase.READY), s.sent.map { it.frameCase })
        // Live: device attributes (ESC [ c) and a cursor position report (ESC [ 6 n). With a client that types attached the host's own emulator
        // does not answer, so this one does, as the terminal it is.
        s.host(Frames.output("\u001B[c\u001B[6n", 5)); runCurrent()
        val answers = s.sent.filter { it.hasInput() }.joinToString("") { it.input.data.toStringUtf8() }
        assertTrue("got ${answers.replace("\u001B", "ESC")}", answers.contains("\u001B[?") && answers.contains("R"))
        // What the user types goes too.
        r.client.type("q".toByteArray())
        assertEquals("q", s.sent.last().input.data.toStringUtf8())
    }

    @Test fun `a typing client of an agent's terminal leaves the program's queries to the host and the desktop`() = runTest {
        val r = TypingRig(this); val s = r.socket()
        r.client.start(); runCurrent(); granted(s); s.host(Frames.replayDone(5)); runCurrent()
        s.host(Frames.output("\u001B[c\u001B[6n", 5)); runCurrent()
        // A second answer from here would reach the program as typed input beside the desktop's own.
        assertEquals(listOf(TermFrame.FrameCase.ATTACH, TermFrame.FrameCase.READY), s.sent.map { it.frameCase })
    }

    @Test fun `a viewer sends nothing but the attach, whatever the program asks`() = runTest {
        val r = TypingRig(this, input = false); val s = r.socket()
        r.client.start(); runCurrent(); granted(s, input = false); s.host(Frames.replayDone(5)); runCurrent()
        s.host(Frames.output("\u001B[c\u001B[6n\u001B[5n", 5)); runCurrent()
        assertEquals(listOf(TermFrame.FrameCase.ATTACH), s.sent.map { it.frameCase })
    }

    @Test fun `a resize to the size already sent is not sent again`() = runTest {
        val r = ShellRig(this) { 52 to 30 }; val s = r.socket()
        r.client.start(); runCurrent(); granted(s); s.host(Frames.replayDone(5)); runCurrent()
        assertTrue(!r.client.resize(52, 30))
        assertTrue(r.client.resize(52, 20))
        assertTrue(!r.client.resize(52, 20))
        assertEquals(1, s.sent.count { it.hasResize() })
    }

    @Test fun `a host that did not grant the input right gets no input, and Ready is not sent`() = runTest {
        val r = TypingRig(this); val s = r.socket()
        r.client.start(); runCurrent(); granted(s, input = false); s.host(Frames.replayDone(5)); runCurrent()
        assertTrue(!r.client.typing.value); assertTrue(!r.client.type("a".toByteArray()))
        assertEquals(listOf(TermFrame.FrameCase.ATTACH), s.sent.map { it.frameCase })
    }

    @Test fun `without the keyboard it asks for no input right and sends nothing but the attach`() = runTest {
        val r = TypingRig(this, input = false); val s = r.socket()
        r.client.start(); runCurrent()
        assertEquals(listOf(false), r.api.ticketInputs)
        granted(s); s.host(Frames.replayDone(5)); runCurrent()
        assertTrue(!r.client.typing.value); assertTrue(!r.client.type("a".toByteArray()))
        assertEquals(listOf(TermFrame.FrameCase.ATTACH), s.sent.map { it.frameCase })
    }

    @Test fun `after a drop it is typing again only once the new stream's replay is done`() = runTest {
        val r = TypingRig(this); val s = r.socket()
        r.client.start(); runCurrent(); granted(s); s.host(Frames.replayDone(5)); runCurrent()
        assertTrue(r.client.typing.value)
        val second = r.socket()
        s.close(1006, "network"); runCurrent()
        assertTrue(!r.client.typing.value)
        advanceTimeBy(600); runCurrent()
        granted(second); runCurrent()
        assertTrue(!r.client.typing.value)
        second.host(Frames.replayDone(5)); runCurrent()
        assertTrue(r.client.typing.value)
        assertEquals(listOf(true, true), r.api.ticketInputs)
    }

    // ── The screen: paste and selection ──

    @Test fun `a paste is the program's own bracketed or plain form, newlines as returns and escapes dropped`() {
        val screen = TermScreen(); screen.reset(40, 5)
        assertEquals("a\rb", screen.pasteBytes("a\nb\u001B").toString(Charsets.UTF_8))
        screen.write("\u001B[?2004h".toByteArray())
        assertEquals("\u001B[200~one\rtwo\u001B[201~", screen.pasteBytes("one\r\ntwo").toString(Charsets.UTF_8))
        screen.write("\u001B[?2004l".toByteArray())
        assertEquals("x", screen.pasteBytes("x").toString(Charsets.UTF_8))
    }

    @Test fun `a reply the emulator made to a query is not carried into the next paste`() {
        val screen = TermScreen(); screen.reset(40, 5)
        // A query answered while no paste is being encoded is dropped, and the next paste doesn't carry it.
        screen.write("\u001B[c".toByteArray())
        assertEquals("p", screen.pasteBytes("p").toString(Charsets.UTF_8))
    }

    @Test fun `the text of a selection, across rows, and kept to the same words while output scrolls`() {
        val screen = TermScreen(); screen.reset(20, 3)
        screen.write("alpha beta\r\ngamma delta\r\nepsilon".toByteArray())
        val sel = Selection(CellPos(6, screen.absoluteRow(0)), CellPos(4, screen.absoluteRow(1)))
        assertEquals("beta\ngamma", screen.selectedText(sel))
        // New lines push the screen up: the same words stay selected (rows are counted from the start of the history).
        screen.write("\r\nzeta\r\neta".toByteArray())
        assertEquals("beta\ngamma", screen.selectedText(sel))
        // A selection of just a word.
        assertEquals("delta", screen.selectedText(Selection(CellPos(6, sel.end.row), CellPos(10, sel.end.row))))
    }

    @Test fun `a word is found by cells, so wide characters before it don't move the selection`() {
        val screen = TermScreen(); screen.reset(30, 3)
        // Two wide characters take four columns, then a blank, then two words; an emoji takes two columns and two chars of text.
        screen.write("世界 abc def 😀x".toByteArray())
        assertEquals(5..7, screen.wordRange(0, 6))
        assertEquals(9..11, screen.wordRange(0, 9))
        assertEquals(0..3, screen.wordRange(0, 2))
        assertEquals(4..4, screen.wordRange(0, 4))
        assertEquals(13..15, screen.wordRange(0, 14))
        assertEquals(29..29, screen.wordRange(0, 29))
        // Plain text is as it was.
        val plain = TermScreen(); plain.reset(20, 2)
        plain.write("git commit -m fix".toByteArray())
        assertEquals(4..9, plain.wordRange(0, 6)); assertEquals(0..2, plain.wordRange(0, 0))
    }

    @Test fun `a selection made before the screen was made again is cut to the new screen, and the tab is told by a count`() {
        val screen = TermScreen(); screen.reset(120, 5)
        screen.write("x".repeat(110).toByteArray())
        val sel = Selection(CellPos(100, screen.absoluteRow(0)), CellPos(108, screen.absoluteRow(0)))
        val before = screen.resets
        screen.reset(80, 5)
        assertEquals(before + 1, screen.resets)
        // Columns past the narrower screen are cut, not read: this used to throw out of the emulator.
        assertEquals("", screen.selectedText(sel))
        // A selection that spans onto the new screen's own rows stays within them.
        screen.write("hello".toByteArray())
        assertEquals("hello", screen.selectedText(Selection(CellPos(0, screen.absoluteRow(0)), CellPos(500, screen.absoluteRow(9)))))
    }

    @Test fun `an end that lands on either half of a wide character takes the whole of it, as the highlight does`() {
        val screen = TermScreen(); screen.reset(20, 2)
        // a at 0, 世 at 1-2, 界 at 3-4, b at 5.
        screen.write("a世界b".toByteArray())
        val r = screen.absoluteRow(0)
        assertEquals(1..2, screen.glyphRange(0, 1)); assertEquals(1..2, screen.glyphRange(0, 2)); assertEquals(0..0, screen.glyphRange(0, 0))
        // Forward, ending on the left half of 世: it is selected (this copied just "a").
        assertEquals("a世", screen.selectedText(Selection(CellPos(0, r), CellPos(1, r))))
        // Ending on the right half.
        assertEquals("a世", screen.selectedText(Selection(CellPos(0, r), CellPos(2, r))))
        // Backward, from b to the right half of 世: the whole of 世 (the highlight says so too).
        assertEquals("世界b", screen.selectedText(Selection(CellPos(5, r), CellPos(2, r))))
        // Starting on a left half takes it as well.
        assertEquals("界b", screen.selectedText(Selection(CellPos(3, r), CellPos(5, r))))
    }

    private class ShellRig(scope: TestScope, size: (() -> Pair<Int, Int>?)?) {
        val api = RecordingApi()
        val screen = TermScreen()
        val client = TermClient(scope.backgroundScope, "sh1", { api }, screen, baseMs = 500, maxMs = 10_000, input = true, shell = true, size = size)
        fun socket() = FakeSocket().also { api.sockets += it }
    }

    @Test fun `a shell takes its own ticket, and the phone's size when it sized the shell`() = runTest {
        val r = ShellRig(this) { 52 to 30 }; val s = r.socket()
        r.client.start(); runCurrent()
        assertEquals(listOf("sh1"), r.api.shellTickets); assertTrue(r.api.tickets.isEmpty())
        assertEquals(listOf(true), r.api.shellTicketInputs)
        assertEquals(52, s.sent.first().attach.cols); assertEquals(30, s.sent.first().attach.rows)
        // A resize is only for a client that types, once it does.
        assertTrue(!r.client.resize(50, 20))
        granted(s); s.host(Frames.replayDone(5)); runCurrent()
        assertTrue(r.client.resize(50, 20))
        assertEquals(50, s.sent.last().resize.cols); assertEquals(20, s.sent.last().resize.rows)
    }

    @Test fun `a shell the phone did not size attaches at size 0 and never resizes, like an agent's terminal`() = runTest {
        val r = ShellRig(this, null); val s = r.socket()
        r.client.start(); runCurrent(); granted(s); s.host(Frames.replayDone(5)); runCurrent()
        assertEquals(0, s.sent.first().attach.cols); assertEquals(0, s.sent.first().attach.rows)
        assertTrue(!r.client.resize(50, 20))
        assertTrue(s.sent.none { it.frameCase == TermFrame.FrameCase.RESIZE })
    }
}
