package dev.agentmesh.android

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import androidx.test.core.app.ApplicationProvider
import dev.agentmesh.android.notify.Notifier
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

/**
 * The background connection (a foreground service, so a permanent notification) is an opt-in: off by default, and an
 * install that had it on only by the old default is off now.
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [36])
class BackgroundServiceTest {
    private val app get() = ApplicationProvider.getApplicationContext<Application>()
    private fun prefsFile() = app.getSharedPreferences("prefs", android.content.Context.MODE_PRIVATE)

    @Test fun `a fresh install keeps no connection in the background`() {
        prefsFile().edit().clear().commit()
        assertFalse(DevicePrefs(app).background.value)
    }

    @Test fun `an install that never touched the switch is off, though the old default was on`() {
        // What an earlier version left: other preferences stored, the background one never written.
        prefsFile().edit().clear().putBoolean("asked_notifications", true).putBoolean("push", true).commit()
        assertFalse(DevicePrefs(app).background.value)
    }

    @Test fun `a choice made explicitly is kept, on or off`() {
        prefsFile().edit().clear().putBoolean("background", true).commit()
        assertTrue(DevicePrefs(app).background.value)
        prefsFile().edit().clear().putBoolean("background", false).commit()
        assertFalse(DevicePrefs(app).background.value)
        // And turning it on in the settings is stored for the next start.
        val prefs = DevicePrefs(app)
        prefs.setBackground(true)
        assertTrue(DevicePrefs(app).background.value)
    }

    @Test fun `the notification it needs sits on a minimum-importance channel, and the old low one is gone`() {
        val sys = app.getSystemService(NotificationManager::class.java)
        // A channel an earlier version made, low importance (status-bar icon).
        sys.createNotificationChannel(NotificationChannel("connection", "Connection", NotificationManager.IMPORTANCE_LOW))
        Notifier(app)
        assertNull(sys.getNotificationChannel("connection"))
        val ch = sys.getNotificationChannel(Notifier.CH_CONNECTION)
        assertEquals(NotificationManager.IMPORTANCE_MIN, ch.importance)
        assertFalse(ch.canShowBadge())
        assertEquals("connection_min", Notifier.CH_CONNECTION)
    }
}
