import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import GroupVideoBillingRules from '../GroupVideoBillingRules.vue'

const getCatalog = vi.hoisted(() => vi.fn())
vi.mock('@/api/admin/groups', () => ({ getVideoPricingCatalog: getCatalog }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const models = [
  { id: 'kling/pro', name: 'Kling Pro', aliases: ['cinema'], resolutions: ['auto'], allow_zero_rate: false,
    recommendation: { unit: 'second', rates: { auto: .112 }, source: 'https://example.com/price', checked_on: '2026-11-25' } },
  { id: 'manual/model', name: 'Manual Model', resolutions: ['720p'], allow_zero_rate: false }
]

function button(wrapper: ReturnType<typeof mount>, suffix: string) {
  return wrapper.findAll('button').find(item => item.text().endsWith(suffix))!
}

describe('GroupVideoBillingRules', () => {
  it('searches aliases and prefills only verified auto rates until saved', async () => {
    getCatalog.mockReset().mockResolvedValue(models)
    const wrapper = mount(GroupVideoBillingRules, { props: { rules: {} } })
    await button(wrapper, '.addModel').trigger('click')
    await flushPromises()
    await wrapper.get('input[type=search]').setValue('cinema')
    expect(wrapper.text()).not.toContain('Manual Model')
    await button(wrapper, 'kling/pro').trigger('click')
    expect(wrapper.text()).toContain('automaticResolution')
    expect(wrapper.get('input[type=number]').element.value).toBe('0.112')
    expect(wrapper.emitted('update')).toBeUndefined()
    await button(wrapper, 'common.save').trigger('click')
    expect(wrapper.emitted('update')).toEqual([[{ 'kling/pro': { unit: 'second', rates: { auto: .112 }, spec_rates: {} } }]])
  })

  it('uses saved rates, clears on unit switch and preserves other models', async () => {
    getCatalog.mockReset().mockResolvedValue(models)
    const manual = { unit: 'generation' as const, rates: { '720p': 2 } }
    const wrapper = mount(GroupVideoBillingRules, { props: { rules: { 'kling/pro': { unit: 'second', rates: { auto: .3 } }, 'manual/model': manual } } })
    await button(wrapper, 'common.edit').trigger('click')
    await flushPromises()
    expect(wrapper.get('input[type=number]').element.value).toBe('0.3')
    await wrapper.get('select').setValue('generation')
    expect(wrapper.get('input[type=number]').element.value).toBe('')
    await wrapper.get('input[type=number]').setValue('1.2')
    await button(wrapper, 'common.save').trigger('click')
    expect(wrapper.emitted('update')).toEqual([[{ 'kling/pro': { unit: 'generation', rates: { auto: 1.2 }, spec_rates: {} }, 'manual/model': manual }]])
  })

  it('requires a manually entered rate and does not save on cancel', async () => {
    getCatalog.mockReset().mockResolvedValue(models)
    const wrapper = mount(GroupVideoBillingRules, { props: { rules: {} } })
    await button(wrapper, '.addModel').trigger('click')
    await flushPromises()
    await button(wrapper, 'manual/model').trigger('click')
    expect(wrapper.get('select').element.value).toBe('')
    await wrapper.get('select').setValue('second')
    await button(wrapper, 'common.save').trigger('click')
    expect(wrapper.get('[role=alert]').text()).toContain('invalidRates')
    await button(wrapper, 'common.cancel').trigger('click')
    expect(wrapper.emitted('update')).toBeUndefined()
  })

  it('keeps legacy spec overrides when editing the rate for their resolution', async () => {
    const specs = [5, 8].map(duration => ({ key: `720p-${duration}s`, resolution: '720p', duration }))
    getCatalog.mockReset().mockResolvedValue([{ ...models[1], specs }])
    const spec_rates = { '720p-5s': .2, '720p-8s': .4 }
    const wrapper = mount(GroupVideoBillingRules, { props: { rules: { 'manual/model': { unit: 'generation', spec_rates } } } })
    await button(wrapper, 'common.edit').trigger('click')
    await flushPromises()
    await wrapper.get('label input[type=number]').setValue('1.5')
    await button(wrapper, 'common.save').trigger('click')
    expect(wrapper.emitted('update')).toEqual([[{ 'manual/model': { unit: 'generation', rates: { '720p': 1.5 }, spec_rates } }]])
  })
})
