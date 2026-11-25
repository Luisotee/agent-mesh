package dev.agentmesh.android

import dev.agentmesh.android.hosts.HostRepository
import dev.agentmesh.android.hosts.SecretBox
import dev.agentmesh.android.net.Route
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/** A box that reverses the text: enough to show the file never holds the token as it was given. */
private object Reversing : SecretBox {
    override fun seal(plain: String) = "sealed:" + plain.reversed()
    override fun open(sealed: String) = sealed.removePrefix("sealed:").reversed()
}

class HostRepositoryTest {
    @get:Rule val tmp = TemporaryFolder()
    private fun repo() = HostRepository(tmp.root.resolve("hosts.json"), Reversing)

    @Test fun `a host is kept across restarts with its routes, and its token comes back`() {
        val r = repo()
        val h = r.add(" Home ", listOf(Route.Direct("https://box.ts.net", "ab".repeat(32))), "tok-123")
        val again = repo()
        assertEquals(listOf(h), again.hosts.value)
        assertEquals("Home", again.get(h.id)!!.name)
        assertEquals(Route.Direct("https://box.ts.net", "ab".repeat(32)), again.get(h.id)!!.routes.single())
        assertEquals("tok-123", again.token(h.id))
    }

    @Test fun `the token is never in the file in the clear`() {
        repo().add("Home", listOf(Route.Direct("https://box")), "tok-123")
        val text = tmp.root.resolve("hosts.json").readText()
        assertFalse("tok-123" in text)
    }

    @Test fun `edit keeps the token unless a new one is given, and remove forgets it`() {
        val r = repo()
        val h = r.add("Home", listOf(Route.Direct("https://a")), "one")
        r.update(h.id, "Home 2", listOf(Route.Direct("https://b")))
        assertEquals("one", r.token(h.id)); assertEquals("Home 2", r.get(h.id)!!.name)
        r.update(h.id, "Home 2", listOf(Route.Direct("https://b")), "two")
        assertEquals("two", r.token(h.id))
        r.remove(h.id)
        assertNull(r.get(h.id)); assertNull(r.token(h.id))
        assertEquals(emptyList<Any>(), repo().hosts.value)
    }

    @Test fun `a daemon id is learnt once`() {
        val r = repo()
        val h = r.add("Home", listOf(Route.Direct("https://a")), "t")
        r.setDaemonId(h.id, "d1")
        assertEquals("d1", repo().get(h.id)!!.daemonId)
    }

    @Test fun `a corrupt file is an empty list, not a crash`() {
        tmp.root.resolve("hosts.json").writeText("{not json")
        assertEquals(emptyList<Any>(), repo().hosts.value)
    }
}
