<script setup lang="ts">
import { onMounted, onScopeDispose, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Stripe, StripeElements, StripePaymentElement } from '@stripe/stripe-js'

const props = defineProps<{ clientSecret: string; publishableKey: string; returnUrl: string }>()
const emit = defineEmits<{ submitted: [] }>()
const { t } = useI18n()
const target = ref<HTMLElement>()
const ready = ref(false)
const busy = ref(false)
const issue = ref('')
let stripe: Stripe | null = null
let elements: StripeElements | undefined
let field: StripePaymentElement | undefined
let disposed = false
let method = ''

onScopeDispose(() => {
  disposed = true
  field?.destroy()
})

onMounted(async () => {
  try {
    const { loadStripe } = await import('@stripe/stripe-js/pure')
    stripe = await loadStripe(props.publishableKey)
    if (disposed) return
    if (!stripe || !target.value) throw new Error()
    const dark = document.documentElement.classList.contains('dark')
    const accent = dark ? '#84b4ff' : '#0057ff'
    elements = stripe.elements({
      clientSecret: props.clientSecret,
      appearance: {
        theme: dark ? 'night' : 'stripe',
        variables: {
          colorPrimary: accent,
          colorBackground: dark ? '#1c2534' : '#ffffff',
          colorText: dark ? '#f5f7fb' : '#171717',
          colorDanger: dark ? '#fca5a5' : '#b91c1c',
          borderRadius: '12px',
          focusBoxShadow: `0 0 0 2px ${accent}`,
        },
      },
    })
    field = elements.create('payment', { layout: 'tabs' })
    field.on('ready', () => { ready.value = true })
    field.on('change', event => { method = event.value.type })
    field.on('loaderror', () => { issue.value = t('payment.stripeLoadFailed') })
    field.mount(target.value)
  } catch {
    issue.value = t('payment.stripeLoadFailed')
  }
})

async function confirm() {
  if (!stripe || !elements || !ready.value || busy.value) return
  busy.value = true
  issue.value = ''
  try {
    // Stripe's WeChat and Alipay helpers provide their specialized QR/redirect UI.
    const result = method === 'wechat_pay'
      ? await stripe.confirmWechatPayPayment(props.clientSecret, { payment_method_options: { wechat_pay: { client: 'web' } } })
      : method === 'alipay'
        ? await stripe.confirmAlipayPayment(props.clientSecret, { return_url: props.returnUrl })
        : await stripe.confirmPayment({ elements, confirmParams: { return_url: props.returnUrl }, redirect: 'if_required' })
    if (result.error) issue.value = result.error.message || t('payment.result.failed')
    else if (result.paymentIntent?.status === 'succeeded' || result.paymentIntent?.status === 'processing') emit('submitted')
  } catch {
    issue.value = t('payment.result.failed')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <form class="space-y-3 text-left" @submit.prevent="confirm">
    <div ref="target" />
    <p v-if="issue" role="alert" class="text-sm text-red-700 dark:text-red-300">{{ issue }}</p>
    <p v-else-if="!ready" role="status" class="text-sm">{{ t('common.loading') }}</p>
    <button type="submit" class="btn pay-button pay-button--primary w-full" :disabled="!ready || busy">{{ t('payment.stripePay') }}</button>
  </form>
</template>
