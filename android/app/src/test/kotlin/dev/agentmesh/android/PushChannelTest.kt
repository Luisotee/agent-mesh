package dev.agentmesh.android

import android.app.Application
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.notify.UnifiedPushChannel
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import org.unifiedpush.android.connector.UnifiedPush
import org.unifiedpush.android.connector.data.PublicKeySet
import org.unifiedpush.android.connector.keys.KeyManager

/** The real UnifiedPush channel, on the connector library itself. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36], application = TestApp::class)
class PushChannelTest {
    private val context = ApplicationProvider.getApplicationContext<Application>()

    @Test fun `unregistering the last host keeps the distributor for the next one`() {
        // The library forgets the distributor with its last instance; a host added later would then never register.
        // ntfy installed: a receiver of the distributor's REGISTER broadcast.
        val pm = org.robolectric.Shadows.shadowOf(context.packageManager)
        val receiver = android.content.ComponentName("io.heckel.ntfy", "io.heckel.ntfy.up.BroadcastReceiver")
        pm.installPackage(android.content.pm.PackageInfo().apply { packageName = "io.heckel.ntfy" })
        pm.addOrUpdateReceiver(android.content.pm.ActivityInfo().apply {
            packageName = receiver.packageName; name = receiver.className; exported = true
            applicationInfo = android.content.pm.ApplicationInfo().apply { packageName = receiver.packageName }
        })
        pm.addIntentFilterForReceiver(receiver, android.content.IntentFilter("org.unifiedpush.android.distributor.REGISTER"))
        assertEquals(listOf("io.heckel.ntfy"), UnifiedPush.getDistributors(context))
        val channel = UnifiedPushChannel(context, NoKeys())
        UnifiedPush.saveDistributor(context, "io.heckel.ntfy")
        channel.register("h1", "Home")
        channel.unregister("h1")
        assertEquals("io.heckel.ntfy", UnifiedPush.getSavedDistributor(context))
    }
}

/** Keys in memory: Robolectric has no Android Keystore. */
private class NoKeys : KeyManager {
    private val made = mutableSetOf<String>()
    override fun decrypt(instance: String, sealed: ByteArray): ByteArray = error("unused")
    override fun generate(instance: String) { made += instance }
    override fun getPublicKeySet(instance: String) = if (instance in made) PublicKeySet("pub", "auth") else null
    override fun exists(instance: String) = instance in made
    override fun delete(instance: String) { made -= instance }
}
