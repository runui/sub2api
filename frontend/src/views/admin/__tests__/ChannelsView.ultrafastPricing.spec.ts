import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Channel, CreateChannelRequest, UpdateChannelRequest } from '@/api/admin/channels'
import ChannelsView from '@/views/admin/ChannelsView.vue'
import PricingEntryCard from '@/components/admin/channel/PricingEntryCard.vue'

const { list, create, update, getGroups, getAccounts, getFalVideoPublicPricing, showError } = vi.hoisted(() => ({
  list: vi.fn(), create: vi.fn(), update: vi.fn(), getGroups: vi.fn(), getAccounts: vi.fn(), getFalVideoPublicPricing: vi.fn(), showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    channels: { list, create, update, getFalVideoPublicPricing },
    groups: { getAll: getGroups },
    accounts: { list: getAccounts },
    settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false }) }
  }
}))
vi.mock('@/api/admin/channels', () => ({
  default: { getModelDefaultPricing: vi.fn().mockResolvedValue({ found: false }) }
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

function mountView() {
  return mount(ChannelsView, {
    global: {
      stubs: {
        AppLayout: defineComponent({ template: '<main><slot /></main>' }),
        TablePageLayout: defineComponent({
          template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>'
        }),
        BaseDialog: defineComponent({
          props: ['show'], template: '<section v-if="show"><slot /><slot name="footer" /></section>'
        }),
        DataTable: defineComponent({
          props: ['data'],
          template: '<div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div>'
        }),
        Pagination: true, ConfirmDialog: true, EmptyState: true,
        Select: true, Icon: true, PlatformIcon: true
      }
    }
  })
}

type View = ReturnType<typeof mountView>

async function clickButton(wrapper: View, text: string) {
  const button = wrapper.findAll('button').find(button => button.text() === text)
  expect(button).toBeDefined()
  await button!.trigger('click')
  await flushPromises()
}

function multiplierInput(wrapper: View) {
  return wrapper.findAll('label')
    .find(label => label.text() === 'admin.channels.form.ultrafastMultiplier')!
    .element.parentElement!.querySelector('input')!
}

function makeChannel(): Channel {
  return {
    id: 1, name: 'Ultra Fast channel', description: '', status: 'active',
    billing_model_source: 'channel_mapped', restrict_models: false,
    features_config: {}, group_ids: [22], model_pricing: [], model_mapping: {},
    apply_pricing_to_account_stats: false, account_stats_pricing_rules: [],
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z'
  }
}

describe('ChannelsView Ultra Fast pricing', () => {
  let stored: Channel[]

  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
    stored = []
    list.mockImplementation(async () => ({ items: stored, total: stored.length }))
    getGroups.mockResolvedValue([{ id: 22, name: 'OpenAI group', platform: 'openai', rate_multiplier: 1 }])
    create.mockImplementation(async (request: CreateChannelRequest) => {
      stored = [{ ...makeChannel(), ...request } as Channel]
      return stored[0]
    })
    update.mockImplementation(async (_id: number, request: UpdateChannelRequest) => {
      stored = [{ ...stored[0], ...request } as Channel]
      return stored[0]
    })
  })

  async function createPricingForm() {
    const wrapper = mountView()
    await flushPromises()
    await clickButton(wrapper, 'admin.channels.createChannel')
    await wrapper.get('input[required]').setValue('Ultra Fast channel')
    await wrapper.findAll('label').find(label => label.text() === 'admin.groups.platforms.openai')!
      .get('input[type="checkbox"]').setValue(true)
    await clickButton(wrapper, 'admin.groups.platforms.openai')
    await wrapper.findAll('label').find(label => label.text().includes('OpenAI group'))!
      .get('input[type="checkbox"]').setValue(true)
    await wrapper.findAll('button').filter(button => button.text() === '+ common.add').at(-1)!.trigger('click')
    await flushPromises()
    const modelInput = wrapper.get('input[placeholder="admin.channels.form.modelsPlaceholder"]')
    await modelInput.setValue('gpt-6-astra')
    await modelInput.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    return wrapper
  }

  async function setMultiplier(wrapper: View, value: string) {
    multiplierInput(wrapper).value = value
    multiplierInput(wrapper).dispatchEvent(new Event('input', { bubbles: true }))
    await flushPromises()
  }

  it('persists, reopens, updates and clears the multiplier through the real pricing card', async () => {
    const wrapper = await createPricingForm()
    await setMultiplier(wrapper, '3')
    await wrapper.get('#channel-form').trigger('submit')
    await flushPromises()
    expect(showError).not.toHaveBeenCalled()
    expect(create).toHaveBeenCalledOnce()
    expect(create.mock.calls[0][0].model_pricing[0].ultrafast_multiplier).toBe(3)

    for (const [previous, next] of [['3', '1.25'], ['1.25', '']]) {
      await clickButton(wrapper, 'common.edit')
      await clickButton(wrapper, 'admin.groups.platforms.openai')
      expect(multiplierInput(wrapper).value).toBe(previous)
      await setMultiplier(wrapper, next)
      await wrapper.get('#channel-form').trigger('submit')
      await flushPromises()
      expect(showError).not.toHaveBeenCalled()
      expect(update.mock.lastCall![1].model_pricing[0].ultrafast_multiplier).toBe(next ? Number(next) : null)
    }
    await clickButton(wrapper, 'common.edit')
    await clickButton(wrapper, 'admin.groups.platforms.openai')
    expect(multiplierInput(wrapper).value).toBe('')
    wrapper.unmount()
  })

  it.each(['0', '-1'])('rejects a non-positive multiplier %s before sending a request', async value => {
    const wrapper = await createPricingForm()
    await setMultiplier(wrapper, value)
    await wrapper.get('#channel-form').trigger('submit')
    await flushPromises()
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('admin.channels.form.multiplierPositive'))
    expect(create).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('shows Ultra Fast only for OpenAI channel pricing', async () => {
    const wrapper = await createPricingForm()
    const entry = wrapper.findComponent(PricingEntryCard).props('entry')
    for (const [platform, enabled, visible] of [
      ['openai', true, true], ['anthropic', true, false], ['openai', false, false]
    ] as const) {
      const card = mount(PricingEntryCard, { props: { entry, platform, enableTierMultipliers: enabled } })
      expect(card.text().includes('admin.channels.form.ultrafastMultiplier')).toBe(visible)
      card.unmount()
    }
    wrapper.unmount()
  })

  it('saves, reopens, and explicitly clears fal routes without changing existing pricing', async () => {
    getGroups.mockResolvedValue([{ id: 22, name: 'OpenAI group', platform: 'openai', status: 'active', subscription_type: 'standard', allow_image_generation: true }])
    getAccounts.mockResolvedValue({ items: [{ id: 8, name: 'fal account' }], pages: 1 })
    const wrapper = await createPricingForm()
    expect(getAccounts).not.toHaveBeenCalled()
    await clickButton(wrapper, 'admin.channels.falVideo.add')
    expect(getAccounts).toHaveBeenCalledWith(1, 100, expect.objectContaining({ platform: 'fal', type: 'apikey' }))
    const editor = wrapper.get('fieldset')
    await editor.findAll('select')[1].setValue('8')
    await wrapper.get('#channel-form').trigger('submit')
    expect(showError).toHaveBeenCalledWith('admin.channels.falVideo.invalid')
    expect(create).not.toHaveBeenCalled()
    getFalVideoPublicPricing.mockResolvedValueOnce({ '480p': 0.08, '720p': 0.14, '1080p': 0.25 })
    await clickButton(wrapper, 'admin.channels.falVideo.loadPricing')
    const priceInputs = editor.findAll('input[type="number"]')
    expect(priceInputs.map(input => (input.element as HTMLInputElement).value)).toEqual(['', '', ''])
    await clickButton(wrapper, 'admin.channels.falVideo.applyPricing')
    expect(priceInputs.map(input => (input.element as HTMLInputElement).value)).toEqual(['0.08', '0.14', '0.25'])
    getFalVideoPublicPricing.mockRejectedValueOnce(new Error('offline'))
    await clickButton(wrapper, 'admin.channels.falVideo.loadPricing')
    expect(editor.get('[role="alert"]').text()).toBe('admin.channels.falVideo.pricingFailed')
    expect(priceInputs.map(input => (input.element as HTMLInputElement).value)).toEqual(['0.08', '0.14', '0.25'])
    await priceInputs[0].setValue('')
    await priceInputs[2].setValue('')
    await editor.findAll('input[type="number"]')[1].setValue('0.12')
    await wrapper.get('#channel-form').trigger('submit')
    await flushPromises()
    expect(create.mock.lastCall![0].features_config.fal_video_routes).toEqual([
      { group_id: 22, account_id: 8, model: 'grok-imagine-video-1.5', price_usd_per_second: { '720p': 0.12 } }
    ])
    await clickButton(wrapper, 'common.edit')
    expect((wrapper.get('fieldset input[type="number"]').element as HTMLInputElement).value).toBe('')
    expect((wrapper.get('fieldset').findAll('input[type="number"]')[1].element as HTMLInputElement).value).toBe('0.12')
    await clickButton(wrapper, 'admin.channels.falVideo.remove')
    await wrapper.get('#channel-form').trigger('submit')
    await flushPromises()
    expect(update.mock.lastCall![1].features_config.fal_video_routes).toEqual([])
    expect(update.mock.lastCall![1].model_pricing).toEqual(create.mock.lastCall![0].model_pricing)
    wrapper.unmount()
  })
})
