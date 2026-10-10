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

// jsdom has no media queries. Every query reports no match, which selects the
// wide layout; tests of narrow layouts replace this stub.
window.matchMedia = (query: string): MediaQueryList => ({
  matches: false,
  media: query,
  onchange: null,
  addEventListener: () => {},
  removeEventListener: () => {},
  addListener: () => {},
  removeListener: () => {},
  dispatchEvent: () => false,
});

// Node's experimental global localStorage shadows jsdom's and is unusable
// without a backing file, so tests get an in-memory Storage instead.
class MemoryStorage implements Storage {
  private readonly items = new Map<string, string>();
  get length(): number {
    return this.items.size;
  }
  clear(): void {
    this.items.clear();
  }
  getItem(key: string): string | null {
    return this.items.get(key) ?? null;
  }
  key(index: number): string | null {
    return [...this.items.keys()][index] ?? null;
  }
  removeItem(key: string): void {
    this.items.delete(key);
  }
  setItem(key: string, value: string): void {
    this.items.set(key, String(value));
  }
}

Object.defineProperty(globalThis, "localStorage", { value: new MemoryStorage(), configurable: true });
