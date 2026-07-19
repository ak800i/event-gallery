import '@testing-library/jest-dom/vitest'

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
