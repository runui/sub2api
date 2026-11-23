<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'

defineProps<{ title: string }>()
const { t } = useI18n()
const app = useAppStore()
</script>

<template>
  <main class="min-h-screen bg-gradient-to-b from-primary-50 via-gray-50 to-gray-50 px-4 py-10 dark:from-primary-950 dark:via-dark-900 dark:to-dark-900 sm:py-20">
    <section aria-labelledby="checkout-title" class="mx-auto max-w-md overflow-hidden rounded-2xl border border-gray-200 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-800">
      <header class="border-b border-gray-100 px-6 py-5 dark:border-dark-700 sm:px-8">
        <p class="text-sm font-medium text-primary-700 dark:text-primary-300">{{ app.siteName }} &middot; {{ t('payment.checkout.label') }}</p>
        <h1 id="checkout-title" class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ title }}</h1>
      </header>
      <div class="space-y-5 p-6 dark:text-gray-100 sm:p-8">
        <slot />
      </div>
    </section>
  </main>
</template>
