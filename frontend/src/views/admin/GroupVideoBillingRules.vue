<template>
  <section class="mt-4 space-y-3 border-t border-dashed border-gray-200 pt-4 dark:border-dark-700">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h4 class="text-sm font-medium">{{ t('admin.groups.videoPricing.modelOverridesTitle') }}</h4>
      <button type="button" class="btn btn-secondary btn-sm" @click="openPicker">{{ t('admin.groups.videoPricing.addModel') }}</button>
    </div>
    <p class="text-xs text-gray-500">{{ t('admin.groups.videoPricing.modelOverridesDescription') }}</p>
    <p v-if="conflicts?.length" class="text-xs text-amber-700 dark:text-amber-300" role="status">
      {{ t('admin.groups.videoPricing.legacyConflict') }} {{ conflicts.join(', ') }}
    </p>
    <ul v-if="Object.keys(rules).length" class="divide-y divide-gray-200 rounded-lg border border-gray-200 px-3 dark:divide-dark-700 dark:border-dark-700">
      <li v-for="(rule, id) in rules" :key="id" class="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
        <div class="min-w-0 break-words">
          <p class="font-medium">{{ byID.get(id)?.name ?? id }}</p>
          <p class="text-xs text-gray-500">{{ summary(rule) }}</p>
        </div>
        <div class="flex gap-2">
          <button type="button" class="btn btn-secondary btn-sm" @click="edit(id)">{{ t('common.edit') }}</button>
          <button type="button" class="btn btn-secondary btn-sm" @click="remove(id)">{{ t('common.delete') }}</button>
        </div>
      </li>
    </ul>
    <p v-else class="text-xs text-gray-500">{{ t('admin.groups.videoPricing.notConfigured') }}</p>

    <div v-if="picker || draft" class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
      <p v-if="loading" class="text-sm" role="status">{{ t('common.loading') }}</p>
      <div v-if="failed" class="flex flex-wrap items-center gap-2 text-sm" role="alert">
        {{ t('admin.groups.videoPricing.catalogFailed') }}
        <button type="button" class="btn btn-secondary btn-sm" @click="load">{{ t('admin.groups.videoPricing.retryCatalog') }}</button>
      </div>
      <template v-if="picker && loaded">
        <input v-model="search" type="search" class="input" :placeholder="t('admin.groups.videoPricing.searchModel')" :aria-label="t('admin.groups.videoPricing.searchModel')" />
        <ul class="max-h-56 space-y-1 overflow-auto">
          <li v-for="model in matches" :key="model.id">
            <button type="button" class="w-full break-words rounded px-2 py-1 text-left text-sm hover:bg-gray-100 dark:hover:bg-dark-700" @click="start(model.id)">
              {{ model.name }} <span v-if="model.id !== model.name" class="text-xs text-gray-500">{{ model.id }}</span>
              <span v-if="rules[model.id]" class="ml-1 text-xs">{{ t('admin.groups.videoPricing.configured') }}</span>
            </button>
          </li>
        </ul>
        <p v-if="!matches.length" class="text-sm text-gray-500">{{ t('admin.groups.videoPricing.noMatchingModels') }}</p>
      </template>
      <template v-if="draft">
        <p class="break-words text-sm font-medium">{{ entry?.name ?? draft.id }}</p>
        <template v-if="entry">
          <p v-if="recommendation" class="text-xs text-gray-500">
            {{ t(recommendation.estimated ? 'admin.groups.videoPricing.estimatedCost' : 'admin.groups.videoPricing.upstreamCost') }}
            <a :href="recommendation.source" target="_blank" rel="noopener noreferrer" class="underline">{{ t('admin.groups.videoPricing.priceSource') }}</a>
            · {{ recommendation.checked_on }}<template v-if="recommendation.conditions"> · {{ conditionLabel(recommendation.conditions) }}</template>
          </p>
          <p v-else-if="!rules[draft.id]" class="text-xs text-gray-500">{{ t('admin.groups.videoPricing.manualRates') }}</p>
          <label class="block text-sm">
            {{ t('admin.groups.videoPricing.billingUnit') }}
            <select class="input mt-1" :value="draft.unit" @change="changeUnit">
              <option value="" disabled>{{ t('admin.groups.videoPricing.selectUnit') }}</option>
              <option value="generation">{{ t('admin.groups.videoPricing.byGeneration') }}</option>
              <option value="second">{{ t('admin.groups.videoPricing.byDuration') }}</option>
            </select>
          </label>
          <template v-if="draft.unit">
            <p v-if="changedUnit" class="text-xs text-gray-500">{{ t('admin.groups.videoPricing.unitChanged') }}</p>
            <div class="grid grid-cols-2 gap-2 sm:grid-cols-3">
              <label v-for="tier in entry.resolutions" :key="tier" class="text-xs">
                {{ tierLabel(tier) }} · {{ unitLabel(draft.unit) }}
                <input v-model="draft.rates[tier]" type="number" min="0" step="any" class="input mt-1" />
              </label>
            </div>
            <details v-if="entry.specs?.length" :open="Object.values(draft.specRates).some(value => value !== '')">
              <summary class="cursor-pointer text-xs">{{ t('admin.groups.videoPricing.specOverrides') }}</summary>
              <div class="mt-2 grid max-h-60 grid-cols-2 gap-2 overflow-auto sm:grid-cols-3">
                <label v-for="spec in entry.specs" :key="spec.key" class="text-xs">
                  {{ tierLabel(spec.resolution) }} · {{ spec.duration }}s<template v-if="spec.aspect_ratio"> · {{ spec.aspect_ratio }}</template> · {{ unitLabel(draft.unit) }}
                  <input v-model="draft.specRates[spec.key]" type="number" min="0" step="any" class="input mt-1" />
                </label>
              </div>
            </details>
            <div v-if="retiredKeys.length" class="space-y-1 text-xs text-gray-500">
              <p>{{ t('admin.groups.videoPricing.historicalTiers') }}</p>
              <div v-for="{ key, kind } in retiredKeys" :key="`${kind}:${key}`" class="flex items-center justify-between gap-2">
                <span>{{ key }} · {{ draft[kind][key] }} {{ unitLabel(draft.unit) }}</span>
                <button type="button" class="btn btn-secondary btn-sm" @click="delete draft[kind][key]">{{ t('common.delete') }}</button>
              </div>
            </div>
          </template>
          <p v-if="invalid" class="text-xs text-red-600" role="alert">{{ t('admin.groups.videoPricing.invalidRates') }}</p>
          <button type="button" class="btn btn-primary btn-sm mr-2" @click="apply">{{ t('common.save') }}</button>
        </template>
        <p v-else-if="loaded" class="text-xs text-gray-500">{{ t('admin.groups.videoPricing.historicalModel') }}</p>
      </template>
      <button type="button" class="btn btn-secondary btn-sm" @click="close">{{ t('common.cancel') }}</button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getVideoPricingCatalog, type VideoBillingModel } from '@/api/admin/groups'
