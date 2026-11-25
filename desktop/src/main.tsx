import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './index.css'
import { applyTheme, dracula } from './theme'

applyTheme(dracula)
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App api={window.desktop} />
  </StrictMode>,
)
