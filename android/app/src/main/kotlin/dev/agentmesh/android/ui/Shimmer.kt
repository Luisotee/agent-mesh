package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.ContentDrawScope
import androidx.compose.ui.node.DrawModifierNode
import androidx.compose.ui.node.ModifierNodeElement
import androidx.compose.ui.platform.InspectorInfo
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.animation.core.withInfiniteAnimationFrameMillis
import androidx.compose.ui.node.invalidateDraw
import kotlinx.coroutines.Job
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

private const val SWEEP_MS = 1600L

/**
 * A moving light across what is loading. It is draw-only: a node that moves a gradient and redraws, never recomposes
 * (a shimmer written as state recomposed its row every frame in happy, and cost it a tenth of a frame budget), and it
 * runs only while its node is attached, so a row scrolled off the screen stops it. Still, as a flat fill, when [animate] is off.
 */
@Composable
fun Modifier.shimmer(animate: Boolean = !LocalReducedMotion.current): Modifier {
    val s = MaterialTheme.colorScheme
    return this.then(ShimmerElement(s.surfaceVariant, s.primaryContainer.copy(alpha = 0.55f), animate))
}

private data class ShimmerElement(val base: Color, val light: Color, val animate: Boolean) : ModifierNodeElement<ShimmerNode>() {
    override fun create() = ShimmerNode(base, light, animate)
    override fun update(node: ShimmerNode) = node.update(base, light, animate)
    override fun InspectorInfo.inspectableProperties() { name = "shimmer" }
}

private class ShimmerNode(var base: Color, var light: Color, private var animate: Boolean) : Modifier.Node(), DrawModifierNode {
    private var phase = 0f
    private var job: Job? = null

    /** New colours (a theme change) redraw at once; a change of [animate] starts or stops the sweep. */
    fun update(base: Color, light: Color, on: Boolean) {
        val moved = base != this.base || light != this.light
        this.base = base
        this.light = light
        if (on != animate) {
            animate = on
            if (isAttached) restart()
        } else if (moved && isAttached) invalidateDraw()
    }

    override fun onAttach() = restart()
    override fun onDetach() { job?.cancel(); job = null }

    private fun restart() {
        job?.cancel()
        job = null
        if (!animate) { phase = 0f; invalidateDraw(); return }
        job = coroutineScope.launch {
            // The infinite variant: a test's clock may cancel it (an endless frame loop would keep Compose from ever being idle).
            val start = withInfiniteAnimationFrameMillis { it }
            while (isActive) {
                val t = withInfiniteAnimationFrameMillis { it }
                phase = ((t - start) % SWEEP_MS).toFloat() / SWEEP_MS
                invalidateDraw()
            }
        }
    }

    override fun ContentDrawScope.draw() {
        if (!animate) { drawRect(base); return }
        val w = size.width
        // A band a third of the width wide, sliding from before the left edge to past the right.
        val x = -w / 3 + (w + w / 3) * phase
        drawRect(Brush.linearGradient(listOf(base, light, base), start = Offset(x, 0f), end = Offset(x + w / 3, 0f)))
    }
}

/** A placeholder bar in the shape of what is loading. It replaces a "Loading…" text; its region carries the label. */
@Composable
fun Skeleton(modifier: Modifier = Modifier, height: Dp = 12.dp, radius: Dp = 6.dp) {
    Box(modifier.height(height).clip(RoundedCornerShape(radius)).shimmer())
}

/** Lines of skeleton for a list or a block of text, the last one shorter, [label] read out as what is loading. */
@Composable
fun SkeletonLines(label: String, modifier: Modifier = Modifier, lines: Int = 3) {
    Column(modifier.clearAndSetSemantics { contentDescription = label }, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        repeat(lines) { i -> Skeleton(Modifier.fillMaxWidth(if (i == lines - 1) 0.66f else if (i % 2 == 1) 0.85f else 1f)) }
    }
}

/** Rows shaped like the list's own (a round icon, two lines) while a list loads. */
@Composable
fun SkeletonRows(label: String, modifier: Modifier = Modifier, rows: Int = 4) {
    Column(modifier.clearAndSetSemantics { contentDescription = label }, verticalArrangement = Arrangement.spacedBy(20.dp)) {
        repeat(rows) { i ->
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Skeleton(Modifier.size(20.dp), height = 20.dp, radius = 10.dp)
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Skeleton(Modifier.fillMaxWidth(if (i % 2 == 0) 0.6f else 0.45f), height = 14.dp)
                    Skeleton(Modifier.fillMaxWidth(if (i % 2 == 0) 0.85f else 0.7f), height = 11.dp)
                }
            }
        }
    }
}
