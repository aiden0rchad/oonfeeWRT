export function registerControllerApp() {
  if (!window.isSecureContext || !('serviceWorker' in navigator)) return
  // Failure affects installation/offline guidance only, never normal sign-in.
  void navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' }).catch(() => {})
}
