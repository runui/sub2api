import { afterEach, describe, expect, it, vi } from 'vitest'
import { captureCheckoutActionToken, checkoutAPI } from '../checkout'

afterEach(() => {
  sessionStorage.clear()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('anonymous checkout API', () => {
  it('reads a checkout by order number without credentials or a ticket', async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify({ code: 0, data: {} })))
    vi.stubGlobal('fetch', fetch)
    const { signal } = new AbortController()
    await checkoutAPI.getOrder('42', signal)

    expect(fetch).toHaveBeenNthCalledWith(1, '/api/v1/payment/public/checkout/orders/42', expect.objectContaining({
      credentials: 'omit', cache: 'no-store', redirect: 'error', headers: {},
    }))
  })

  it('preserves a processing response and Retry-After for pay', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 202, headers: { 'Retry-After': '3' } })))
    await expect(checkoutAPI.pay('42', 'wxpay', '', false, new AbortController().signal))
      .resolves.toMatchObject({ status: 202, retryAfter: 3 })
  })

  it('sends the action credential only in request headers, not API URLs', async () => {
    const replacement = 'fedcba9876543210fedcba9876543210'
    const fetch = vi.fn(async (_url: string, _opts: RequestInit) => Response.json({ code: 0, data: { order_id: replacement } }))
    vi.stubGlobal('fetch', fetch)
    const { signal } = new AbortController()
    const token = `2000000000.${'a'.repeat(43)}`
    captureCheckoutActionToken('42', `#action_token=${token}`)
    await checkoutAPI.getOrder('42', signal)
    expect(new Headers(fetch.mock.calls[0][1].headers).get('X-Checkout-Action-Token')).toBe(token)
    await expect(checkoutAPI.switchMethod('42', signal)).resolves.toBe(replacement)
    expect(fetch.mock.calls[1][0]).toBe('/api/v1/payment/public/checkout/orders/42/switch-method')
    expect(new Headers(fetch.mock.calls[1][1].headers).get('X-Checkout-Action-Token')).toBe(token)
    sessionStorage.clear()
    await checkoutAPI.getOrder('42', signal)
    await checkoutAPI.pay('42', 'wxpay', '', false, signal)
    expect(new Headers(fetch.mock.calls[2][1].headers).has('X-Checkout-Action-Token')).toBe(false)
    expect(new Headers(fetch.mock.calls[3][1].headers).has('X-Checkout-Action-Token')).toBe(false)
  })

  it.each(['read', 'quote', 'pay'] as const)('bounds a stalled %s request', async (kind) => {
    vi.useFakeTimers()
    const fetch = vi.fn((_url: string, options: RequestInit) => new Promise<Response>((_, reject) => {
      options.signal!.addEventListener('abort', () => reject(options.signal!.reason), { once: true })
    }))
    vi.stubGlobal('fetch', fetch)
    const { signal } = new AbortController()
    const request = kind === 'read' ? checkoutAPI.getOrder('42', signal)
      : kind === 'quote' ? checkoutAPI.quote('42', 'wxpay', signal)
        : checkoutAPI.pay('42', 'wxpay', '', false, signal)
    const rejected = expect(request).rejects.toMatchObject({ name: 'AbortError' })
    const timeout = kind === 'pay' ? 30000 : 5000
    await vi.advanceTimersByTimeAsync(timeout - 1)
    expect(fetch.mock.calls[0][1].signal?.aborted).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    await rejected
  })
})
