package dev.agentmesh.android

import com.connectrpc.Code
import dev.agentmesh.android.net.ApiException
import dev.agentmesh.android.net.echoCloseCode
import dev.agentmesh.android.net.refusal
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class TerminalSocketTest {
    private fun response(code: Int) = Response.Builder().request(Request.Builder().url("http://h/").build()).protocol(Protocol.HTTP_1_1).code(code).message("m").build()

    @Test fun `a close is answered with the peer's code only when a close frame may carry it`() {
        for (c in listOf(1000, 1001, 1011, 4000, 4001, 4002, 4003, 4999)) assertEquals(c, echoCloseCode(c))
        // An empty close frame arrives as 1005; 1006, 1015 and the rest are never sent: OkHttp throws on them.
        for (c in listOf(0, 999, 1004, 1005, 1006, 1012, 1015, 2999, 5000)) assertEquals("code $c", 1000, echoCloseCode(c))
    }

    @Test fun `a refused upgrade is a stale ticket, a forbidden or missing terminal, or just a failure`() {
        val boom = java.io.IOException("boom")
        assertEquals(Code.UNAUTHENTICATED, (refusal(response(401), boom) as ApiException).code)
        assertEquals(Code.PERMISSION_DENIED, (refusal(response(403), boom) as ApiException).code)
        assertEquals(Code.NOT_FOUND, (refusal(response(404), boom) as ApiException).code)
        assertSame(boom, refusal(response(502), boom))
        assertSame(boom, refusal(null, boom))
        assertTrue(refusal(response(403), boom).cause === boom)
    }
}
