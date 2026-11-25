package dev.agentmesh.android

import dev.agentmesh.android.net.UI_ACTIONS
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/** The app can do nothing an agent can't: every call it makes is an action of the registry that a device may call. */
class RegistryPinTest {
    private val repo = generateSequence(File(System.getProperty("user.dir")).absoluteFile) { it.parentFile }.first { File(it, "proto/agentmesh/v1/mesh.proto").exists() }
    private val proto = File(repo, "proto/agentmesh/v1/mesh.proto").readText()
    private val main = File("src/main/kotlin")

    private fun String.lowerFirst() = replaceFirstChar { it.lowercase() }

    /** The `ACTOR_CLASS_*` entries of each rpc's `action` option, by method name (lower camel). */
    private val actors: Map<String, Set<String>> = Regex("""rpc (\w+)\([^)]*\) returns [^{]*\{\s*option \(action\) = \{\s*actors: \[([^\]]*)\]""")
        .findAll(proto).associate { m -> m.groupValues[1].lowerFirst() to Regex("ACTOR_CLASS_\\w+").findAll(m.groupValues[2]).map { it.value }.toSet() }

    @Test fun `every UI action is a registry method a device may call`() {
        assertTrue(actors.size > 10)
        for (name in UI_ACTIONS) {
            assertTrue("$name is not an action of MeshService", name in actors)
            assertTrue("$name may not be called by a device", "ACTOR_CLASS_DEVICE" in actors[name]!!)
        }
    }

    private fun sources() = main.walkTopDown().filter { it.extension == "kt" }.toList()

    @Test fun `the code calls no host method outside that list`() {
        val used = mutableSetOf<String>()
        for (f in sources()) for (m in Regex("""\bservice\.(\w+)\(""").findAll(f.readText())) used += m.groupValues[1]
        assertTrue(used.isNotEmpty())
        assertEquals(emptySet<String>(), used - UI_ACTIONS.toSet())
    }

    @Test fun `only the net package reaches the network or the generated client`() {
        val offenders = sources().filter { "/net/" !in it.path.replace('\\', '/') }.filter { f ->
            Regex("""okhttp3\.|java\.net\.(Socket|URL|HttpURLConnection)|MeshServiceClient|ConnectOkHttpClient|WebSocket""").containsMatchIn(f.readText())
        }
        assertEquals(emptyList<File>(), offenders)
    }

    /** Pairing is the one other service a device calls, and it needs no token: only `RedeemPairing`, which must exist in its proto. */
    @Test fun `the one PairingService call is RedeemPairing`() {
        val pairingProto = File(repo, "proto/agentmesh/v1/pairing.proto").readText()
        assertTrue("rpc RedeemPairing(" in pairingProto)
        val used = mutableSetOf<String>()
        for (f in sources()) for (m in Regex("""\bpairingService\.(\w+)\(""").findAll(f.readText())) used += m.groupValues[1]
        assertEquals(setOf("redeemPairing"), used)
        // The host-side pairing actions stay the host's own: nothing here calls them.
        for (f in sources()) assertFalse(f.path, Regex("""createPairing|listDevices|revokeDevice""", RegexOption.IGNORE_CASE).containsMatchIn(f.readText().replace(Regex("""//.*|/\*[\s\S]*?\*/"""), "")))
    }
}
