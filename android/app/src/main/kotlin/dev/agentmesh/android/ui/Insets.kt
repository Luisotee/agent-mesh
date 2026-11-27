package dev.agentmesh.android.ui

import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.ui.Modifier

/**
 * The content of a Scaffold: inside the padding it hands over, and with the insets that padding holds (the bars) counted as
 * used. A Scaffold does not say so itself, so without this an `imePadding()` below adds the keyboard's whole inset, which
 * includes the navigation bar the padding already has: the nav bar's height shows twice, as a blank band over the keyboard.
 */
fun Modifier.inScaffold(pad: PaddingValues): Modifier = padding(pad).consumeWindowInsets(pad)

/** [inScaffold] for a screen with text fields, which the soft keyboard must not cover (edge-to-edge apps are not resized by it). */
fun Modifier.inScaffoldWithKeyboard(pad: PaddingValues): Modifier = inScaffold(pad).imePadding()
