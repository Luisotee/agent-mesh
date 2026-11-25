package dev.agentmesh.android.pair

import android.os.Handler
import android.os.Looper
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import dev.agentmesh.android.net.PairingLink
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

/**
 * The camera, looking for a pairing QR code: a preview, and each frame's luminance through [QrDecoder]. It gives the
 * first text that holds a pairing link to [onCode] (once), and says through [onError] when there is no camera to use.
 * Needs the CAMERA permission already granted. This wiring runs only on a device: the decoding it calls is tested.
 */
@Composable
fun QrScanner(onCode: (String) -> Unit, onError: (String) -> Unit, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val owner = LocalLifecycleOwner.current
    val view = remember { PreviewView(context) }
    val code by rememberUpdatedState(onCode)
    val error by rememberUpdatedState(onError)
    DisposableEffect(owner) {
        val finished = AtomicBoolean(false)
        val executor = Executors.newSingleThreadExecutor()
        val main = Handler(Looper.getMainLooper())
        val future = ProcessCameraProvider.getInstance(context)
        future.addListener({
            try {
                val provider = future.get()
                val preview = Preview.Builder().build().also { it.surfaceProvider = view.surfaceProvider }
                val analysis = ImageAnalysis.Builder().setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST).build()
                analysis.setAnalyzer(executor) { image ->
                    try {
                        if (!finished.get()) {
                            val plane = image.planes[0]
                            val y = QrDecoder.tight(plane.buffer, image.width, image.height, plane.rowStride, plane.pixelStride)
                            val text = QrDecoder.decode(y, image.width, image.height)
                            if (text != null && PairingLink.find(text) != null && finished.compareAndSet(false, true)) main.post { code(text) }
                        }
                    } finally {
                        image.close()
                    }
                }
                provider.unbindAll()
                provider.bindToLifecycle(owner, CameraSelector.DEFAULT_BACK_CAMERA, preview, analysis)
            } catch (e: Exception) {
                error("The camera isn't available (${e.message ?: e.javaClass.simpleName}). Paste the link instead.")
            }
        }, ContextCompat.getMainExecutor(context))
        onDispose {
            finished.set(true)
            runCatching { future.get().unbindAll() }
            executor.shutdown()
        }
    }
    AndroidView(factory = { view }, modifier = modifier)
}
