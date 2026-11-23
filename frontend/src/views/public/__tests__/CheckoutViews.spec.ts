import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { ref } from 'vue'
import QRCode from 'qrcode'
import BaseDialog from '@/components/common/BaseDialog.vue'
import CheckoutOrderView from '../CheckoutOrderView.vue'
import { checkoutAPI, CheckoutRequestError } from '@/api/checkout'

const route = vi.hoisted(() => ({ hash: '', query: {} as Record<string, string>, params: { orderId: '0123456789abcdef0123456789abcdef' } }))
const number = route.params.orderId
const push = vi.hoisted(() => vi.fn())
const replace = vi.hoisted(() => vi.fn())
vi.mock('vue-router', () => ({ useRoute: () => route, useRouter: () => ({ push, replace }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: ref('en') }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ siteName: 'Sub2API' }) }))
vi.mock('@/api/checkout', async (importOriginal) => ({
  ...await importOriginal<typeof import('@/api/checkout')>(),
  checkoutAPI: { getOrder: vi.fn(), quote: vi.fn(), pay: vi.fn(), switchMethod: vi.fn(), cancel: vi.fn() },
}))
vi.mock('qrcode', () => ({ default: { toDataURL: vi.fn() } }))
vi.mock('../CheckoutStripeForm.vue', () => ({ default: { template: '<p>Stripe Payment Element</p>' } }))
vi.mock('../checkoutProviderActions', () => ({
  invokeCheckoutWeChat: vi.fn(), openAirwallexCheckout: vi.fn(),
}))
import { invokeCheckoutWeChat, openAirwallexCheckout } from '../checkoutProviderActions'

const order = {
  order_id: number, status: 'AWAITING_CONFIRMATION', order_type: 'subscription', amount: 25,
  plan_name: 'Starter', plan_currency: 'NZD', expires_at: '2100-01-01T00:00:00Z',
  methods: { wxpay: { payment_type: 'wxpay', currency: 'CNY', display_name: 'WeChat', daily_limit: 0, single_min: 0, single_max: 0, fee_rate: 1 } },
} as const
const result = { ...order, status: 'PENDING', client_return_url: `exampleapp://return/checkout?order_id=${number}` } as const
const mountOrder = () => mount(CheckoutOrderView)
const findButton = (wrapper: ReturnType<typeof mountOrder>, key: string) => wrapper.findAll('button').find((button) => button.text() === `payment.checkout.${key}`)!

enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.resetAllMocks()
  route.hash = ''
  route.query = {}
  vi.spyOn(window, 'open').mockReturnValue(null)
  vi.mocked(QRCode.toDataURL).mockImplementation(() => Promise.resolve('data:image/png;base64,AA'))
})
afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