import type { VideoBillingUnit, VideoModelBillingRule, VideoModelBillingRules } from '@/types'

type DraftRates = Record<string, string | number>
const props = defineProps<{ rules: VideoModelBillingRules; conflicts?: string[] }>()
const emit = defineEmits<{ update: [rules: VideoModelBillingRules] }>()
const { t } = useI18n()
const catalog = ref<VideoBillingModel[]>([])
const byID = computed(() => new Map(catalog.value.map(model => [model.id, model])))
const loading = ref(false)
const failed = ref(false)
const loaded = ref(false)
const picker = ref(false)
const search = ref('')
const draft = ref<{ id: string; unit: VideoBillingUnit | ''; rates: DraftRates; specRates: DraftRates }>()
const entry = computed(() => draft.value && byID.value.get(draft.value.id))
const retiredKeys = computed(() => {
  const row = draft.value
  const model = entry.value
  if (!row || !model) return []
  return [
    ...Object.keys(row.rates).filter(key => !model.resolutions.includes(key)).map(key => ({ key, kind: 'rates' as const })),
    ...Object.keys(row.specRates).filter(key => !model.specs?.some(spec => spec.key === key)).map(key => ({ key, kind: 'specRates' as const })),
  ]
})
const recommendation = computed(() => draft.value && !props.rules[draft.value.id] && !changedUnit.value ? entry.value?.recommendation : undefined)
const changedUnit = ref(false)
const invalid = ref(false)
const matches = computed(() => {
  const terms = search.value.toLowerCase().trim().split(/\s+/)
  return catalog.value.filter(model => terms.every(term => [model.name, model.id, ...(model.aliases ?? [])].join(' ').toLowerCase().includes(term)))
})

