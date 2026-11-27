<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import './checkout-theme.css'

defineProps<{ title: string }>()
const { t } = useI18n()
const app = useAppStore()
</script>

<template>
  <main class="checkout-theme checkout-theme--background min-h-screen px-4 py-10 sm:py-20">
    <section aria-labelledby="checkout-title" class="pay-card mx-auto max-w-md">
      <header class="pay-rule border-b px-6 py-5 sm:px-8">
        <p class="pay-accent text-sm font-medium">{{ app.siteName }} &middot; {{ t('payment.checkout.label') }}</p>
        <h1 id="checkout-title" class="mt-2 text-2xl font-semibold tracking-tight">{{ title }}</h1>
      </header>
      <div class="space-y-5 p-6 sm:p-8">
        <slot />
      </div>
    </section>
  </main>
</template>
