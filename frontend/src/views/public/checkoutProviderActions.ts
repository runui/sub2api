import type { CheckoutPaymentAction } from '@/api/checkout'

export async function openAirwallexCheckout(action: CheckoutPaymentAction, currency: string, returnURL: string, locale: string, signal: AbortSignal) {
  if (!action.intent_id || !action.client_secret) throw new Error('Missing Airwallex payment action')
  const { init } = await import('@airwallex/components-sdk')
  if (signal.aborted) return
  const { payments } = await init({
    env: action.payment_env === 'prod' ? 'prod' : 'demo',
    enabledElements: ['payments'],
    locale: locale.toLowerCase().startsWith('zh') ? 'zh' : 'en',
  })
  if (signal.aborted) return
  if (!payments) throw new Error('Airwallex checkout unavailable')
  const target = payments.redirectToCheckout({ intent_id: action.intent_id, client_secret: action.client_secret,
    currency, country_code: action.country_code, successUrl: returnURL })
  if (typeof target === 'string' && target) window.location.assign(target)
}

export function invokeCheckoutWeChat(payload: NonNullable<CheckoutPaymentAction['jsapi']>): Promise<string> {
  const bridge = (window as Window & { WeixinJSBridge?: {
    invoke: (method: string, params: typeof payload, callback: (result: { err_msg?: string }) => void) => void
  } }).WeixinJSBridge
  if (!bridge) return Promise.reject(new Error('WeChat payment bridge unavailable'))
  return new Promise(resolve => bridge.invoke('getBrandWCPayRequest', payload, result => resolve(result?.err_msg || '')))
}
