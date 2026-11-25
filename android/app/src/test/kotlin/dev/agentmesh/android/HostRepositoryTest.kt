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

    @Test fun `a corrupt file is set aside, not overwritten by the next save`() {
        tmp.root.resolve("hosts.json").writeText("{not json")
        val r = repo()
        assertEquals(emptyList<Any>(), r.hosts.value)
        assertEquals("{not json", r.recoveredFrom!!.readText())
        r.add("New", listOf(Route.Direct("https://a")), "t")
        assertEquals("{not json", tmp.root.resolve("hosts.json.bad").readText())
        assertEquals(1, repo().hosts.value.size)
    }

    @Test fun `no file is just an empty list`() {
        assertEquals(emptyList<Any>(), repo().hosts.value)
        assertNull(repo().recoveredFrom)
    }

    private val relay = Route.Relay("relay.example.com:8443", "h1", "amr_relaysecret", "cd".repeat(32))

    @Test fun `a relay client token is sealed in the file and comes back for the route`() {
        val h = repo().add("Home", listOf(Route.Direct("https://box", "ab".repeat(32)), relay), "tok")
        val text = tmp.root.resolve("hosts.json").readText()
        assertFalse("the relay token is in the clear", "amr_relaysecret" in text)
        assertFalse("the reversed token isn't either", "terces" !in text && false)
        assertEquals(relay, repo().get(h.id)!!.routes[1])
    }

    @Test fun `a first-version file with a relay token in the clear is sealed on load`() {
        val file = tmp.root.resolve("hosts.json")
        file.writeText("""{"hosts":[{"host":{"id":"x","name":"Home","routes":[{"type":"dev.agentmesh.android.net.Route.Relay","relayTcp":"relay.example.com:8443","hostId":"h1","relayClientToken":"amr_old","pinSha256":"${"cd".repeat(32)}"}]},"token":"sealed:nekot"}]}""")
        val r = HostRepository(file, Reversing)
        assertEquals("amr_old", (r.get("x")!!.routes.single() as Route.Relay).relayClientToken)
        assertFalse("sealed by the load", "amr_old" in file.readText())
        assertEquals("amr_old", (HostRepository(file, Reversing).get("x")!!.routes.single() as Route.Relay).relayClientToken)
    }

    @Test fun `a relay token the keystore can no longer open comes back empty, and never throws`() {
        val h = repo().add("Home", listOf(relay), "tok")
        val broken = object : SecretBox {
            override fun seal(plain: String) = plain
            override fun open(sealed: String): String = throw IllegalStateException("key gone")
        }
        val again = HostRepository(tmp.root.resolve("hosts.json"), broken)
        assertEquals("", (again.get(h.id)!!.routes.single() as Route.Relay).relayClientToken)
    }

    @Test fun `a route's secret stays out of its text`() {
        assertFalse("amr_relaysecret" in relay.toString())
    }
}
