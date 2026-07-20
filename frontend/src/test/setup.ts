import '@testing-library/jest-dom/vitest'

// Layout components need a non-zero container width. jsdom performs no
// layout and otherwise reports clientWidth=0, so React Photo Album correctly
// computes an empty model instead of rendering tiles.
Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
  configurable: true,
  value: 1024,
})

if (typeof globalThis.ResizeObserver === 'undefined') {
  class MockResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = MockResizeObserver
}

// jsdom does not implement IntersectionObserver; the Gallery's infinite
// scroll sentinel needs at least a no-op stand-in so tests don't crash.
if (typeof globalThis.IntersectionObserver === 'undefined') {
  class MockIntersectionObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  // @ts-expect-error -- partial mock is sufficient for tests
  globalThis.IntersectionObserver = MockIntersectionObserver
}
