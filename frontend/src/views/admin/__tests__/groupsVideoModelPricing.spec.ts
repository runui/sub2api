import { describe, expect, it } from 'vitest'

import {
  addVideoEndpointPrice,
  createVideoModelPricesForm,
  serializeVideoModelPrices,
  videoModelPriceFamilyRows,
  videoModelPriceResolutions
} from '../groupsVideoModelPricing'

describe('Grok video model pricing form', () => {
  it('adds an exact endpoint price with a separate 4K tier', () => {
    const form = createVideoModelPricesForm()
    expect(addVideoEndpointPrice(form, 'https://queue.fal.run/fal-ai/veo3.1')).toBe(false)
    expect(addVideoEndpointPrice(form, ' fal-ai/veo3.1/fast ')).toBe(true)
    const endpoint = 'fal-ai/veo3.1/fast'
    expect(videoModelPriceResolutions(endpoint).map(({ key }) => key)).toContain('4k')
    form[endpoint]['4k'] = 0.45
    expect(serializeVideoModelPrices(form)).toEqual({ [endpoint]: { '4k': 0.45 } })
  })
  it('provides editable rows for both canonical Grok video families', () => {
    const form = createVideoModelPricesForm()

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toEqual([
      'grok-imagine-video',
      'grok-imagine-video-1.5'
    ])
    expect(form['grok-imagine-video']['480p']).toBeNull()
    expect(form['grok-imagine-video-1.5']['1080p']).toBeNull()
  })

  it('serializes only finite non-negative prices and preserves future families', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
    form['grok-imagine-video']['480p'] = 0.05
    form['grok-imagine-video']['720p'] = ''
    form['grok-imagine-video-1.5']['1080p'] = -1

    expect(serializeVideoModelPrices(form)).toEqual({
      'grok-imagine-video': { '480p': 0.05 },
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
  })

  it('round-trips unknown model families so editing does not discard them', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '480p': 0.2 }
    })

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toContain(
      'grok-imagine-video-2'
    )
    expect(serializeVideoModelPrices(form)).toMatchObject({
      'grok-imagine-video-2': { '480p': 0.2 }
    })
  })
})
