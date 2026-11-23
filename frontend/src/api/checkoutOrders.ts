import type { OrderType } from '@/types/payment'
import { apiClient } from './client'
import type { CheckoutOrder } from './checkout'

interface CheckoutLink {
  order_id: string
  checkout_url: string
  /** Open this in the system browser; share only checkout_url. */
  open_url?: string
  expires_at: string
}

type CheckoutOrderInput =
  | { order_type: Extract<OrderType, 'balance'>; amount: number }
  | { order_type: Extract<OrderType, 'subscription'>; plan_id: number }

export const checkoutOrdersAPI = {
  /** Reuse the same idempotency key when retrying creation after an ambiguous failure. */
  async create(order: CheckoutOrderInput, idempotencyKey: string) {
    const { data } = await apiClient.post<CheckoutLink>('/payment/checkout/orders', order, {
      headers: { 'Idempotency-Key': idempotencyKey },
    })
    return data
  },
  async getOrder(id: string) {
    const { data } = await apiClient.get<CheckoutOrder>(`/payment/checkout/orders/${encodeURIComponent(id)}`)
    return data
  },
  async reissueLink(id: string) {
    const { data } = await apiClient.post<CheckoutLink>(`/payment/checkout/orders/${encodeURIComponent(id)}/link`)
    return data
  },
}
