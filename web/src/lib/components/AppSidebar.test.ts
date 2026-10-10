import { getBenchDB } from "../api/benchdb";
import type { AxiosInstance } from "axios";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { DEFAULT_BROWSE_QUERY, DEFAULT_HOME_QUERY, type Route } from "../router";
import AppSidebar from "./AppSidebar.svelte";

const GET = vi.fn();
vi.mock("../api/client", () => ({
  createBenchDBClient: () => getBenchDB({ get: GET } as unknown as AxiosInstance),
}));

const ARROW = "https://github.com/apache/arrow";
const ARROW_GO = "https://github.com/apache/arrow-go";

function repositoriesResponse(urls: string[]) {
  return { status: 200, data: { repositories: urls.map((repository) => ({ repository })) } };
}

const home = (repository = ""): Route => ({ name: "home", query: { ...DEFAULT_HOME_QUERY, repository } });
const browse = (query: Partial<typeof DEFAULT_BROWSE_QUERY> = {}): Route => ({
  name: "browse",
  query: { ...DEFAULT_BROWSE_QUERY, ...query },
});
const page = (name: "compare" | "run" | "result" | "series-leaf" | "ci-report"): Route => {
  switch (name) {
    case "compare":
      return { name, query: { baseline: "", contender: "", threshold: null, thresholdZ: null } };
    case "run":
      return { name, runId: "run-a" };
    case "result":
      return { name, resultId: "result-a" };
    case "series-leaf":
      return { name, resultId: "result-a", query: { range: { mode: "relative", days: 90 }, sigma: 2, yAxis: "zero" } };
    case "ci-report":
      return {
        name,
        query: {
          saved: false, repository: "", commit: "", runIDs: "", baselineRunIDs: "", baseline: "", threshold: "", thresholdZ: "",
        },
      };
  }
};

// Modified clicks fall through to the anchor's default action; jsdom does not
// implement navigation and would log to stderr. Swallow the default here so the
// component's behavior is unchanged but test output stays clean.
const swallowAnchorNavigation = (e: Event) => {
  const target = e.target;
  if (target instanceof Element && target.closest("a")) {
    e.preventDefault();
  }
};
beforeEach(() => {
  localStorage.clear();
  document.addEventListener("click", swallowAnchorNavigation);
  window.history.replaceState(null, "", "/");
  GET.mockReset();
  GET.mockResolvedValue(repositoriesResponse([]));
});
afterEach(() => document.removeEventListener("click", swallowAnchorNavigation));

