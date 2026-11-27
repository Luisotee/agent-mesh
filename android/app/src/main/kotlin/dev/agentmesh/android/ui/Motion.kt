package dev.agentmesh.android.ui

import android.provider.Settings
import androidx.compose.animation.core.FiniteAnimationSpec
import androidx.compose.animation.core.spring
import androidx.compose.foundation.lazy.LazyItemScope
import androidx.compose.runtime.Composable
import androidx.compose.runtime.compositionLocalOf
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
    return remember { Settings.Global.getFloat(ctx.contentResolver, Settings.Global.ANIMATOR_DURATION_SCALE, 1f) == 0f }
}

/** A list item that glides to its new place when the list changes (an insert, a removal, a reorder); still under reduced motion. */
@Composable
fun LazyItemScope.motionItem(): Modifier =
    if (LocalReducedMotion.current) Modifier else Modifier.animateItem(
        fadeInSpec = spring(stiffness = 400f), placementSpec = spring(stiffness = 500f, visibilityThreshold = IntOffset(1, 1)),
        fadeOutSpec = spring(stiffness = 400f),
    )

/** A spring for what answers a touch, or no animation at all under reduced motion. */
fun <T> motionSpring(reduced: Boolean, stiffness: Float = 500f): FiniteAnimationSpec<T> =
    if (reduced) androidx.compose.animation.core.snap() else spring(stiffness = stiffness)
