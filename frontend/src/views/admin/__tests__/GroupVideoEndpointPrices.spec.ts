import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import GroupVideoEndpointPrices from '../GroupVideoEndpointPrices.vue'

const getCatalog = vi.hoisted(() => vi.fn())
vi.mock('@/api/admin/groups', () => ({ getVideoPricingCatalog: getCatalog }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('GroupVideoEndpointPrices', () => {
  it('loads on demand and edits each pricing unit separately', async () => {
    getCatalog.mockReset().mockResolvedValue([
      { endpoint: 'kling/model', unit: 'second', options: [{ key: 'auto' }] },
      { endpoint: 'ltx/model', unit: 'generation', options: [{ key: '720p-5s-16x9', resolution: '720p', duration: 5, aspect_ratio: '16:9' }] }
    ])
    const wrapper = mount(GroupVideoEndpointPrices, {
      props: { seconds: { 'kling/model': { auto: 0.1 } }, specs: { 'ltx/model': { '720p-5s-16x9': 1 } } }
    })
    expect(getCatalog).not.toHaveBeenCalled()
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()

    await wrapper.get('select').setValue('kling/model')
    expect(wrapper.text()).toContain('automaticResolution')
    expect(wrapper.get('input').element.value).toBe('0.1')
    await wrapper.get('input').setValue('0.2')
    await wrapper.get('select').setValue('ltx/model')
    expect(wrapper.text()).toContain('perGeneration')
    expect(wrapper.get('input').element.value).toBe('1')
    await wrapper.get('input').setValue('')
    expect(wrapper.emitted('price')).toEqual([
      ['second', 'kling/model', 'auto', '0.2'],
      ['generation', 'ltx/model', '720p-5s-16x9', '']
    ])
    wrapper.unmount()
  })

  it('allows retrying a failed catalog request', async () => {
    getCatalog.mockReset().mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce([
      { endpoint: 'wan/model', unit: 'second', options: [{ key: '720p', resolution: '720p' }] }
    ])
    const wrapper = mount(GroupVideoEndpointPrices, { props: { seconds: {}, specs: {} } })
    wrapper.get('details').element.open = true
    await wrapper.get('details').trigger('toggle')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('catalogFailed')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('select').text()).toContain('wan/model')
    wrapper.unmount()
  })
})
