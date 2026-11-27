import './index.css'
import { boot } from './boot'

// The bundled fonts are local files: wait for them (a second at most) before the first render, so nothing is laid out
// in a fallback font and then shifts (a terminal would be resized under its program).
const fonts = Promise.all(['14px "Geist Variable"', '600 14px "Geist Variable"', '13px "JetBrains Mono Variable"'].map((f) => document.fonts.load(f)))
void Promise.race([fonts, new Promise((r) => setTimeout(r, 1000))])
  .catch(() => {})
  .then(() => boot(document.getElementById('root')!, window.desktop))
