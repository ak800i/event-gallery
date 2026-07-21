import { describe, expect, it } from 'vitest'
import { brandingThemeStyle, DEFAULT_BRANDING, formatBrandingText } from './branding'

describe('branding utilities', () => {
  it('maps every configurable color to the public CSS variable contract', () => {
    expect(brandingThemeStyle(DEFAULT_BRANDING)).toEqual({
      '--color-bg': '#faf6f2',
      '--color-surface': '#ffffff',
      '--color-primary': '#7a5c48',
      '--color-primary-dark': '#5c4433',
      '--color-text': '#2b2420',
      '--color-muted': '#7a7268',
      '--color-border': '#e5ddd3',
      '--color-danger': '#b3432b',
    })
  })

  it('substitutes supported plain-text placeholders without interpreting markup', () => {
    expect(formatBrandingText('<b>Limit:</b> {maxSize}', { maxSize: '5 GB' })).toBe('<b>Limit:</b> 5 GB')
  })
})
