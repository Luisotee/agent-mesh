package dev.agentmesh.android

import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import dev.agentmesh.android.net.PairingLink
import dev.agentmesh.android.pair.QrDecoder
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import java.nio.ByteBuffer

/** The camera's frames can't be had here, but a QR code made by ZXing's writer is the same picture the scanner reads. */
class QrDecoderTest {
    private val link = "agentmesh://pair?v=1&h=h1234567890ab&fp=sha256:${"ab".repeat(32)}&t=amp_0123456789abcdef0123456789abcdef" +
        "&hn=work+station&r=https%3A%2F%2Frelay.example.com&rtcp=relay.example.com%3A8443&rt=amr_c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0&d=https%3A%2F%2F100.64.0.1%3A9443"

    private fun luminance(text: String, size: Int = 480, transpose: Boolean = false, invert: Boolean = false): Pair<ByteArray, Int> {
        val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size, mapOf(EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.M))
        val out = ByteArray(size * size)
        for (y in 0 until size) for (x in 0 until size) {
            val black = if (transpose) m.get(y, x) else m.get(x, y)
            out[y * size + x] = if (black != invert) 0 else 255.toByte()
        }
        return out to size
    }

    @Test fun `a QR code of a real pairing link decodes to that link`() {
        val (y, size) = luminance(link)
        val text = QrDecoder.decode(y, size, size)
        assertEquals(link, text)
        assertEquals("h1234567890ab", PairingLink.parse(text!!).hostId)
    }

    @Test fun `it reads a rotated code and a light-on-dark one`() {
        val (rot, size) = luminance(link, transpose = true)
        assertEquals(link, QrDecoder.decode(rot, size, size))
        val (inv, s2) = luminance(link, invert = true)
        assertEquals(link, QrDecoder.decode(inv, s2, s2))
    }

    @Test fun `a frame with no code gives nothing`() {
        assertNull(QrDecoder.decode(ByteArray(200 * 200) { 128.toByte() }, 200, 200))
        assertNull(QrDecoder.decode(ByteArray(200 * 200) { (it * 7).toByte() }, 200, 200))
    }

    @Test fun `a camera plane with padded rows is made tight before decoding`() {
        val (y, size) = luminance(link, size = 300)
        val stride = size + 13
        val padded = ByteBuffer.allocate(stride * size)
        for (r in 0 until size) { padded.position(r * stride); padded.put(y, r * size, size) }
        val tight = QrDecoder.tight(padded, size, size, stride, 1)
        assertEquals(y.toList(), tight.toList())
        assertEquals(link, QrDecoder.decode(tight, size, size))
    }

    @Test fun `a plane that is short on its last row, or has a pixel stride, is read without an exception`() {
        val (y, size) = luminance(link, size = 300)
        // No padding on the last row: the buffer ends where the picture does.
        val stride = size + 13
        val short = ByteBuffer.allocate(stride * (size - 1) + size)
        for (r in 0 until size) { short.position(r * stride); short.put(y, r * size, size) }
        assertEquals(link, QrDecoder.decode(QrDecoder.tight(short, size, size, stride, 1), size, size))
        // Every other byte (a pixel stride of 2).
        val spaced = ByteBuffer.allocate(size * 2 * size)
        for (r in 0 until size) for (c in 0 until size) spaced.put(r * size * 2 + c * 2, y[r * size + c])
        assertEquals(y.toList(), QrDecoder.tight(spaced, size, size, size * 2, 2).toList())
        // A buffer far too small gives a (dark) picture, not a crash.
        assertNull(QrDecoder.decode(QrDecoder.tight(ByteBuffer.allocate(50), size, size, size, 1), size, size))
    }
}
