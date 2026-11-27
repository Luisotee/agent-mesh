package dev.agentmesh.android.ui

import androidx.compose.material3.Typography
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.TextUnit
import androidx.compose.ui.unit.sp
import dev.agentmesh.android.R

/**
 * The two bundled variable fonts, the desktop's pair (OFL 1.1, `android/licenses/`): Geist for the interface and
 * JetBrains Mono for code, the terminal and ids. The weights in use are 400, 500 and 600: the system's 700 is too
 * heavy at phone sizes (happy clamps its weights for the same reason).
 */
val Geist = FontFamily(listOf(400, 500, 600).map { w -> Font(R.font.geist, FontWeight(w), variationSettings = FontVariation.Settings(FontVariation.weight(w))) })
val JetBrainsMono = FontFamily(listOf(400, 500).map { w -> Font(R.font.jetbrains_mono, FontWeight(w), variationSettings = FontVariation.Settings(FontVariation.weight(w))) })

private fun style(size: Int, line: Int, weight: Int = 400, family: FontFamily = Geist, tracking: TextUnit = 0.sp) =
    TextStyle(fontFamily = family, fontSize = size.sp, lineHeight = line.sp, fontWeight = FontWeight(weight), letterSpacing = tracking)

/**
 * The type scale. A phone is read closer and in motion, so the chat's prose is 16 (the desktop's 15), the lists' rows
 * 15, their meta 13, and nothing is under 12. Titles are sentence case at 500 or 600, never tracked capitals.
 */
val MeshTypography = Typography(
    displayLarge = style(32, 40, 600), displayMedium = style(28, 36, 600), displaySmall = style(24, 32, 600),
    headlineLarge = style(24, 32, 600), headlineMedium = style(22, 28, 600), headlineSmall = style(20, 28, 600),
    titleLarge = style(20, 28, 600), titleMedium = style(16, 24, 500), titleSmall = style(15, 20, 500),
    bodyLarge = style(16, 24), bodyMedium = style(15, 22), bodySmall = style(13, 18),
    labelLarge = style(14, 20, 500), labelMedium = style(13, 16, 500), labelSmall = style(12, 16, 500),
)

val Mono = JetBrainsMono
val MonoSmall = TextStyle(fontFamily = JetBrainsMono, fontSize = 13.sp, lineHeight = 18.sp)
