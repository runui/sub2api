<template>
  <details class="mt-4 rounded-lg border border-gray-200 p-3 dark:border-dark-700" @toggle="onToggle">
    <summary class="cursor-pointer text-sm font-medium">{{ t('admin.groups.videoPricing.endpointPricingTitle') }}</summary>
    <p v-if="loading" class="mt-2 text-sm">{{ t('common.loading') }}</p>
    <div v-else-if="failed" class="mt-2 flex items-center gap-3 text-sm" role="alert">
      {{ t('admin.groups.videoPricing.catalogFailed') }}
      <button type="button" class="btn btn-secondary btn-sm" @click="load">{{ t('admin.groups.videoPricing.retryCatalog') }}</button>
    </div>
    <template v-else-if="catalog.length">
      <select v-model="selected" class="input mt-3" :aria-label="t('admin.groups.videoPricing.endpointId')">
        <option value="">{{ t('admin.groups.videoPricing.selectEndpoint') }}</option>
        <option v-for="entry in catalog" :key="entry.endpoint" :value="entry.endpoint">{{ entry.endpoint }}</option>
      </select>
      <div v-if="entry" class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3">
        <label v-for="option in entry.options" :key="option.key" class="text-xs">
          <span class="mb-1 block text-gray-600 dark:text-gray-300">
            {{ option.resolution || t('admin.groups.videoPricing.automaticResolution') }}
            <template v-if="option.duration"> · {{ option.duration }}s</template>
            <template v-if="option.aspect_ratio"> · {{ option.aspect_ratio }}</template>
            · {{ t(entry.unit === 'second' ? 'admin.groups.videoPricing.perSecond' : 'admin.groups.videoPricing.perGeneration') }}
          </span>
          <input class="input" type="number" min="0" step="any" :value="price(option.key) ?? ''" @input="setPrice(option.key, $event)" />
        </label>
      </div>
    </template>
  </details>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getVideoPricingCatalog, type VideoEndpointPricing } from '@/api/admin/groups'
import type { VideoModelPricesForm } from './groupsVideoModelPricing'

const props = defineProps<{ seconds: VideoModelPricesForm; specs: VideoModelPricesForm }>()
const emit = defineEmits<{ price: [unit: VideoEndpointPricing['unit'], endpoint: string, key: string, value: string] }>()
const { t } = useI18n()
const catalog = ref<VideoEndpointPricing[]>([])
const selected = ref('')
const loading = ref(false)
const loaded = ref(false)
const failed = ref(false)
const entry = computed(() => catalog.value.find(row => row.endpoint === selected.value))

function onToggle(event: Event) {
  if ((event.currentTarget as HTMLDetailsElement).open && !loaded.value && !failed.value) void load()
}

async function load() {
  if (loading.value) return
  loading.value = true
  failed.value = false
  try {
    catalog.value = await getVideoPricingCatalog()
    loaded.value = true
  } catch {
    failed.value = true
  } finally {
    loading.value = false
  }
}

function price(key: string) {
  const row = entry.value
  if (!row) return undefined
  return (row.unit === 'second' ? props.seconds : props.specs)[row.endpoint]?.[key]
}

function setPrice(key: string, event: Event) {
  const row = entry.value
  if (row) emit('price', row.unit, row.endpoint, key, (event.target as HTMLInputElement).value)
}
</script>
