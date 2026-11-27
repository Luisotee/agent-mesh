package dev.agentmesh.android.ui

import androidx.compose.foundation.border
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.vector.PathParser
import androidx.compose.ui.graphics.withSave
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.agentmesh.android.derive.Vendor
import dev.agentmesh.android.derive.vendorOf
import dev.agentmesh.android.theme.VendorColors

/** Each vendor's colour in the theme in use; an unknown vendor is quiet. */
fun vendorColor(v: Vendor, c: VendorColors, quiet: Color): Color = when (v) {
    Vendor.Claude -> c.claude
    Vendor.Gemini -> c.gemini
    Vendor.OpenAI -> c.openai
    Vendor.Copilot -> c.copilot
    Vendor.OpenCode -> c.opencode
    Vendor.DeepSeek -> c.deepseek
    Vendor.Other -> quiet
}

private val MARK_KEY = mapOf(Vendor.Claude to "claude", Vendor.Gemini to "gemini", Vendor.Copilot to "copilot", Vendor.OpenCode to "opencode", Vendor.DeepSeek to "deepseek")

/** The marks' paths, parsed once: they are 24x24 and stay in that box until drawn. */
private val PATHS: Map<String, Path> by lazy { VENDOR_MARKS.mapValues { (_, d) -> PathParser().parsePathString(d).toPath() } }

/**
 * The mark of the vendor behind a CLI or a model, in the vendor's brand colour (the desktop's `VendorIcon`): by the
 * agent's CLI `kind` and `model`, or by a [vendor] given. OpenAI has no mark in the set and an unknown kind has none at
 * all: both get a letter badge, so a row never lacks its icon.
 */
@Composable
fun VendorIcon(kind: String = "", model: String = "", vendor: Vendor? = null, size: Dp = 16.dp, modifier: Modifier = Modifier) {
    val v = vendor ?: vendorOf(kind, model)
    val color = vendorColor(v, LocalMeshTheme.current.vendors, MaterialTheme.colorScheme.onSurfaceVariant)
    val label = if (v == Vendor.Other && kind.isNotEmpty()) kind else v.label
    val path = MARK_KEY[v]?.let { PATHS[it] }
    if (path != null) {
        Canvas(modifier.size(size).semantics { contentDescription = label }) {
            val k = this.size.minDimension / 24f
            drawContext.canvas.withSave {
                drawContext.canvas.scale(k, k)
                drawPath(path, color)
            }
        }
    } else {
        val letter = if (v == Vendor.OpenAI) "O" else kind.trim().take(1).uppercase().ifEmpty { "?" }
        Box(
            modifier.size(size).border(1.dp, color, RoundedCornerShape(size / 5)).semantics { contentDescription = label },
            contentAlignment = Alignment.Center,
        ) { Text(letter, color = color, fontSize = (size.value * 0.6f).sp, lineHeight = (size.value * 0.6f).sp, fontWeight = FontWeight.SemiBold) }
    }
}