describe('checkout order', () => {
  it.each([
    ['stripe', { client_secret: 'pi_secret', publishable_key: 'pk_test' }],
    ['airwallex', { client_secret: 'awx_secret', intent_id: 'int_test' }],
  ])('reopens the %s provider tab from the same order without exposing credentials in the link', async (paymentType, paymentAction) => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, payment_type: paymentType, payment_action: paymentAction })
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.get('a').attributes('href')).toBe(`${window.location.origin}/payment/checkout/${number}?provider=${paymentType}`)
    expect(wrapper.get('a').attributes('target')).toBe('_blank')
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
  })

  it('mounts Stripe in the provider tab and removes Stripe return parameters', async () => {
    route.query = { provider: 'stripe', payment_intent_client_secret: 'pi_secret', redirect_status: 'failed' }
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, payment_type: 'stripe',
      payment_action: { client_secret: 'pi_secret', publishable_key: 'pk_test' } })
    const wrapper = mountOrder()
    await flushPromises()
    expect(replace).toHaveBeenCalledWith(expect.objectContaining({ query: { provider: 'stripe' } }))
    expect(wrapper.text()).toContain('Stripe Payment Element')
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
  })

  it('opens Airwallex with the saved intent and returns to the same order', async () => {
    route.query = { provider: 'airwallex' }
    const action = { client_secret: 'awx_secret', intent_id: 'int_test', payment_env: 'prod' }
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, payment_type: 'airwallex', currency: 'USD', payment_action: action })
    mountOrder()
    await flushPromises()
    expect(openAirwallexCheckout).toHaveBeenCalledWith(action, 'USD', `${window.location.origin}/payment/checkout/${number}`, 'en', expect.any(AbortSignal))
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
  })

  it('invokes WeChat with the saved JSAPI action without treating its callback as fulfillment', async () => {
    const jsapi = { appId: 'wx123', package: 'prepay_id=abc' }
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, payment_type: 'wxpay', payment_action: { jsapi } })
    vi.mocked(invokeCheckoutWeChat).mockResolvedValue('get_brand_wcpay_request:ok')
    const wrapper = mountOrder()
    await flushPromises()
    await findButton(wrapper, 'openProvider').trigger('click')
    await flushPromises()
    expect(invokeCheckoutWeChat).toHaveBeenCalledWith(jsapi)
    expect(wrapper.text()).toContain('payment.checkout.processing')
    expect(wrapper.text()).not.toContain('payment.checkout.completed')
  })
  it.each([false, true])('offers cancellation on the confirmation page only with permission (%s)', async (allowed) => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValueOnce({ ...order, can_cancel: allowed })
      .mockResolvedValue({ ...order, status: 'CANCELLED' })
    const wrapper = mountOrder()
    await flushPromises()
    expect(!!findButton(wrapper, 'cancelOrder')).toBe(allowed)
    if (!allowed) return
    await findButton(wrapper, 'cancelOrder').trigger('click')
    expect(checkoutAPI.cancel).not.toHaveBeenCalled()
    const confirm = [...document.body.querySelectorAll<HTMLButtonElement>('[role="dialog"] button')]
      .find(button => button.textContent?.trim() === 'payment.checkout.confirmCancel')
    expect(confirm).toBeDefined()
    confirm!.click()
    await flushPromises()
    expect(checkoutAPI.cancel).toHaveBeenCalledWith(number, expect.any(AbortSignal))
    expect(wrapper.text()).toContain('payment.checkout.orderCancelled')
    expect(wrapper.find('input[type="radio"]').exists()).toBe(false)
  })
  it.each([false, true])('shows switching only when authorized by the server (%s)', async (authorized) => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, can_switch_method: authorized })
    const wrapper = mountOrder()
    await flushPromises()
    expect(!!findButton(wrapper, 'switchMethod')).toBe(authorized)
    if (!authorized) return
    const replacement = 'fedcba9876543210fedcba9876543210'
    vi.mocked(checkoutAPI.switchMethod).mockResolvedValue(replacement)
    await findButton(wrapper, 'switchMethod').trigger('click')
    const confirm = [...document.body.querySelectorAll<HTMLButtonElement>('[role="dialog"] button')]
      .find(button => button.textContent?.trim() === 'payment.checkout.switchMethod')
    expect(confirm).toBeDefined()
    confirm!.click()
    await flushPromises()
    expect(checkoutAPI.switchMethod).toHaveBeenCalledWith(number, expect.any(AbortSignal))
    expect(push).toHaveBeenCalledWith({ name: 'PublicCheckoutOrder', params: { orderId: replacement } })
  })

  it.each(['PAID', 'COMPLETED', 'EXPIRED', 'CANCELLED'] as const)('does not offer switching for %s orders', async (status) => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...order, status, can_switch_method: true })
    const wrapper = mountOrder()
    await flushPromises()
    expect(findButton(wrapper, 'switchMethod')).toBeUndefined()
  })
  it.each(['AWAITING_CONFIRMATION', 'PENDING', 'PAID', 'COMPLETED', 'EXPIRED'] as const)(
    'renders the server state %s after reloading the original link', async (status) => {
      vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...order, status, ...(status === 'PENDING' ? {
        payment_type: 'wxpay', pay_url: 'https://provider.example/pay', qr_code: 'provider-code',
      } : {}) })
      const beforeReload = mountOrder()
      await flushPromises()
      beforeReload.unmount()
      const afterReload = mountOrder()
      await flushPromises()
      expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(2)
      expect(checkoutAPI.getOrder).toHaveBeenLastCalledWith(number, expect.any(AbortSignal))
      expect(afterReload.find('input[type="radio"]').exists()).toBe(status === 'AWAITING_CONFIRMATION')
      if (status === 'PENDING') {
        expect(afterReload.get('a').attributes('href')).toBe('https://provider.example/pay')
        expect(afterReload.get('img').attributes('src')).toBe('data:image/png;base64,AA')
      }
      if (status === 'PAID') expect(afterReload.text()).toContain('payment.checkout.fulfillmentPending')
      if (status === 'COMPLETED') expect(afterReload.text()).toContain('payment.checkout.completed')
      if (status === 'EXPIRED') expect(afterReload.text()).toContain('payment.checkout.paymentExpired')
      expect(checkoutAPI.pay).not.toHaveBeenCalled()
    },
  )
  it('reports an unknown order without attempting payment', async () => {
    vi.mocked(checkoutAPI.getOrder).mockRejectedValue(new CheckoutRequestError(404))
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.invalidLink')
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
  })

  it('shows a pending order read-only when its payment action is unavailable', async () => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...order, status: 'PENDING', payment_type: 'wxpay', can_pay: false })
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.processing')
    expect(findButton(wrapper, 'retryPayment')).toBeUndefined()
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
  })

  it('shows subscription currency, quotes a map member, then renders QR and polls the anonymous result', async () => {
    const close = vi.fn()
    const replace = vi.fn()
    vi.spyOn(window, 'open').mockReturnValue({ close, location: { replace } } as unknown as Window)

    vi.mocked(checkoutAPI.getOrder).mockResolvedValue(order)
    vi.mocked(checkoutAPI.quote).mockResolvedValue({ quote_id: 'quote-42', payment_type: 'wxpay', pay_amount: 175, currency: 'CNY', amount: 25, fee_rate: 1 })
    vi.mocked(checkoutAPI.pay).mockResolvedValue({ status: 200, data: { order_id: number, amount: 25, pay_amount: 175, fee_rate: 1, status: 'PENDING', expires_at: order.expires_at, qr_code: 'provider-code' } })
    vi.mocked(checkoutAPI.getOrder).mockResolvedValueOnce(order).mockResolvedValue({ ...result, qr_code: 'provider-code' })
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.text()).toContain('NZD')
    expect(findButton(wrapper, 'pay')).toBeUndefined()
    await wrapper.get('input[type="radio"]').trigger('change')
    await flushPromises()
    expect(wrapper.text()).toContain('CNY')
    expect(checkoutAPI.quote).toHaveBeenCalledWith(number, 'wxpay', expect.any(AbortSignal))
    await findButton(wrapper, 'pay').trigger('click')
    await flushPromises()
    expect(checkoutAPI.pay).toHaveBeenCalledWith(number, 'wxpay', 'quote-42', expect.any(Boolean), expect.any(AbortSignal))
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(2)
    expect(wrapper.get('img').attributes('src')).toBe('data:image/png;base64,AA')
    expect(close).toHaveBeenCalledOnce()
    expect(replace).not.toHaveBeenCalled()
    expect(wrapper.find('a[href^="exampleapp:"]').exists()).toBe(false)
    expect(findButton(wrapper, 'checkStatus')).toBeUndefined()
  })

  it('only replays the locked method after reopening a PENDING checkout', async () => {

    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...order, status: 'PENDING', payment_type: 'wxpay', methods: undefined })
    vi.mocked(checkoutAPI.pay).mockResolvedValue({ status: 202, data: undefined })
    const wrapper = mountOrder()
    await flushPromises()
    expect(checkoutAPI.pay).not.toHaveBeenCalled()
    await findButton(wrapper, 'retryPayment').trigger('click')
    await flushPromises()
    expect(checkoutAPI.pay).toHaveBeenCalledWith(number, 'wxpay', '', expect.any(Boolean), expect.any(AbortSignal))
    expect(wrapper.text()).toContain('payment.checkout.launchPending')
  })

  it('redirects to the HTTPS provider while polling the same order', async () => {

    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...order, status: 'PENDING', payment_type: 'wxpay', methods: undefined })
    vi.mocked(checkoutAPI.pay).mockResolvedValue({ status: 200, data: { order_id: number, amount: 25, pay_amount: 175, fee_rate: 1, status: 'PENDING', expires_at: order.expires_at, pay_url: 'https://provider.example/pay' } })
    vi.mocked(checkoutAPI.getOrder).mockResolvedValueOnce({ ...order, status: 'PENDING', payment_type: 'wxpay', methods: undefined })
      .mockResolvedValue({ ...result, pay_url: 'https://provider.example/pay' })
    const tab = { opener: window, closed: false, close: vi.fn(), location: { replace: vi.fn() } }
    const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window)
    const wrapper = mountOrder()
    await flushPromises()
    await findButton(wrapper, 'retryPayment').trigger('click')
    await flushPromises()
    expect(open).toHaveBeenCalledOnce()
    expect(open).toHaveBeenCalledWith('about:blank', '_blank')
    expect(open.mock.invocationCallOrder[0]).toBeLessThan(vi.mocked(checkoutAPI.pay).mock.invocationCallOrder[0])
    expect(tab.opener).toBeNull()
    expect(tab.location.replace).toHaveBeenCalledOnce()
    expect(tab.location.replace).toHaveBeenCalledWith('https://provider.example/pay')
    expect(tab.close).not.toHaveBeenCalled()
    expect(wrapper.get('a').attributes('href')).toBe('https://provider.example/pay')
    expect(document.body.querySelector('[role="dialog"]')?.textContent).toContain('payment.checkout.payingTitle')
    expect(document.body.querySelector('[role="dialog"] a')).toBeNull()
    expect(wrapper.text()).toContain('payment.checkout.resultTitle')
    expect(wrapper.text()).not.toContain('payment.checkout.viewResult')
    const done = [...document.body.querySelectorAll<HTMLButtonElement>('[role="dialog"] button')]
      .find((button) => button.textContent?.trim() === 'payment.checkout.paymentDone')
    expect(done).toBeDefined()
    const checks = vi.mocked(checkoutAPI.getOrder).mock.calls.length
    done!.click()
    await flushPromises()
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(checks)
    expect(wrapper.getComponent(BaseDialog).props('show')).toBe(false)
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, status: 'PAID' })
    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.fulfillmentPending')
    expect(checkoutAPI.pay).toHaveBeenCalledTimes(1)
  })

  it('treats a pending-order limit as definitive instead of offering a launch replay', async () => {

    vi.mocked(checkoutAPI.getOrder).mockResolvedValue(order)
    vi.mocked(checkoutAPI.quote).mockResolvedValue({ quote_id: 'quote-42', payment_type: 'wxpay', pay_amount: 175, currency: 'CNY', amount: 25, fee_rate: 1 })
    vi.mocked(checkoutAPI.pay).mockRejectedValue(new CheckoutRequestError(429, 'TOO_MANY_PENDING'))
    const wrapper = mountOrder()
    await flushPromises()
    await wrapper.get('input[type="radio"]').trigger('change')
    await flushPromises()
    await findButton(wrapper, 'pay').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.tooManyPending')
    expect(findButton(wrapper, 'retryPayment')).toBeUndefined()
  })
})

