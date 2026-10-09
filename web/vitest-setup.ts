import "@testing-library/jest-dom/vitest";

class TestResizeObserver implements ResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

globalThis.ResizeObserver = TestResizeObserver;

// jsdom has no layout, so every observed element counts as on screen. Tests of
// offscreen behavior replace this stub.
class TestIntersectionObserver implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = "0px";
  readonly thresholds = [0];
  constructor(private readonly callback: IntersectionObserverCallback) {}
  observe(target: Element): void {
    const entry = { isIntersecting: true, target } as IntersectionObserverEntry;
    this.callback([entry], this);
  }
  unobserve(): void {}
  disconnect(): void {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}

globalThis.IntersectionObserver = TestIntersectionObserver;
HTMLElement.prototype.scrollIntoView = () => {};
