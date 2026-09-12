import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './lib/tokens.css'
import { App } from './App'
import { registerControllerApp } from './lib/pwa'

if (import.meta.env.PROD) registerControllerApp()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