describe('checkout result', () => {
  it('continues slow polling and checks immediately after the tab becomes visible', async () => {
    vi.useFakeTimers()
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue(result)
    const wrapper = mountOrder()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(14 * 4000)
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(15)
    await vi.advanceTimersByTimeAsync(30000)
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(16)
    visibility.mockReturnValue('hidden')
    await vi.advanceTimersByTimeAsync(30000)
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(16)
    visibility.mockReturnValue('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    await flushPromises()
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(17)
    expect(wrapper.find('button').exists()).toBe(false)
  })
  it('polls through PAID and RECHARGING and returns once at COMPLETED', async () => {
    vi.useFakeTimers()
    vi.mocked(checkoutAPI.getOrder)
      .mockResolvedValueOnce({ ...result, status: 'PAID' })
      .mockResolvedValueOnce({ ...result, status: 'RECHARGING' })
      .mockResolvedValueOnce({ ...result, status: 'COMPLETED' })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.fulfillmentPending')
    expect(wrapper.find('a').exists()).toBe(false)
    await vi.advanceTimersByTimeAsync(4000)
    expect(click).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(4000)
    expect(wrapper.get('a').attributes('href')).toBe(result.client_return_url)
    expect(click).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(12000)
    expect(checkoutAPI.getOrder).toHaveBeenCalledTimes(3)
    expect(click).toHaveBeenCalledTimes(1)
  })

  it('blocks unsafe URLs even for a COMPLETED projection', async () => {
    vi.mocked(checkoutAPI.getOrder).mockResolvedValue({ ...result, status: 'COMPLETED', client_return_url: 'javascript://unsafe' })
    const wrapper = mountOrder()
    await flushPromises()
    expect(wrapper.text()).toContain('payment.checkout.completed')
    expect(wrapper.find('a').exists()).toBe(false)
  })
})
