<script setup lang="ts">
import { computed, defineAsyncComponent, onScopeDispose, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import QRCode from 'qrcode'
import { captureCheckoutActionToken, checkoutAPI, CheckoutRequestError, type CheckoutOrder, type CheckoutPaymentAction, type CheckoutQuote } from '@/api/checkout'
import { formatPaymentAmount } from '@/components/payment/currency'
import OrderStatusBadge from '@/components/payment/OrderStatusBadge.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { buildAlipayDeepLink } from '@/components/payment/alipayDeepLink'
import { buildApiUrl } from '@/api/url'
import { CHECKOUT_ORDER_ROUTE } from '@/router/checkoutRoutes'
import { isMobileDevice } from '@/utils/device'
import CheckoutShell from './CheckoutShell.vue'
import { autoReturnOnce, checkoutURL, isCheckoutAccessError, isCheckoutProcessing, retryDelay, useCheckoutPolling } from './checkoutFlow'
import { invokeCheckoutWeChat, openAirwallexCheckout } from './checkoutProviderActions'

const CheckoutStripeForm = defineAsyncComponent(() => import('./CheckoutStripeForm.vue').then(module => module.default))

const { t, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const mobile = isMobileDevice()
const order = ref<CheckoutOrder>()
const method = ref('')
const quote = ref<CheckoutQuote>()
const busy = ref(false)
const uncertain = ref(false)
const issue = ref('')
const statusIssue = ref('')
const payURL = ref('')
const qrImage = ref('')
const paymentAction = ref<CheckoutPaymentAction>()
const alipayLink = ref('')
const sdkSubmitted = ref(false)
const wechatResumeToken = ref('')
const airwallexTab = computed(() => route.query?.provider === 'airwallex')
const stripeTab = computed(() => route.query?.provider === 'stripe')
let airwallexLaunched = false
const actionExpiresAt = ref('')
const actionExpired = ref(false)
const showPaymentDialog = ref(false)
const showSwitchDialog = ref(false)
const showCancelDialog = ref(false)
const cancelling = ref(false)
const cancelIssue = ref(false)
const switching = ref(false)
const switchIssue = ref('')
const returnLink = ref<HTMLAnchorElement | null>(null)

let paymentRequest: AbortController | undefined
let retryTimer: ReturnType<typeof setTimeout> | undefined
let actionTimer: ReturnType<typeof setTimeout> | undefined
const retryCoolingDown = ref(false)

const id = computed(() => typeof route.params.orderId === 'string' ? route.params.orderId : '')
const status = computed(() => order.value?.status)
const completed = computed(() => status.value === 'COMPLETED')
const awaitingConfirmation = computed(() => status.value === 'AWAITING_CONFIRMATION')
const methods = computed(() => Object.keys(order.value?.methods ?? {}))
const showStatusPage = computed(() => !!status.value && !awaitingConfirmation.value)
const hasPaymentUI = computed(() => !!(qrImage.value || payURL.value || paymentAction.value))
const checkoutReturnURL = computed(() => `${window.location.origin}/payment/checkout/${encodeURIComponent(id.value)}`)
const airwallexURL = computed(() => checkoutReturnURL.value + '?provider=airwallex')
const stripeURL = computed(() => checkoutReturnURL.value + '?provider=stripe')
const isStripe = computed(() => !!(paymentAction.value?.client_secret && paymentAction.value.publishable_key))
const isAirwallex = computed(() => !!(paymentAction.value?.client_secret && paymentAction.value.intent_id))
const paymentActive = computed(() => !switching.value && order.value?.can_pay !== false && !actionExpired.value && (!status.value || awaitingConfirmation.value || status.value === 'PENDING'))
const hostedURL = computed(() => payURL.value || (isAirwallex.value && !airwallexTab.value ? airwallexURL.value : '') ||
  (isStripe.value && !stripeTab.value ? stripeURL.value : ''))
const paymentDialogOpen = computed(() => showPaymentDialog.value && !!hostedURL.value && paymentActive.value)
const canSwitchMethod = computed(() => !!order.value?.can_switch_method && status.value === 'PENDING' && !actionExpired.value)
const canCancel = computed(() => !!order.value?.can_cancel && awaitingConfirmation.value && !actionExpired.value && !uncertain.value)
const replayMethod = computed(() => status.value === 'PENDING' && !actionExpired.value ? order.value?.payment_type || quote.value?.payment_type : '')
const paymentTotal = computed(() => {
  const { pay_amount: amount, currency } = order.value ?? {}
  return amount == null || !currency ? '' : `${formatPaymentAmount(amount, currency, locale.value)}\u00a0${currency}`
})
// Use only COMPLETED from the authorized projection, never provider or query data.
const returnURL = computed(() => completed.value
  ? checkoutURL(order.value?.client_return_url, true)
  : '')
autoReturnOnce(returnLink)

const launchErrors: Record<string, string> = {
  CHECKOUT_QUOTE_CHANGED: 'quoteChanged',
  CHECKOUT_METHOD_CHANGED: 'quoteChanged',
  CHECKOUT_METHOD_UNAVAILABLE: 'methodUnavailable',
  CHECKOUT_PLAN_CHANGED: 'planChanged',
  NO_AVAILABLE_INSTANCE: 'methodUnavailable',
  CHECKOUT_CLIENT_IP_REQUIRED: 'methodUnavailable',
  INVALID_WECHAT_PAYMENT_RESUME_TOKEN: 'wechatAuthorizationExpired',
  TOO_MANY_PENDING: 'tooManyPending',
}

const { checking, refresh, stop } = useCheckoutPolling(async (signal) => {
  try {
    const resolved = await checkoutAPI.getOrder(id.value, signal)
    if (signal.aborted) return false
    if (resolved.status === 'PENDING' && resolved.can_pay !== false) {
      await setPaymentAction(resolved, signal)
      if (signal.aborted) return false
    } else {
      await setPaymentAction({}, signal)
      if (signal.aborted) return false
    }
    order.value = resolved
    statusIssue.value = ''
    if (airwallexTab.value && !airwallexLaunched && resolved.payment_type === 'airwallex' && resolved.currency && resolved.can_pay !== false && resolved.status === 'PENDING' && resolved.payment_action?.intent_id) {
      airwallexLaunched = true
      void launchAirwallex(resolved.payment_action, resolved.currency)
    }
    if (wechatResumeToken.value && awaitingConfirmation.value && !method.value && methods.value.includes('wxpay')) {
      void selectMethod('wxpay')
    }
    return isCheckoutProcessing(status.value) || uncertain.value && awaitingConfirmation.value
  } catch (error) {
    if (signal.aborted) return false
    if (isCheckoutAccessError(error)) {
      statusIssue.value = error instanceof CheckoutRequestError && error.reason === 'PAYMENT_DISABLED'
        ? 'disabled'
        : 'invalidLink'
      return false
    }
    statusIssue.value = 'statusUnavailable'
    return retryDelay(error)
  }
}, () => !!id.value && !busy.value && (
  statusIssue.value === 'statusUnavailable' || (!statusIssue.value && (awaitingConfirmation.value || isCheckoutProcessing(status.value) || uncertain.value))
))

watch(() => actionExpiresAt.value || order.value?.expires_at, (expiresAt) => {
  clearTimeout(actionTimer)
  actionExpired.value = false
  const expiration = expiresAt ? Date.parse(expiresAt) : NaN
  if (!Number.isFinite(expiration)) return
  const remaining = expiration - Date.now()
  if (remaining <= 0) actionExpired.value = true
  else if (remaining <= 2_147_483_647) actionTimer = setTimeout(() => { actionExpired.value = true }, remaining)
})

function reset() {
  stop()
  paymentRequest?.abort()
  paymentRequest = undefined
  order.value = undefined
  method.value = ''
  quote.value = undefined
  busy.value = false
  uncertain.value = false
  issue.value = ''
  statusIssue.value = ''
  payURL.value = ''
  qrImage.value = ''
  paymentAction.value = undefined
  alipayLink.value = ''
  sdkSubmitted.value = false
  wechatResumeToken.value = ''
  airwallexLaunched = false
  showPaymentDialog.value = false
  showSwitchDialog.value = false
  showCancelDialog.value = false
  cancelling.value = false
  cancelIssue.value = false
  switching.value = false
  switchIssue.value = ''
  clearTimeout(retryTimer)
  clearTimeout(actionTimer)
  retryCoolingDown.value = false
  actionExpiresAt.value = ''
  actionExpired.value = false
}

watch(() => [id.value, route.hash] as const, ([orderId, hash], previous) => {
  if (previous?.[0] !== orderId) reset()
  const fragment = new URLSearchParams(hash?.slice(1))
  const query = { ...route.query }
  // Stripe returns its client secret in the query; the order URL is enough to reload.
  const stripeReturn = ['payment_intent', 'payment_intent_client_secret', 'redirect_status'].some(key => key in query)
  if (stripeReturn) {
    delete query.payment_intent
    delete query.payment_intent_client_secret
    delete query.redirect_status
  }
  if (orderId && (fragment.has('action_token') || fragment.has('wechat_resume_token') || stripeReturn)) {
    if (fragment.has('action_token')) captureCheckoutActionToken(orderId, hash)
    if (fragment.has('wechat_resume_token')) wechatResumeToken.value = fragment.get('wechat_resume_token') || ''
    void router.replace({ path: route.path, query, hash: '' })
  } else if (previous?.[0] === orderId) {
    return // Removing the fragment must not reset a loaded checkout.
  }
  if (orderId) refresh()
  else statusIssue.value = 'invalidLink'
}, { immediate: true })
onScopeDispose(reset)

async function selectMethod(paymentType: string) {
  if (!awaitingConfirmation.value || !paymentActive.value || busy.value || uncertain.value) return
  const controller = new AbortController()
  paymentRequest = controller
  method.value = paymentType
  quote.value = undefined
  issue.value = ''
  busy.value = true
  try {
    const priced = await checkoutAPI.quote(id.value, paymentType, controller.signal)
    if (!controller.signal.aborted) quote.value = priced
  } catch (error) {
    if (controller.signal.aborted) return
    issue.value = 'quoteUnavailable'
    if (isCheckoutAccessError(error)) refresh()
  } finally {
    if (!controller.signal.aborted) busy.value = false
  }
}

async function pay(paymentType: string, quoteId: string) {
  if (busy.value || !id.value || !paymentActive.value || retryCoolingDown.value) return

  // Reserve the tab in the click handler: opening it after fetch can be blocked.
  let providerTab = window.open('about:blank', '_blank')
  if (providerTab) providerTab.opener = null
  const controller = new AbortController()
  paymentRequest = controller
  stop()
  busy.value = true
  issue.value = ''
  try {
    const resumeToken = paymentType === 'wxpay' ? wechatResumeToken.value : ''
    const response = await (resumeToken
      ? checkoutAPI.pay(id.value, paymentType, quoteId, mobile, controller.signal, resumeToken)
      : checkoutAPI.pay(id.value, paymentType, quoteId, mobile, controller.signal))
    if (controller.signal.aborted) return
    const payment = response.data
    if (payment?.expires_at) actionExpiresAt.value = payment.expires_at
    if (response.status === 202 || !payment) {
      markUncertain(response.retryAfter)
      return
    }
    if (payment.oauth?.authorize_url) {
      const oauth = new URL(payment.oauth.authorize_url, window.location.origin)
      if (oauth.origin !== window.location.origin || oauth.pathname !== '/api/v1/auth/oauth/wechat/payment/start') throw new Error()
      window.location.assign(buildApiUrl(oauth.pathname) + oauth.search)
      return
    }
    if (payment.status && payment.status !== 'PENDING') {
      payURL.value = ''
      qrImage.value = ''
      return
    }
    await setPaymentAction(payment, controller.signal)
    if (controller.signal.aborted) return
    if (order.value && payment.status) {
      order.value = { ...order.value, status: payment.status, payment_type: paymentType,
        pay_amount: payment.pay_amount, currency: payment.currency, expires_at: payment.expires_at }
    }
    if (!hasPaymentUI.value) {
      markUncertain()
      return
    }
    uncertain.value = false
    // Keep the checkout alive to reconcile while the provider tab is open.
    // The status page retains the link if the browser blocks the automatic tab.
    if (hostedURL.value) {
      showPaymentDialog.value = true
      if (providerTab && !providerTab.closed) {
        providerTab.location.replace(hostedURL.value)
        providerTab = null
      }
    }
  } catch (error) {
    if (controller.signal.aborted) return
    const reason = error instanceof CheckoutRequestError ? error.reason ?? '' : ''
    const issueKey = launchErrors[reason]
    if (issueKey) {
      if (reason === 'INVALID_WECHAT_PAYMENT_RESUME_TOKEN') wechatResumeToken.value = ''
      quote.value = undefined
      uncertain.value = false
      issue.value = issueKey
    } else if (reason === 'CHECKOUT_NOT_PAYABLE') {
      issue.value = ''
    } else if (reason === 'PAYMENT_DISABLED') {
      issue.value = 'disabled'
      quote.value = undefined
    } else if (!isCheckoutAccessError(error)) {
      // A timeout or operational failure is not evidence the provider call failed.
      // Only the same confirmed method/quote may be retried; never auto-submit again.
      markUncertain(error instanceof CheckoutRequestError ? error.retryAfter : undefined)
    }
  } finally {
    providerTab?.close()
    if (!controller.signal.aborted) {
      busy.value = false
      refresh()
    }
  }
}

async function setPaymentAction(payment: { pay_url?: string; qr_code?: string; payment_action?: CheckoutPaymentAction }, signal: AbortSignal) {
  const image = payment.qr_code && !qrImage.value
    ? await QRCode.toDataURL(payment.qr_code, { width: 224, margin: 3 }).catch(() => '')
    : qrImage.value
  if (signal.aborted) return
  payURL.value = checkoutURL(payment.pay_url)
  qrImage.value = payment.qr_code ? image : ''
  paymentAction.value = payment.payment_action
  alipayLink.value = mobile && payment.payment_action?.alipay_deep_link && payment.qr_code
    ? buildAlipayDeepLink(payment.qr_code) : ''
}

function paymentSubmitted() {
  sdkSubmitted.value = true
  refresh()
}

async function launchAirwallex(action: CheckoutPaymentAction, currency: string) {
  const controller = new AbortController()
  paymentRequest = controller
  try {
    await openAirwallexCheckout(action, currency, checkoutReturnURL.value, locale.value, controller.signal)
  } catch {
    if (!controller.signal.aborted) issue.value = 'providerLaunchFailed'
  }
}

async function openWeChat() {
  if (!paymentAction.value?.jsapi || busy.value) return
  const orderID = id.value
  busy.value = true
  issue.value = ''
  try {
    const result = await invokeCheckoutWeChat(paymentAction.value.jsapi)
    if (id.value !== orderID) return
    if (result.endsWith(':ok')) paymentSubmitted()
    else if (!result.endsWith(':cancel')) issue.value = 'providerLaunchFailed'
  } catch {
    if (id.value === orderID) issue.value = 'providerLaunchFailed'
  } finally {
    if (id.value === orderID) {
      busy.value = false
      refresh()
    }
  }
}

async function switchMethod() {
  if (busy.value || !canSwitchMethod.value) return
  const controller = new AbortController()
  paymentRequest = controller
  stop()
  busy.value = true
  switching.value = true
  switchIssue.value = ''
  showSwitchDialog.value = false
  try {
    const replacement = await checkoutAPI.switchMethod(id.value, controller.signal)
    if (!controller.signal.aborted) await router.push({ name: CHECKOUT_ORDER_ROUTE, params: { orderId: replacement } })
  } catch {
    if (!controller.signal.aborted) switchIssue.value = 'switchMethodFailed'
  } finally {
    if (!controller.signal.aborted) {
      busy.value = false
      switching.value = false
      refresh()
    }
  }
}

async function cancelOrder() {
  if (busy.value || !canCancel.value) return
  const controller = new AbortController()
  paymentRequest = controller
  stop()
  busy.value = true
  cancelling.value = true
  cancelIssue.value = false
  showCancelDialog.value = false
  try {
    await checkoutAPI.cancel(id.value, controller.signal)
    if (!controller.signal.aborted && order.value) {
      order.value = { ...order.value, status: 'CANCELLED', can_cancel: false, can_pay: false }
      quote.value = undefined
    }
  } catch {
    if (!controller.signal.aborted) cancelIssue.value = true
  } finally {
    if (!controller.signal.aborted) {
      busy.value = false
      cancelling.value = false
      refresh()
    }
  }
}

function markUncertain(retryAfter?: number) {
  uncertain.value = true
  if (retryAfter) {
    retryCoolingDown.value = true
    clearTimeout(retryTimer)
    retryTimer = setTimeout(() => { retryCoolingDown.value = false }, Math.min(retryAfter * 1000, 2_147_483_647))
  }
  issue.value = retryAfter ? 'retryLater' : 'launchPending'
}

function resumePending() {
  // Once PENDING, a replay with the locked method returns the existing action;
  // it neither creates a new charge nor authorizes a different method.
  if (replayMethod.value) void pay(replayMethod.value, '')
  else if (quote.value && awaitingConfirmation.value) void pay(quote.value.payment_type, quote.value.quote_id)
}

function methodName(paymentType: string) {
  return order.value?.methods?.[paymentType]?.display_name || t(`payment.methods.${paymentType}`, paymentType)
}
</script>

<template>
  <CheckoutShell :title="t(showStatusPage ? 'payment.checkout.resultTitle' : 'payment.checkout.title')">
    <div role="status" class="space-y-2 pay-muted text-sm">
      <p v-if="statusIssue">{{ t(`payment.checkout.${statusIssue}`) }}</p>
      <p v-else-if="completed">{{ t('payment.checkout.completed') }}</p>
      <p v-else-if="status === 'CANCELLED'">{{ t('payment.checkout.orderCancelled') }}</p>
      <p v-else-if="status === 'PAID' || status === 'RECHARGING'">{{ t('payment.checkout.fulfillmentPending') }}</p>
      <p v-else-if="status === 'EXPIRED' || (actionExpired && (awaitingConfirmation || status === 'PENDING'))">{{ t('payment.checkout.paymentExpired') }}</p>
      <p v-else-if="awaitingConfirmation && !paymentActive">{{ t('payment.checkout.noMethods') }}</p>
      <p v-else-if="isCheckoutProcessing(status) && !issue">{{ t('payment.checkout.processing') }}</p>
      <p v-else-if="!order && checking">{{ t('common.loading') }}</p>
    </div>

    <dl v-if="order" class="pay-inset space-y-3 p-4 text-sm">
      <div class="pay-detail pay-detail--order-id">
        <dt>{{ t('payment.orders.orderId') }}</dt><dd class="pay-order-id">#{{ order.order_id }}</dd>
      </div>
      <div v-if="order.order_type === 'balance'" class="pay-detail">
        <dt>{{ t('payment.checkout.balanceAmount') }}</dt><dd class="pay-number font-medium">{{ formatPaymentAmount(order.amount, 'USD', locale) }}&nbsp;USD</dd>
      </div>
      <div v-else-if="order?.plan_name || order?.plan_id" class="pay-detail">
        <dt>{{ t('payment.tabSubscribe') }}</dt><dd class="font-medium">{{ order.plan_name || (order.plan_id ? `#${order.plan_id}` : '') }}</dd>
      </div>
      <div v-if="order?.order_type === 'subscription' && order.plan_currency && !paymentTotal" class="pay-detail">
        <dt>{{ t('payment.amountLabel') }}</dt><dd class="pay-number font-medium">{{ formatPaymentAmount(order.amount, order.plan_currency, locale) }}&nbsp;{{ order.plan_currency }}</dd>
      </div>
      <div v-if="order?.payment_type" class="pay-detail">
        <dt>{{ t('payment.paymentMethod') }}</dt><dd class="font-medium">{{ methodName(order.payment_type) }}</dd>
      </div>
      <div v-if="paymentTotal" class="pay-amount pay-amount-row px-3 py-2">
        <dt>{{ t('payment.checkout.paymentTotal') }}</dt><dd class="pay-number text-xl font-semibold">{{ paymentTotal }}</dd>
      </div>
      <div v-if="status && status !== 'AWAITING_CONFIRMATION'" class="pay-detail">
        <dt>{{ t('payment.orders.status') }}</dt><dd><OrderStatusBadge :status="status" /></dd>
      </div>
    </dl>

    <p v-if="issue && paymentActive && !statusIssue" role="status" class="text-sm text-amber-700 dark:text-amber-300">
      {{ t(`payment.checkout.${issue}`) }}
    </p>

    <div v-if="hasPaymentUI && paymentActive && !statusIssue" class="space-y-4 text-center">
      <p v-if="qrImage" class="text-sm">{{ t('payment.checkout.scanHint', { method: methodName(order?.payment_type || method) }) }}</p>
      <img v-if="qrImage" :src="qrImage" :alt="t('payment.qr.scanToPay')" width="224" height="224" class="pay-qr mx-auto">
      <p v-if="qrImage && mobile && !payURL && !alipayLink" class="pay-muted text-sm">{{ t('payment.checkout.mobileScanHint') }}</p>
      <a v-if="hostedURL" :href="hostedURL" target="_blank" rel="noreferrer" class="btn pay-button pay-button--primary w-full" @click="showPaymentDialog = true">{{ t('payment.checkout.openProvider') }}</a>
      <a v-if="alipayLink" :href="alipayLink" rel="noreferrer" class="btn pay-button pay-button--primary w-full">{{ t('payment.checkout.openProvider') }}</a>
      <CheckoutStripeForm v-if="isStripe && stripeTab && !sdkSubmitted && paymentAction?.client_secret && paymentAction.publishable_key"
        :key="id" :client-secret="paymentAction.client_secret" :publishable-key="paymentAction.publishable_key"
        :return-url="stripeURL" @submitted="paymentSubmitted" />
      <button v-if="isAirwallex && airwallexTab && order?.currency && paymentAction" type="button" class="btn pay-button pay-button--primary w-full" @click="launchAirwallex(paymentAction, order.currency)">{{ t('payment.checkout.openProvider') }}</button>
      <button v-if="paymentAction?.jsapi && !sdkSubmitted" type="button" class="btn pay-button pay-button--primary w-full" :disabled="busy" @click="openWeChat">{{ t('payment.checkout.openProvider') }}</button>
    </div>

    <template v-else-if="awaitingConfirmation && paymentActive && !uncertain && !statusIssue">
      <fieldset v-if="methods.length" :disabled="busy" class="space-y-2">
        <legend class="mb-2 text-sm font-medium">{{ t('payment.paymentMethod') }}</legend>
        <label v-for="key in methods" :key="key" class="pay-option flex items-center gap-3 p-3 text-sm" :class="{ 'pay-option--selected': method === key }">
          <input type="radio" name="checkout-method" :value="key" :checked="method === key" @change="selectMethod(key)">
          <span class="pay-option-label">{{ methodName(key) }}</span>
        </label>
      </fieldset>
      <p v-else class="text-sm" role="status">{{ t('payment.checkout.noMethods') }}</p>

      <template v-if="quote">
        <div class="pay-amount pay-amount-row p-4">
          <div>
            <p class="text-sm font-medium">{{ t('payment.checkout.paymentTotal') }}</p>
            <p v-if="quote.fee_rate > 0" class="text-xs pay-muted">{{ t('payment.checkout.feeIncluded', { rate: quote.fee_rate }) }}</p>
          </div>
          <strong class="pay-number text-xl font-semibold tracking-tight min-[360px]:text-2xl sm:text-[28px]">{{ formatPaymentAmount(quote.pay_amount, quote.currency, locale) }}&nbsp;{{ quote.currency }}</strong>
        </div>
        <p v-if="order?.order_type === 'balance'" class="pay-muted text-sm">
          {{ t('payment.creditedBalance') }}: {{ formatPaymentAmount(quote.amount, 'USD', locale) }} USD
        </p>
        <button class="btn pay-button pay-button--primary w-full" :disabled="busy" @click="pay(quote.payment_type, quote.quote_id)">
          {{ busy ? t('common.loading') : t('payment.checkout.pay') }}
        </button>
      </template>
      <div v-else-if="method && busy" class="space-y-5" aria-hidden="true">
        <div class="pay-amount pay-amount-row p-4">
          <div>
            <p class="text-sm font-medium">{{ t('payment.checkout.paymentTotal') }}</p>
            <p class="invisible text-xs">&nbsp;</p>
          </div>
          <span class="text-xl font-semibold min-[360px]:text-2xl sm:text-[28px]">—</span>
        </div>
        <div v-if="order?.order_type === 'balance'" class="h-5" />
        <div class="pay-quote-placeholder-button" />
      </div>
      <button v-else-if="method && methods.includes(method) && issue && issue !== 'planChanged'" class="btn pay-button pay-button--secondary w-full" :disabled="busy" @click="selectMethod(method)">
        {{ t('payment.checkout.refreshQuote') }}
      </button>
    </template>

    <button v-if="(replayMethod || (uncertain && awaitingConfirmation && quote)) && paymentActive && !hasPaymentUI && !statusIssue" class="btn pay-button pay-button--secondary w-full" :disabled="busy || retryCoolingDown" @click="resumePending">
      {{ t('payment.checkout.retryPayment') }}
    </button>
    <a v-if="returnURL" ref="returnLink" :href="returnURL" rel="noreferrer" class="btn pay-button pay-button--primary w-full">
      {{ t('payment.checkout.returnToApp') }}
    </a>
    <p v-if="cancelIssue && status !== 'CANCELLED'" role="status" class="text-sm text-amber-700 dark:text-amber-300">
      {{ t('payment.checkout.cancelFailed') }}
    </p>
    <button v-if="canCancel && !statusIssue" type="button" class="btn pay-button pay-button--secondary w-full" :disabled="busy" @click="showCancelDialog = true">
      {{ t(cancelling ? 'common.loading' : 'payment.checkout.cancelOrder') }}
    </button>
    <BaseDialog :show="showCancelDialog && canCancel" :title="t('payment.checkout.cancelOrder')" width="narrow" panel-class="checkout-theme" @close="showCancelDialog = false">
      <p class="text-sm">{{ t('payment.checkout.cancelOrderHint') }}</p>
      <template #footer>
        <button type="button" class="btn pay-button pay-button--secondary" @click="showCancelDialog = false">{{ t('common.back') }}</button>
        <button type="button" class="btn pay-button pay-button--danger" @click="cancelOrder">{{ t('payment.checkout.confirmCancel') }}</button>
      </template>
    </BaseDialog>
    <p v-if="switchIssue" role="status" class="text-sm text-amber-700 dark:text-amber-300">
      {{ t(`payment.checkout.${switchIssue}`) }}
    </p>
    <button v-if="canSwitchMethod && !statusIssue" type="button" class="btn pay-button pay-button--secondary w-full" :disabled="busy" @click="showSwitchDialog = true">
      {{ t(switching ? 'payment.checkout.switchMethodWorking' : 'payment.checkout.switchMethod') }}
    </button>
    <BaseDialog :show="showSwitchDialog && canSwitchMethod" :title="t('payment.checkout.switchMethod')" width="narrow" panel-class="checkout-theme" @close="showSwitchDialog = false">
      <p class="text-sm">{{ t('payment.checkout.switchMethodHint') }}</p>
      <template #footer>
        <button type="button" class="btn pay-button pay-button--secondary" @click="showSwitchDialog = false">{{ t('common.cancel') }}</button>
        <button type="button" class="btn pay-button pay-button--primary" @click="switchMethod">{{ t('payment.checkout.switchMethod') }}</button>
      </template>
    </BaseDialog>
    <BaseDialog :show="paymentDialogOpen" :title="t('payment.checkout.payingTitle')" width="narrow" panel-class="checkout-theme" @close="showPaymentDialog = false">
      <div role="status" class="flex items-center gap-3 pay-muted text-sm">
        <span aria-hidden="true" class="pay-spinner h-5 w-5 shrink-0 rounded-full border-2 motion-safe:animate-spin" />
        <p v-if="statusIssue === 'statusUnavailable'">{{ t('payment.checkout.statusUnavailable') }}</p>
        <p v-else>{{ t('payment.checkout.payingHint') }}</p>
      </div>
      <template #footer>
        <button type="button" class="btn pay-button pay-button--primary" @click="showPaymentDialog = false">
          {{ t('payment.checkout.paymentDone') }}
        </button>
      </template>
    </BaseDialog>
  </CheckoutShell>
</template>
