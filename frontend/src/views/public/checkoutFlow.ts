import { onMounted, onScopeDispose, ref, watch, type Ref } from 'vue'
import { CheckoutRequestError } from '@/api/checkout'

const POLLING_INTERVAL = 4000
const FAST_CHECKS = 15
const SLOW_POLLING_INTERVAL = 30000
const STATUS_PROCESSING = new Set(['PENDING', 'PAID', 'RECHARGING'])
const BLOCKED_RETURN_SCHEMES = new Set([
  'http:', 'javascript:', 'data:', 'file:', 'blob:', 'about:', 'intent:', 'market:',
  'chrome:', 'chrome-extension:', 'resource:', 'view-source:', 'filesystem:', 'vbscript:',
  'ftp:', 'ws:', 'wss:',
])

export function checkoutURL(value?: string, allowAppScheme = false): string {
  // URL parsing can silently strip embedded control characters from destinations.
  // eslint-disable-next-line no-control-regex
  if (!value || !/^[a-z][a-z\d+.-]*:\/\//i.test(value) || /[\s\\\u0000-\u001f\u007f]/.test(value)) return ''
  try {
    const url = new URL(value)
    if (!url.hostname || url.username || url.password) return ''
    if (url.protocol === 'https:' || (allowAppScheme && !BLOCKED_RETURN_SCHEMES.has(url.protocol))) return url.href
  } catch {
    // An invalid destination must never be used for navigation.
  }
  return ''
}

export function isCheckoutProcessing(status?: string): boolean {
  return !!status && STATUS_PROCESSING.has(status)
}

export function isCheckoutAccessError(error: unknown): boolean {
  return error instanceof CheckoutRequestError && ([400, 401, 403, 404, 410].includes(error.status) || error.reason === 'CHECKOUT_EXPIRED')
}

export function retryDelay(error: unknown): number | false {
  if (isCheckoutAccessError(error)) return false
  return error instanceof CheckoutRequestError && error.retryAfter
    ? Math.min(Math.max(POLLING_INTERVAL, error.retryAfter * 1000), 2_147_483_647)
    : POLLING_INTERVAL
}

export function useCheckoutPolling(check: (signal: AbortSignal) => Promise<boolean | number>, canRefresh: () => boolean) {
  const checking = ref(false)
  let controller: AbortController | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let attempts = 0
  let retryAt = 0

  function stop() {
    controller?.abort()
    controller = undefined
    clearTimeout(timer)
    checking.value = false
    retryAt = 0
  }

  async function run() {
    clearTimeout(timer)
    const active = new AbortController()
    controller = active
    checking.value = true
    let next: boolean | number = false
    try {
      next = await check(active.signal)
    } finally {
      if (!active.signal.aborted) {
        controller = undefined
        checking.value = false
        retryAt = typeof next === 'number' ? Date.now() + next : 0
        if (next) {
          const interval = ++attempts < FAST_CHECKS ? POLLING_INTERVAL : SLOW_POLLING_INTERVAL
          timer = setTimeout(() => {
            if (document.visibilityState !== 'hidden') void run()
          }, typeof next === 'number' ? Math.max(next, interval) : interval)
        }
      }
    }
  }

  function refresh() {
    if (checking.value) return
    attempts = 0
    void run()
  }

  // Browsers throttle background timers; check as soon as the user comes back.
  function onReturn() {
    if (document.visibilityState === 'hidden' || checking.value || !canRefresh()) return
    if (Date.now() >= retryAt) refresh()
    else {
      clearTimeout(timer)
      timer = setTimeout(onReturn, Math.max(0, retryAt - Date.now()))
    }
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onReturn)
    window.addEventListener('focus', onReturn)
    window.addEventListener('pageshow', onReturn)
  })
  onScopeDispose(() => {
    document.removeEventListener('visibilitychange', onReturn)
    window.removeEventListener('focus', onReturn)
    window.removeEventListener('pageshow', onReturn)
    stop()
  })
  return { checking, refresh, stop }
}

export function autoReturnOnce(link: Ref<HTMLAnchorElement | null>) {
  let attempted = false
  watch(link, (element) => {
    if (!element || attempted) return
    attempted = true
    element.click()
  }, { flush: 'post' })
}