describe("AppSidebar", () => {
  it("renders the brand as a home link", () => {
    render(AppSidebar, { props: { route: home() } });
    expect(screen.getByRole("link", { name: "BenchDB" })).toHaveAttribute("href", "/");
  });

  it("exposes primary product navigation", () => {
    render(AppSidebar, { props: { route: page("compare") } });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    expect(within(nav).getByRole("link", { name: "Runs" })).toHaveAttribute("href", "/");
    expect(within(nav).getByRole("link", { name: "Benchmarks" })).toHaveAttribute("href", "/series");
    expect(within(nav).queryByRole("link", { name: "Results" })).not.toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "Compare" })).toHaveAttribute("href", "/compare");
    expect(within(nav).queryByRole("link", { name: "Reports" })).not.toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "Compare" })).toHaveAttribute("aria-current", "page");
    const sidebar = screen.getByRole("complementary", { name: "Sidebar" });
    expect(within(sidebar).getByRole("link", { name: "Account" })).toHaveAttribute("href", "/account");
    expect(within(sidebar).getByRole("link", { name: "API Docs" })).toHaveAttribute("href", "/docs");
  });

  it("collapses to an icon rail and remembers the choice", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    const first = render(AppSidebar, { props: { route: home(ARROW) } });
    await screen.findByRole("combobox", { name: "Project: apache/arrow" });

    await fireEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
    expect(screen.queryByRole("combobox", { name: /project/i })).toBeNull();
    expect(screen.queryByRole("searchbox", { name: "Series search query" })).toBeNull();
    expect(screen.getByRole("button", { name: "Project: apache/arrow (expand sidebar)" })).toBeInTheDocument();
    // Links keep their names for assistive technology and keyboard users.
    expect(screen.getByRole("link", { name: "Benchmarks" })).toHaveAttribute("title", "Benchmarks");
    first.unmount();

    render(AppSidebar, { props: { route: home(ARROW) } });
    expect(screen.getByRole("button", { name: "Expand sidebar" })).toBeInTheDocument();
    await fireEvent.click(screen.getByRole("button", { name: "Search benchmarks (expand sidebar)" }));
    expect(screen.getByRole("searchbox", { name: "Series search query" })).toHaveFocus();
    expect(screen.getByRole("button", { name: "Collapse sidebar" })).toBeInTheDocument();
  });

  it("opens as a drawer behind a menu button on narrow screens", async () => {
    const original = window.matchMedia;
    window.matchMedia = (query: string) => ({ ...original(query), matches: true });
    try {
      GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
      const { rerender } = render(AppSidebar, { props: { route: home(ARROW) } });
      const sidebar = screen.getByRole("complementary", { name: "Sidebar" });
      const content = sidebar.parentElement!.appendChild(document.createElement("main"));
      // The collapse preference does not apply to the drawer.
      expect(screen.queryByRole("button", { name: /collapse sidebar|expand sidebar/i })).toBeNull();
      expect(screen.getByText("apache/arrow", { selector: ".mobile-project" })).toBeInTheDocument();

      const menu = screen.getByRole("button", { name: "Open navigation" });
      expect(menu).toHaveAttribute("aria-expanded", "false");
      await fireEvent.click(menu);
      expect(menu).toHaveAttribute("aria-expanded", "true");
      expect(sidebar).toHaveClass("drawer-open");
      await waitFor(() => expect(within(sidebar).getByRole("button", { name: "Close navigation" })).toHaveFocus());

      // The page behind the open drawer is inert; the backdrop stays live.
      expect(content).toHaveAttribute("inert");
      expect(menu.closest("header")).toHaveAttribute("inert");
      expect(document.querySelector(".drawer-backdrop")).not.toHaveAttribute("inert");

      await fireEvent.keyDown(window, { key: "Escape" });
      expect(sidebar).not.toHaveClass("drawer-open");
      expect(content).not.toHaveAttribute("inert");
      await waitFor(() => expect(menu).toHaveFocus());

      await fireEvent.click(menu);
      await rerender({ route: page("compare") });
      expect(sidebar).not.toHaveClass("drawer-open");
    } finally {
      window.matchMedia = original;
    }
  });

  it("exposes global series search with stable role and label", () => {
    render(AppSidebar, { props: { route: home() } });
    const search = screen.getByRole("search", { name: "Global series search" });
    expect(within(search).getByRole("searchbox", { name: "Series search query" })).toBeInTheDocument();
    expect(within(search).getByRole("button", { name: "Search series" })).toBeInTheDocument();
  });

  it("navigates to series with q and default filters on search submit", async () => {
    render(AppSidebar, { props: { route: home() } });
    const box = screen.getByRole("searchbox", { name: "Series search query" });
    await fireEvent.input(box, { target: { value: "tpch " } });
    await fireEvent.submit(box.closest("form")!);
    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe("?q=tpch");
  });

  it("navigates to series when the explicit search control is clicked", async () => {
    render(AppSidebar, { props: { route: home() } });
    const box = screen.getByRole("searchbox", { name: "Series search query" });
    await fireEvent.input(box, { target: { value: "arrow " } });
    await fireEvent.click(screen.getByRole("button", { name: "Search series" }));
    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe("?q=arrow");
  });

  it("seeds the box from the route's q", () => {
    render(AppSidebar, { props: { route: browse({ q: "demo" }) } });
    expect(screen.getByRole("searchbox", { name: "Series search query" })).toHaveValue("demo");
  });

  it("marks active deep-route navigation as a current location, not the current page", () => {
    render(AppSidebar, { props: { route: page("series-leaf") } });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    const activeLink = within(nav).getByRole("link", { name: "Benchmarks", current: "location" });
    expect(activeLink).toHaveAttribute("href", "/series");
    expect(activeLink).toHaveTextContent("Benchmarks");
    expect(within(nav).queryByRole("link", { name: "Benchmarks", current: "page" })).not.toBeInTheDocument();
  });

  it("keeps individual results inside the Benchmarks navigation hierarchy", () => {
    render(AppSidebar, { props: { route: page("result") } });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    expect(within(nav).getByRole("link", { name: "Benchmarks", current: "location" })).toHaveAttribute(
      "href",
      "/series",
    );
  });

  it("keeps run records inside the Runs navigation hierarchy", () => {
    render(AppSidebar, { props: { route: page("run") } });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    expect(within(nav).getByRole("link", { name: "Runs", current: "location" })).toHaveAttribute(
      "href",
      "/",
    );
  });

  it("keeps CI reports inside the Runs navigation hierarchy", () => {
    render(AppSidebar, { props: { route: page("ci-report") } });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    expect(within(nav).getByRole("link", { name: "Runs", current: "location" })).toBeInTheDocument();
  });

  it("hides the project switcher when no project exists", async () => {
    render(AppSidebar, { props: { route: home() } });
    await waitFor(() => expect(GET).toHaveBeenCalledWith("/api/repositories", undefined));
    expect(screen.queryByRole("combobox", { name: /project/i })).not.toBeInTheDocument();
  });

  it("offers the switcher for a single project", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW]));
    render(AppSidebar, { props: { route: home() } });
    await fireEvent.click(await screen.findByRole("combobox", { name: "Project: All projects" }));
    expect(screen.getByRole("option", { name: "apache/arrow" })).toBeInTheDocument();
  });

  it("keeps the commit search and resets paging when switching projects on Runs", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    render(AppSidebar, {
      props: { route: { name: "home", query: { repository: ARROW, q: "abcdef", offset: 25 } } },
    });
    await fireEvent.click(await screen.findByRole("combobox", { name: "Project: apache/arrow" }));
    await fireEvent.click(screen.getByRole("option", { name: "All projects" }));
    expect(`${window.location.pathname}${window.location.search}`).toBe("/?q=abcdef");
  });

  it("scopes nav links and global search to the route's project", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    render(AppSidebar, { props: { route: browse({ repository: ARROW_GO, q: "sort" }) } });
    await screen.findByRole("combobox", { name: "Project: apache/arrow-go" });
    const nav = screen.getByRole("navigation", { name: "Primary navigation" });
    const scoped = `repository=${encodeURIComponent(ARROW_GO)}`;
    expect(within(nav).getByRole("link", { name: "Runs" })).toHaveAttribute("href", `/?${scoped}`);
    expect(within(nav).getByRole("link", { name: "Benchmarks" })).toHaveAttribute("href", `/series?${scoped}`);

    const box = screen.getByRole("searchbox", { name: "Series search query" });
    await fireEvent.input(box, { target: { value: "join" } });
    await fireEvent.submit(box.closest("form")!);
    expect(window.location.search).toBe(`?q=join&${scoped}`);
  });

  it("remembers the project on pages without a project in the URL", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    const { rerender } = render(AppSidebar, { props: { route: home(ARROW) } });
    await screen.findByRole("combobox", { name: "Project: apache/arrow" });
    await rerender({ route: page("run") });
    expect(screen.getByRole("combobox", { name: "Project: apache/arrow" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Benchmarks" }))
      .toHaveAttribute("href", `/series?repository=${encodeURIComponent(ARROW)}`);

    await rerender({ route: home() });
    expect(screen.getByRole("combobox", { name: "Project: All projects" })).toBeInTheDocument();
  });

  it("switches projects in place on Benchmarks and clears the machine filter", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    render(AppSidebar, { props: { route: browse({ repository: ARROW, hardware: "m5", q: "sort", view: "charts" }) } });
    await fireEvent.click(await screen.findByRole("combobox", { name: "Project: apache/arrow" }));
    await fireEvent.click(screen.getByRole("option", { name: "apache/arrow-go" }));
    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe(`?q=sort&repository=${encodeURIComponent(ARROW_GO)}&view=charts`);
  });

  it("opens Benchmarks from benchmark detail pages and Runs from everything else", async () => {
    GET.mockResolvedValue(repositoriesResponse([ARROW, ARROW_GO]));
    const { unmount } = render(AppSidebar, { props: { route: page("result") } });
    await fireEvent.click(await screen.findByRole("combobox", { name: "Project: All projects" }));
    await fireEvent.click(screen.getByRole("option", { name: "apache/arrow" }));
    expect(`${window.location.pathname}${window.location.search}`)
      .toBe(`/series?repository=${encodeURIComponent(ARROW)}`);
    unmount();

    render(AppSidebar, { props: { route: page("compare") } });
    await fireEvent.click(await screen.findByRole("combobox", { name: "Project: All projects" }));
    await fireEvent.click(screen.getByRole("option", { name: "All projects" }));
    expect(`${window.location.pathname}${window.location.search}`).toBe("/");
  });

  it("shows an unknown project from the URL and disables the switcher when projects fail to load", async () => {
    GET.mockResolvedValue({ status: 500, data: { detail: "database unavailable" } });
    render(AppSidebar, { props: { route: home(ARROW) } });
    const switcher = await screen.findByRole("combobox", { name: "Project: apache/arrow" });
    await waitFor(() => expect(switcher).toBeDisabled());
    expect(switcher.closest(".project-switcher")).toHaveAttribute("title", "database unavailable");
  });

  it("leaves modified brand clicks to the browser", async () => {
    window.history.replaceState(null, "", "/?q=x");
    render(AppSidebar, { props: { route: home() } });
    await fireEvent.click(screen.getByRole("link", { name: "BenchDB" }), { metaKey: true });
    // navigate() was not called: the URL still carries the original search.
    expect(window.location.search).toBe("?q=x");
  });
});

it("keeps native links and SPA navigation under the deployed base path", async () => {
  const base = document.createElement("base");
  base.href = "/tools/bench/";
  document.head.append(base);
  try {
    render(AppSidebar, { props: { route: home() } });
    expect(screen.getByRole("link", { name: "Benchmarks" })).toHaveAttribute("href", "/tools/bench/series");
    expect(screen.getByRole("link", { name: "API Docs" })).toHaveAttribute("href", "/tools/bench/docs");
    await fireEvent.click(screen.getByRole("link", { name: "Benchmarks" }));
    expect(window.location.pathname).toBe("/tools/bench/series");
  } finally {
    base.remove();
    history.replaceState(null, "", "/");
  }
});
