package dev.agentmesh.android.ui

import android.provider.Settings
import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.rotate
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import dev.agentmesh.gen.agentmesh.v1.AgentState

/** The shape that stands for a state, so colour is never the only difference. */
enum class StateShape { Spinner, Triangle, Ring, Check, Cross, Dashed, Dot, Pause, Gauge }

fun shapeOf(state: AgentState): StateShape = when (state) {
    AgentState.AGENT_STATE_WORKING -> StateShape.Spinner
    AgentState.AGENT_STATE_NEEDS_INPUT -> StateShape.Triangle
    AgentState.AGENT_STATE_IDLE -> StateShape.Ring
    AgentState.AGENT_STATE_EXITED -> StateShape.Check
    AgentState.AGENT_STATE_UNKNOWN -> StateShape.Dashed
    AgentState.AGENT_STATE_RETIRED -> StateShape.Pause
    AgentState.AGENT_STATE_LIMITED -> StateShape.Gauge
    AgentState.AGENT_STATE_ERROR -> StateShape.Cross
    else -> StateShape.Dot
}

fun colorOf(state: AgentState, exitCode: Int, detail: String, c: dev.agentmesh.android.theme.StateColors): Color = when (state) {
    AgentState.AGENT_STATE_WORKING -> c.working
    AgentState.AGENT_STATE_NEEDS_INPUT, AgentState.AGENT_STATE_LIMITED -> c.needs
    AgentState.AGENT_STATE_ERROR -> c.failed
    AgentState.AGENT_STATE_IDLE -> c.idle
    AgentState.AGENT_STATE_EXITED -> if (exitCode == 0 && detail != "lost") c.done else c.failed
    AgentState.AGENT_STATE_UNKNOWN, AgentState.AGENT_STATE_RETIRED -> c.unknown
    else -> c.idle
}

/** Whether the system asks for no animation (the "remove animations" accessibility setting). */
@Composable
private fun reducedMotion(): Boolean {
    val ctx = LocalContext.current
    return remember { Settings.Global.getFloat(ctx.contentResolver, Settings.Global.ANIMATOR_DURATION_SCALE, 1f) == 0f }
}

/**
 * An agent's state as an icon: working spins, needs-you is a triangle, idle a ring, finished a check (a cross when
 * it failed), unknown a dashed ring, retired a ring with a pause mark, starting a dot. The shapes differ, and stay static under reduced motion.
 */
@Composable
fun StateIcon(state: AgentState, exitCode: Int, detail: String, description: String, size: Dp = 20.dp, modifier: Modifier = Modifier) {
    val colors = stateColors
    val color = colorOf(state, exitCode, detail, colors)
    var shape = shapeOf(state)
    if (shape == StateShape.Check && color == colors.failed) shape = StateShape.Cross
    val still = reducedMotion()
    val angle = if (shape == StateShape.Spinner && !still) {
        rememberInfiniteTransition(label = "spin").animateFloat(0f, 360f, infiniteRepeatable(tween(1200, easing = LinearEasing), RepeatMode.Restart), label = "angle").value
    } else 0f
    Canvas(modifier.size(size).semantics { contentDescription = description }) { drawShape(shape, color, angle) }
}

private fun DrawScope.drawShape(shape: StateShape, color: Color, angle: Float) {
    val w = size.minDimension
    val stroke = Stroke(width = w * 0.12f, cap = StrokeCap.Round)
    val inset = w * 0.1f
    val box = Size(w - 2 * inset, w - 2 * inset)
    when (shape) {
        StateShape.Spinner -> rotate(angle) {
            drawArc(color.copy(alpha = 0.25f), 0f, 360f, false, Offset(inset, inset), box, style = stroke)
            drawArc(color, 0f, 100f, false, Offset(inset, inset), box, style = stroke)
        }
        StateShape.Ring -> drawCircle(color, radius = box.width / 2, style = stroke)
        StateShape.Pause -> {
            drawCircle(color, radius = box.width / 2, style = stroke)
            drawLine(color, Offset(w * 0.4f, w * 0.33f), Offset(w * 0.4f, w * 0.67f), strokeWidth = w * 0.1f, cap = StrokeCap.Round)
            drawLine(color, Offset(w * 0.6f, w * 0.33f), Offset(w * 0.6f, w * 0.67f), strokeWidth = w * 0.1f, cap = StrokeCap.Round)
        }
        StateShape.Dot -> drawCircle(color, radius = w * 0.22f)
        StateShape.Dashed -> drawCircle(color, radius = box.width / 2, style = Stroke(width = w * 0.12f, pathEffect = androidx.compose.ui.graphics.PathEffect.dashPathEffect(floatArrayOf(w * 0.14f, w * 0.12f))))
        StateShape.Triangle -> {
            val p = Path().apply {
                moveTo(w / 2, inset); lineTo(w - inset, w - inset); lineTo(inset, w - inset); close()
            }
            drawPath(p, color)
            drawLine(Color.Black.copy(alpha = 0.7f), Offset(w / 2, w * 0.38f), Offset(w / 2, w * 0.62f), strokeWidth = w * 0.1f, cap = StrokeCap.Round)
            drawCircle(Color.Black.copy(alpha = 0.7f), radius = w * 0.055f, center = Offset(w / 2, w * 0.76f))
        }
        StateShape.Check -> {
            drawCircle(color, radius = box.width / 2, style = stroke)
            val p = Path().apply { moveTo(w * 0.3f, w * 0.52f); lineTo(w * 0.44f, w * 0.66f); lineTo(w * 0.72f, w * 0.36f) }
            drawPath(p, color, style = stroke)
        }
        // A ring filled to a bar: a quota at its end.
        StateShape.Gauge -> {
            drawCircle(color, radius = box.width / 2, style = stroke)
            drawLine(color, Offset(w * 0.3f, w * 0.5f), Offset(w * 0.7f, w * 0.5f), strokeWidth = w * 0.14f, cap = StrokeCap.Round)
        }
        StateShape.Cross -> {
            drawCircle(color, radius = box.width / 2, style = stroke)
            drawLine(color, Offset(w * 0.33f, w * 0.33f), Offset(w * 0.67f, w * 0.67f), strokeWidth = w * 0.12f, cap = StrokeCap.Round)
            drawLine(color, Offset(w * 0.67f, w * 0.33f), Offset(w * 0.33f, w * 0.67f), strokeWidth = w * 0.12f, cap = StrokeCap.Round)
        }
    }
}
