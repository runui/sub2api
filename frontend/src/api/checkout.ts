import type { ApiResponse } from '@/types'
import type { CreateOrderResult, OrderStatus, OrderType } from '@/types/payment'
import { buildApiUrl } from './url'

export interface CheckoutOrder {
  order_id: string
  status: OrderStatus | 'AWAITING_CONFIRMATION'
  order_type: OrderType
  amount: number
  plan_id?: number
  plan_name?: string
  plan_currency?: string
  payment_type?: string
  pay_amount?: number
  currency?: string
  expires_at: string
  methods?: Record<string, CheckoutMethodLimit>
  can_pay?: boolean
  can_switch_method?: boolean
  can_cancel?: boolean
  pay_url?: string
  qr_code?: string
  payment_action?: CheckoutPaymentAction
  client_return_url?: string
}

interface CheckoutMethodLimit {
  payment_type: string
  display_name?: string
  currency: string
  fee_rate: number
  daily_limit: number
  single_min: number
  single_max: number
}

export interface CheckoutQuote {
  quote_id: string
  payment_type: string
  pay_amount: number
  currency: string
  amount: number
  fee_rate: number
}

export interface CheckoutPaymentAction {
  client_secret?: string
  publishable_key?: string
  intent_id?: string
  country_code?: string
  payment_env?: string
  jsapi?: CreateOrderResult['jsapi']
  alipay_deep_link?: boolean
}

type CheckoutPayment = Omit<CreateOrderResult, 'order_id'> & { order_id: string; status?: OrderStatus; payment_action?: CheckoutPaymentAction }

export class CheckoutRequestError extends Error {
  constructor(readonly status: number, readonly reason?: string, readonly retryAfter?: number) {
    super('Checkout request failed')
  }
}

const CHECKOUT_READ_TIMEOUT_MS = 5000
// Pay may wait for the provider's create call; reads and quotes never do.
const CHECKOUT_PAY_TIMEOUT_MS = 30000

function actionToken(id: string): string {
  try {
    return sessionStorage.getItem(`checkout-action:${id}`) || ''
  } catch {
    // Storage may be disabled; payment and status still work.
    return ''
  }
}

export function captureCheckoutActionToken(id: string, hash: string): void {
  const tokens = new URLSearchParams(hash.slice(1)).getAll('action_token')
  if (tokens.length !== 1 || !/^\d{1,12}\.[A-Za-z0-9_-]{43}$/.test(tokens[0])) return
  try {
    sessionStorage.setItem(`checkout-action:${id}`, tokens[0])
  } catch {
    // Payment and status are independent of the action credential.
  }
}

async function request<T>(path: string, signal: AbortSignal, data?: object, timeoutMs = CHECKOUT_READ_TIMEOUT_MS, token = '') {
  const controller = new AbortController()
  const cancel = () => controller.abort()
  if (signal.aborted) cancel()
  else signal.addEventListener('abort', cancel, { once: true })
  const timer = setTimeout(cancel, timeoutMs)
  // The shared authenticated client reads browser storage and can turn a 401 into a login redirect.
  // Fetch with credentials omitted also suppresses same-origin cookies, unlike XHR's withCredentials.
  try {
    const response = await fetch(buildApiUrl(`/payment/public/checkout${path}`), {
      method: data ? 'POST' : 'GET',
      headers: {
        ...(data ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { 'X-Checkout-Action-Token': token } : {}),
      },
      credentials: 'omit',
      cache: 'no-store',
      redirect: 'error',
      referrerPolicy: 'no-referrer',
      signal: controller.signal,
      body: data ? JSON.stringify(data) : undefined,
    })
    const body = await response.json().catch(() => undefined) as (ApiResponse<T> & { reason?: string }) | undefined
    const retryHeader = response.headers.get('retry-after')
    const retryAfter = retryHeader && /^\d+$/.test(retryHeader) ? Number(retryHeader) : undefined
    if (response.status === 202) return { status: 202, data: body?.data, retryAfter }
    if (!response.ok || body?.code !== 0 || body.data == null) {
      throw new CheckoutRequestError(response.status, body?.reason, retryAfter)
    }
    return { status: response.status, data: body.data, retryAfter }
  } finally {
    clearTimeout(timer)
    signal.removeEventListener('abort', cancel)
  }
}

function orderPath(id: string) {
  return `/orders/${encodeURIComponent(id)}`
}

export const checkoutAPI = {
  async getOrder(id: string, signal: AbortSignal) {
    const { data } = await request<CheckoutOrder>(orderPath(id), signal, undefined, CHECKOUT_READ_TIMEOUT_MS, actionToken(id))
    if (!data) throw new CheckoutRequestError(202)
    return data
  },
  async quote(id: string, paymentType: string, signal: AbortSignal) {
    const { data } = await request<CheckoutQuote>(`${orderPath(id)}/quote`, signal, { payment_type: paymentType })
    if (!data) throw new CheckoutRequestError(202)
    return data
  },
  pay(id: string, paymentType: string, quoteId: string, isMobile: boolean, signal: AbortSignal, wechatResumeToken?: string) {
    return request<CheckoutPayment>(`${orderPath(id)}/pay`, signal, {
      payment_type: paymentType,
      quote_id: quoteId,
      is_mobile: isMobile,
      ...(wechatResumeToken ? { wechat_resume_token: wechatResumeToken } : {}),
    }, CHECKOUT_PAY_TIMEOUT_MS)
  },
  async switchMethod(id: string, signal: AbortSignal) {
    const { data } = await request<{ order_id: string; open_url?: string }>(`${orderPath(id)}/switch-method`, signal, {}, CHECKOUT_PAY_TIMEOUT_MS, actionToken(id))
    if (!data || !/^[a-f0-9]{32}$/.test(data.order_id) || data.order_id === id) throw new CheckoutRequestError(502)
    const fragment = data.open_url?.split('#', 2)[1]
    if (fragment) captureCheckoutActionToken(data.order_id, `#${fragment}`)
    return data.order_id
  },
  async cancel(id: string, signal: AbortSignal) {
    const { data } = await request<{ status: string }>(`${orderPath(id)}/cancel`, signal, {}, CHECKOUT_READ_TIMEOUT_MS, actionToken(id))
    if (data?.status !== 'CANCELLED') throw new CheckoutRequestError(502)
  },
}
