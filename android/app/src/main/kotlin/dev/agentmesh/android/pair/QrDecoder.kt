package dev.agentmesh.android.pair

import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.NotFoundException
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader

/**
 * Reads a QR code out of a camera frame's luminance (the Y plane of a YUV image): the pairing link is its text. Pure,
 * so a generated code can be decoded in a JVM test; the camera wiring around it ([QrScanner]) is the only part that
 * needs a device. A QR code reads at any rotation, so the sensor's orientation doesn't matter.
 */
object QrDecoder {
    private val hints = mapOf(DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE), DecodeHintType.TRY_HARDER to true)

    /** The text of the QR code in [y] (`width * height` luminance bytes, row after row), or null when there is none in this frame. */
    fun decode(y: ByteArray, width: Int, height: Int): String? {
        val source = PlanarYUVLuminanceSource(y, width, height, 0, 0, width, height, false)
        for (candidate in listOf(source, source.invert())) {
            try {
                return QRCodeReader().decode(BinaryBitmap(HybridBinarizer(candidate)), hints).text
            } catch (_: NotFoundException) {
            } catch (_: com.google.zxing.ChecksumException) {
            } catch (_: com.google.zxing.FormatException) {
            }
        }
        return null
    }

    /** The Y plane as a tight `width * height` array: a camera's rows are padded to [rowStride] (and [pixelStride] apart). */
    fun tight(plane: java.nio.ByteBuffer, width: Int, height: Int, rowStride: Int, pixelStride: Int): ByteArray {
        val out = ByteArray(width * height)
        val row = ByteArray(rowStride)
        for (r in 0 until height) {
            val start = r * rowStride
            if (start >= plane.limit()) break
            plane.position(start)
            // The last row may be shorter than a padded one: read what is there, and leave the rest black.
            val n = minOf(rowStride, plane.remaining())
            java.util.Arrays.fill(row, 0)
            plane.get(row, 0, n)
            for (c in 0 until width) {
                val i = c * pixelStride
                if (i < n) out[r * width + c] = row[i]
            }
        }
        return out
    }
}
