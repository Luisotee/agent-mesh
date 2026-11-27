package dev.agentmesh.android.ui

import android.provider.Settings
import androidx.compose.animation.core.FiniteAnimationSpec
import androidx.compose.animation.core.spring
import androidx.compose.foundation.lazy.LazyItemScope
import androidx.compose.runtime.Composable
import androidx.compose.animation.animateContentSize
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import android.database.ContentObserver
import android.os.Handler
import android.os.Looper
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.IntOffset

/**
 * Whether the system asks for no animation (the "remove animations" accessibility setting, which sets the animator
 * duration scale to 0). Everything that moves reads it: the spinner, the shimmer, list changes, folds. What is a state
 * (a colour, a shape, a check mark) never depends on it.
 */
val LocalReducedMotion = compositionLocalOf { false }

@Composable
fun rememberReducedMotion(): Boolean {
    val ctx = LocalContext.current
    fun read() = Settings.Global.getFloat(ctx.contentResolver, Settings.Global.ANIMATOR_DURATION_SCALE, 1f) == 0f
    var reduced by remember { mutableStateOf(read()) }
    // Turned on while the app is open (the user flips the setting and comes back): it takes effect at once.
    DisposableEffect(ctx) {
        val observer = object : ContentObserver(Handler(Looper.getMainLooper())) {
            override fun onChange(selfChange: Boolean) { reduced = read() }
        }
        ctx.contentResolver.registerContentObserver(Settings.Global.getUriFor(Settings.Global.ANIMATOR_DURATION_SCALE), false, observer)
        onDispose { ctx.contentResolver.unregisterContentObserver(observer) }
    }
    return reduced
}

/**
 * A list item that fades in and out, and glides to its new place when the list changes (an insert, a removal, a
 * reorder); still under reduced motion. A list that keeps the reader's place by scrolling (the chat, where earlier
 * entries come in above) passes `glide = false`: items only fade, so the placement never fights the scroll.
 */
@Composable
fun LazyItemScope.motionItem(glide: Boolean = true): Modifier =
    if (LocalReducedMotion.current) Modifier else Modifier.animateItem(
        fadeInSpec = spring(stiffness = 400f),
        placementSpec = if (glide) spring(stiffness = 500f, visibilityThreshold = IntOffset(1, 1)) else null,
        fadeOutSpec = spring(stiffness = 400f),
    )

/**
 * A size change that animates (a card opening), or none under reduced motion. With [animate] false the node is there but
 * snaps: a size that is the first layout (a row scrolled into view) must not animate, and one the user asked for must, from
 * the first time (a node attached with the change takes the new size as its start).
 */
@Composable
fun Modifier.motionSize(animate: Boolean = true): Modifier =
    if (LocalReducedMotion.current) this else this.animateContentSize(if (animate) spring() else androidx.compose.animation.core.snap())

/** A spring for what answers a touch, or no animation at all under reduced motion. */
fun <T> motionSpring(reduced: Boolean, stiffness: Float = 500f): FiniteAnimationSpec<T> =
    if (reduced) androidx.compose.animation.core.snap() else spring(stiffness = stiffness)
