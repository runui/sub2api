<template>
  <fieldset class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
    <legend class="px-1 text-sm font-semibold">{{ t('admin.channels.falVideo.title') }}</legend>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.channels.falVideo.hint') }}</p>
    <div v-if="routes?.length" class="space-y-2">
      <button type="button" class="btn btn-secondary btn-sm" :disabled="pricingLoading" @click="loadPublicPricing">
        {{ t(pricingLoading ? 'admin.channels.falVideo.pricingLoading' : 'admin.channels.falVideo.loadPricing') }}
      </button>
      <p v-if="publicPricing" class="text-xs text-gray-500 dark:text-gray-400" role="status">
        {{ t('admin.channels.falVideo.publicRates', { rates: resolutions.map(r => `${r}: $${publicPricing?.[r]}`).join(' / ') }) }}
      </p>
      <p v-if="pricingFailed" class="text-xs text-red-600" role="alert">{{ t('admin.channels.falVideo.pricingFailed') }}</p>
    </div>
    <div v-for="(route, index) in routes" :key="index" class="space-y-3 rounded-md bg-gray-50 p-3 dark:bg-dark-800">
      <div class="grid gap-3 sm:grid-cols-2">
        <label>
          <span class="input-label">{{ t('admin.channels.falVideo.group') }}</span>
          <select class="input" :value="route.group_id" @change="update(index, { group_id: numberValue($event) })">
            <option :value="0" disabled>{{ t('admin.channels.falVideo.selectGroup') }}</option>
            <option v-if="route.group_id && !groups.some(group => group.id === route.group_id)" :value="route.group_id">#{{ route.group_id }}</option>
            <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }} (#{{ group.id }})</option>
          </select>
        </label>
        <label>
          <span class="input-label">{{ t('admin.channels.falVideo.account') }}</span>
          <select class="input" :value="route.account_id" @change="update(index, { account_id: numberValue($event) })">
            <option :value="0" disabled>{{ t('admin.channels.falVideo.selectAccount') }}</option>
            <option v-if="route.account_id && !accounts.some(account => account.id === route.account_id)" :value="route.account_id">#{{ route.account_id }}</option>
            <option v-for="account in accounts" :key="account.id" :value="account.id">{{ account.name }} (#{{ account.id }})</option>
          </select>
        </label>
      </div>
      <label class="block">
        <span class="input-label">{{ t('admin.channels.falVideo.model') }}</span>
        <input class="input" :value="route.model" maxlength="128" @input="update(index, { model: ($event.target as HTMLInputElement).value })" />
      </label>
      <div class="grid gap-3 sm:grid-cols-3">
        <label v-for="resolution in resolutions" :key="resolution">
          <span class="input-label">{{ resolution }} (USD/s){{ resolution === '720p' ? ' *' : '' }}</span>
          <input type="number" min="0.00000001" max="1000" step="any" class="input" :value="route.price_usd_per_second[resolution]" @input="setPrice(index, resolution, $event)" />
        </label>
      </div>
      <div class="flex flex-wrap gap-3">
        <button v-if="publicPricing" type="button" class="btn btn-secondary btn-sm" @click="update(index, { price_usd_per_second: { ...publicPricing } })">{{ t('admin.channels.falVideo.applyPricing') }}</button>
        <button type="button" class="text-sm text-red-600 dark:text-red-400" @click="routes = routes?.filter((_, i) => i !== index)">{{ t('admin.channels.falVideo.remove') }}</button>
      </div>
    </div>
    <p v-if="loading" class="text-xs text-gray-500" role="status">{{ t('admin.channels.falVideo.loading') }}</p>
    <p v-if="loadFailed" class="text-xs text-red-600" role="alert">
      {{ t('admin.channels.falVideo.loadError') }}
      <button type="button" class="underline" @click="loadAccounts">{{ t('admin.channels.falVideo.retry') }}</button>
    </p>
    <div class="flex flex-wrap gap-2">
      <button type="button" class="btn btn-secondary btn-sm" @click="addRoute">{{ t('admin.channels.falVideo.add') }}</button>
      <button v-if="routes === undefined" type="button" class="btn btn-secondary btn-sm" @click="routes = []">{{ t('admin.channels.falVideo.disableLegacy') }}</button>
      <span v-else-if="routes.length === 0" class="self-center text-xs text-gray-500">{{ t('admin.channels.falVideo.disabled') }}</span>
    </div>
  </fieldset>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { FalVideoChannelRoute } from '@/api/admin/channels'

const { t } = useI18n()
const props = defineProps<{ groups: { id: number; name: string }[] }>()
const routes = defineModel<FalVideoChannelRoute[] | undefined>()
const resolutions = ['480p', '720p', '1080p'] as const
const accounts = ref<{ id: number; name: string }[]>([])
const loading = ref(false)
const loadFailed = ref(false)
const publicPricing = ref<Required<FalVideoChannelRoute['price_usd_per_second']>>()
const pricingLoading = ref(false)
const pricingFailed = ref(false)
let loaded = false

async function loadPublicPricing() {
  if (pricingLoading.value) return
  pricingLoading.value = true
  pricingFailed.value = false
  publicPricing.value = undefined
  try {
    publicPricing.value = await adminAPI.channels.getFalVideoPublicPricing()
  } catch {
    pricingFailed.value = true
  } finally {
    pricingLoading.value = false
  }
}

async function loadAccounts() {
  if (loaded || loading.value) return
  loading.value = true
  loadFailed.value = false
  try {
    const options: typeof accounts.value = []
    for (let page = 1; ; page++) {
      const result = await adminAPI.accounts.list(page, 100, { platform: 'fal', type: 'apikey', status: 'active', lite: 'true' })
      options.push(...result.items.map(({ id, name }) => ({ id, name })))
      if (page >= result.pages || result.items.length === 0) break
    }
    accounts.value = options
    loaded = true
  } catch {
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

watch(() => Boolean(routes.value?.length), hasRoutes => { if (hasRoutes) void loadAccounts() }, { immediate: true })

function numberValue(event: Event) {
  return Number((event.target as HTMLSelectElement).value)
}

function addRoute() {
  routes.value = [...(routes.value ?? []), {
    group_id: props.groups.length === 1 ? props.groups[0].id : 0,
    account_id: accounts.value.length === 1 ? accounts.value[0].id : 0,
    model: 'grok-imagine-video-1.5',
    price_usd_per_second: {},
  }]
}

function update(index: number, patch: Partial<FalVideoChannelRoute>) {
  routes.value = routes.value?.map((route, i) => i === index ? { ...route, ...patch } : route)
}

function setPrice(index: number, resolution: typeof resolutions[number], event: Event) {
  const input = event.target as HTMLInputElement
  const price = { ...routes.value?.[index]?.price_usd_per_second }
  if (input.value === '') delete price[resolution]
  else price[resolution] = input.valueAsNumber
  update(index, { price_usd_per_second: price })
}
</script>