async function load() {
  if (loaded.value || loading.value) return
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

function tierLabel(key: string) { return key === 'auto' ? t('admin.groups.videoPricing.automaticResolution') : key }
function unitLabel(unit: VideoBillingUnit) { return t(unit === 'second' ? 'admin.groups.videoPricing.perSecond' : 'admin.groups.videoPricing.perGeneration') }
function conditionLabel(condition: string) {
  if (condition === 'audio_off') return t('admin.groups.videoPricing.audioOff')
  if (condition === 'single_clip_audio_off') return t('admin.groups.videoPricing.singleClipAudioOff')
  if (condition === 'xai_output_only' || condition === 'output_only') return t('admin.groups.videoPricing.outputOnly')
  return condition
}
function summary(rule: VideoModelBillingRule) {
  const tiers = Object.entries(rule.rates ?? {}).map(([tier, price]) => `${tierLabel(tier)} ${price}`)
  const count = Object.keys(rule.spec_rates ?? {}).length
  if (count) tiers.push(`${t('admin.groups.videoPricing.specOverrides')} (${count})`)
  return [unitLabel(rule.unit), ...tiers].join(' · ')
}
function openPicker() { close(); picker.value = true; search.value = ''; void load() }
function edit(id: string) { start(id); void load() }
function start(id: string) {
  const rule = props.rules[id] ?? byID.value.get(id)?.recommendation
  picker.value = false
  changedUnit.value = false
  invalid.value = false
  draft.value = { id, unit: rule?.unit ?? '', rates: { ...rule?.rates }, specRates: { ...rule?.spec_rates } }
}
function close() { picker.value = false; draft.value = undefined }
function changeUnit(event: Event) {
  if (!draft.value) return
  // A rate in dollars per second is not a rate in dollars per generation.
  if (draft.value.unit) {
    changedUnit.value = true
    draft.value.rates = {}
    draft.value.specRates = {}
  }
  draft.value.unit = (event.target as HTMLSelectElement).value as VideoBillingUnit
}
function parseRates(values: DraftRates, allowZero: boolean) {
  const entries = Object.entries(values).filter(([, value]) => value !== '').map(([key, value]) => [key, Number(value)] as const)
  if (entries.some(([, value]) => !Number.isFinite(value) || value < 0 || (!allowZero && value === 0))) return undefined
  return Object.fromEntries(entries)
}
function apply() {
  const model = entry.value
  const row = draft.value
  if (!row || !model) return
  const rates = parseRates(row.rates, model.allow_zero_rate)
  const specRates = parseRates(row.specRates, model.allow_zero_rate)
  if (!row.unit || !rates || !specRates || !Object.keys({ ...rates, ...specRates }).length) { invalid.value = true; return }
  emit('update', { ...props.rules, [row.id]: { unit: row.unit, rates, spec_rates: specRates } })
  close()
}
function remove(id: string) {
  const updated = { ...props.rules }
  delete updated[id]
  emit('update', updated)
  if (draft.value?.id === id) close()
}
</script>
