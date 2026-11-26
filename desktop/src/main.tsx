import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './index.css'
import { DEFAULT_THEME, applyTheme, themeByName } from './theme'

applyTheme(themeByName(DEFAULT_THEME))
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App api={window.desktop} />
  </StrictMode>,
)
