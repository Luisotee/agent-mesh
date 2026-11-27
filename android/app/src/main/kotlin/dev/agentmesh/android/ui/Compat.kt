package dev.agentmesh.android.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.State
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.StateFlow

@Composable
fun <T> StateFlow<T>.collectAsStateWithLifecycleCompat(): State<T> = collectAsStateWithLifecycle()

/** A flow that isn't a state, collected only while the screen is started, from [initial] until it has said something. */
@Composable
fun <T> Flow<T>.collectAsStateWithLifecycleCompat(initial: T): State<T> = collectAsStateWithLifecycle(initial)
