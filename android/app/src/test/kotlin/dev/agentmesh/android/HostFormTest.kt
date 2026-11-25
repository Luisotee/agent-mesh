package dev.agentmesh.android

import dev.agentmesh.android.net.Route
import dev.agentmesh.android.ui.editedRoutes
import dev.agentmesh.android.ui.validateHost
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Test

class HostFormTest {
    private val fp = "ab".repeat(32)
    private val relay = Route.Relay("relay.example.com:8443", "h1", "amr_c", fp)
    private val tailscale = Route.Direct("https://100.64.0.1:9443", fp)

    @Test fun `a host with a relay route can be saved with no address, and one without still needs it`() {
        assertNull(validateHost("Home", "", "", "", tokenRequired = false, addressOptional = true))
        assertNull(validateHost("Home", "https://", "", "", tokenRequired = false, addressOptional = true))
        assertNotNull(validateHost("Home", "", "", "", tokenRequired = false, addressOptional = false))
        assertNotNull(validateHost("", "", "", "", tokenRequired = false, addressOptional = true)) // the name is still needed
        // An address that is typed is still checked.
        assertNotNull(validateHost("Home", "ftp://x", "", "", tokenRequired = false, addressOptional = true))
    }

    @Test fun `an empty address keeps a relay-only host's routes, and a typed one is added in first place`() {
        assertEquals(listOf<Route>(relay), editedRoutes(listOf(relay), "", ""))
        assertEquals(listOf<Route>(relay), editedRoutes(listOf(relay), "https://", ""))
        assertEquals(listOf<Route>(Route.Direct("https://new:1", null), relay), editedRoutes(listOf(relay), "https://new:1/", ""))
    }

    @Test fun `an address replaces only the first direct route, and keeps the relay`() {
        assertEquals(listOf<Route>(Route.Direct("https://new:1", fp), relay), editedRoutes(listOf(tailscale, relay), "https://new:1", fp))
        assertEquals(listOf<Route>(Route.Direct("https://new:1", null)), editedRoutes(null, "https://new:1", ""))
    }
}
