<script lang="ts">
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui/select-dropdown";
  import { onMount } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { appURL } from "../base-path";
  import { listRepositories, repositoryLabel } from "../repository";
  import {
    DEFAULT_BROWSE_QUERY,
    formatBrowseQuery,
    formatHomeQuery,
    interceptNavClick,
    navigate,
    type Route,
  } from "../router";
  import ThemeToggle from "./ThemeToggle.svelte";

  let { route, baseUrl = "" }: { route: Route; baseUrl?: string } = $props();

  const client = $derived(createBenchDBClient(baseUrl));
  const routeName = $derived(route.name);
  const routeQ = $derived(route.name === "browse" ? route.query.q : "");

  // term is writable (bind:value below), so it must be $state. The effect
  // re-syncs it when the route's q changes underneath (back/forward).
  let term = $state("");
  $effect(() => {
    term = routeQ;
  });

  // Runs and Benchmarks carry the project in their URL. Other pages keep the
  // last project the viewer chose so the nav links and search stay scoped.
  const routeRepository = $derived(
    route.name === "home" || route.name === "browse" ? route.query.repository : null,
  );
  let rememberedRepository = $state("");
  $effect(() => {
    if (routeRepository !== null) rememberedRepository = routeRepository;
  });
  const repository = $derived(routeRepository ?? rememberedRepository);

  let repositories = $state<string[]>([]);
  let repositoriesError = $state<string | null>(null);
  onMount(() => {
    listRepositories(client).then(
      (loaded) => (repositories = loaded),
      (err: unknown) => (repositoriesError = err instanceof Error ? err.message : String(err)),
    );
  });

  const projectOptions = $derived.by((): SelectDropdownOption[] => {
    const urls = repository !== "" && !repositories.includes(repository) ? [repository, ...repositories] : repositories;
    return [
      { value: "", label: "All projects" },
      ...urls.map((url) => ({ value: url, label: repositoryLabel(url) })),
    ];
  });
  // Keep the switcher even for one project: All projects also includes runs
  // submitted without a repository.
  const showProjects = $derived(repositoriesError !== null || projectOptions.length > 1);

  const runsHref = $derived(`/${formatHomeQuery({ repository })}`);
  const benchmarksHref = $derived(`/series${formatBrowseQuery({ ...DEFAULT_BROWSE_QUERY, repository })}`);

  // A global search is a fresh query: it keeps only the project scope.
  function submit(e: SubmitEvent) {
    e.preventDefault();
    navigate(`/series${formatBrowseQuery({ ...DEFAULT_BROWSE_QUERY, q: term.trim(), repository })}`);
  }

  const BENCHMARK_ROUTES: Array<Route["name"]> = [
    "browse",
    "trend",
    "benchmark-trend",
    "series-leaf",
    "results-list",
    "result",
  ];

  // Machines differ between projects, so a project change clears the machine
  // filter and keeps the rest of the Benchmarks view. On Runs it keeps the
  // commit search and starts from the first page.
  function selectRepository(next: string) {
    if (route.name === "home") {
      navigate(`/${formatHomeQuery({ repository: next, q: route.query.q })}`);
    } else if (route.name === "browse") {
      navigate(`/series${formatBrowseQuery({ ...route.query, repository: next, hardware: "" })}`);
    } else if (BENCHMARK_ROUTES.includes(route.name)) {
      navigate(`/series${formatBrowseQuery({ ...DEFAULT_BROWSE_QUERY, repository: next })}`);
    } else {
      navigate(`/${formatHomeQuery({ repository: next })}`);
    }
  }

  const nav = $derived<Array<{ label: string; href: string; route: Route["name"]; active: Array<Route["name"]> }>>([
    { label: "Runs", href: runsHref, route: "home", active: ["home", "run", "batch", "ci-report"] },
    { label: "Benchmarks", href: benchmarksHref, route: "browse", active: BENCHMARK_ROUTES },
    { label: "Compare", href: "/compare", route: "compare", active: ["compare"] },
    { label: "Account", href: "/account", route: "account", active: ["account"] },
  ]);

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  function active(item: { active: Array<Route["name"]> }): boolean {
    return item.active.includes(routeName);
  }

  function ariaCurrent(item: { route: Route["name"]; active: Array<Route["name"]> }): "page" | "location" | undefined {
    if (!active(item)) return undefined;
    return routeName === item.route ? "page" : "location";
  }
</script>

<header class="topbar">
  <div class="brand-group">
    <a class="brand" href={appURL("/")} onclick={(e) => go(e, "/")}>
      <span class="brand-mark" aria-hidden="true">B</span>
      <span class="brand-name">BenchDB</span>
    </a>
    {#if showProjects}
      <div class="project-switcher" title={repositoriesError ?? undefined}>
        <SelectDropdown
          value={repository}
          options={projectOptions}
          title="Project"
          disabled={repositoriesError !== null}
          onchange={selectRepository}
        />
      </div>
    {/if}
  </div>
  <nav class="primary-nav" aria-label="Primary navigation">
    {#each nav as item (item.label)}
      <a
        class="nav-link"
        class:active={active(item)}
        aria-current={ariaCurrent(item)}
        href={appURL(item.href)}
        onclick={(e) => go(e, item.href)}
      >{item.label}</a>
    {/each}
    <a class="nav-link docs-link" href={appURL("/docs")}>API Docs</a>
  </nav>
  <div class="header-end">
    <form class="search" role="search" aria-label="Global series search" onsubmit={submit}>
      <label class="sr-only" for="topbar-series-search">Series search query</label>
      <div class="search-control">
        <span class="search-prefix" aria-hidden="true">series</span>
        <input
          id="topbar-series-search"
          type="search"
          placeholder="benchmark, machine, tag"
          autocomplete="off"
          spellcheck="false"
          bind:value={term}
        />
        <button type="submit" class="search-submit" aria-label="Search series">Search</button>
      </div>
    </form>
    <ThemeToggle />
  </div>
</header>

<style>
  .topbar {
    min-height: var(--app-header-height);
    display: grid;
    grid-template-columns: auto minmax(320px, 1fr) minmax(280px, 420px);
    align-items: center;
    gap: 10px;
    padding: 7px 12px;
    background: var(--c-shell);
    border-bottom: 1px solid var(--c-border);
    box-shadow: 0 1px 0 rgba(16, 24, 40, 0.04), 0 8px 18px rgba(16, 24, 40, 0.04);
    flex-shrink: 0;
    position: sticky;
    top: 0;
    z-index: 20;
  }

  .brand-group {
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .project-switcher {
    min-width: 0;
    max-width: 240px;
  }

  .project-switcher :global(.kit-select-dropdown__trigger) {
    max-width: 100%;
    font-weight: 650;
  }

  .brand {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    min-width: max-content;
    font-weight: 700;
    font-size: 0.95rem;
    color: var(--c-text);
    text-decoration: none;
    white-space: nowrap;
  }
  .brand-mark {
    width: 26px;
    height: 26px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border: 1px solid color-mix(in srgb, var(--c-accent) 42%, transparent);
    border-radius: var(--radius-md);
    background: var(--c-accent);
    color: var(--c-on-accent);
    font-size: 0.78rem;
    font-weight: 800;
  }

  .brand-name {
    letter-spacing: 0;
  }

  .primary-nav {
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 2px;
    padding: 2px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--c-bg-inset) 72%, var(--c-surface));
    overflow-x: auto;
    overscroll-behavior-x: contain;
    scrollbar-width: none;
  }

  .primary-nav::-webkit-scrollbar {
    display: none;
  }

  .nav-link {
    position: relative;
    height: 30px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    color: var(--c-text-muted);
    text-decoration: none;
    font-size: 0.78rem;
    font-weight: 650;
    letter-spacing: 0;
    white-space: nowrap;
    flex: 0 0 auto;
  }

  .nav-link:hover {
    background: var(--c-surface-hover);
    color: var(--c-text);
  }

  .nav-link.active {
    color: var(--c-text);
    background: var(--c-surface);
    box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--c-accent) 25%, var(--c-border-muted));
  }

  .nav-link.active::after {
    content: "";
    position: absolute;
    right: 10px;
    bottom: 4px;
    left: 10px;
    height: 1px;
    border-radius: 999px;
    background: var(--c-accent);
  }

  .header-end {
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .search {
    min-width: 0;
    flex: 1 1 auto;
  }

  .search-control {
    height: 32px;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 0 4px 0 9px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-md);
    background: var(--c-surface);
    color: var(--c-text-muted);
    box-shadow: var(--shadow-hairline);
  }

  .search-control:focus-within {
    border-color: var(--c-accent);
    box-shadow: 0 0 0 3px var(--c-focus-ring);
  }

  .search-prefix {
    color: var(--c-text-faint);
    font-size: 0.68rem;
    font-weight: 750;
    line-height: 1;
    text-transform: uppercase;
  }

  .search input {
    min-width: 0;
    width: 100%;
    height: 30px;
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--c-text);
    font-size: 0.8rem;
  }

  .search input:focus-visible {
    outline: 2px solid var(--c-accent);
    outline-offset: 2px;
  }

  .search input::placeholder {
    color: var(--c-text-faint);
  }

  .search-submit {
    height: 24px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 0 8px;
    border: 1px solid color-mix(in srgb, var(--c-accent) 40%, var(--c-border-muted));
    border-radius: var(--radius-sm);
    background: var(--c-accent);
    color: var(--c-on-accent);
    cursor: pointer;
    font-size: 0.72rem;
    font-weight: 750;
    line-height: 1;
    white-space: nowrap;
  }

  .search-submit:hover {
    background: var(--c-accent-strong);
  }

  .search-submit:focus-visible {
    outline: 2px solid var(--c-accent);
    outline-offset: 2px;
  }

  @media (forced-colors: active) {
    .search-control:focus-within {
      outline: 2px solid Highlight;
      outline-offset: 2px;
      box-shadow: none;
    }

    .search input:focus-visible {
      outline-color: Highlight;
    }
  }

  @media (max-width: 1120px) {
    .topbar {
      grid-template-columns: auto minmax(180px, 1fr);
      grid-template-areas:
        "brand search"
        "nav nav";
      align-items: center;
      padding: 7px 8px;
    }

    .brand-group {
      grid-area: brand;
    }

    .primary-nav {
      grid-area: nav;
      width: 100%;
      padding-bottom: 3px;
    }

    .header-end {
      grid-area: search;
    }
  }

  @media (max-width: 520px) {
    .topbar {
      grid-template-columns: minmax(0, 1fr);
      grid-template-areas:
        "brand"
        "search"
        "nav";
    }

    .brand-group,
    .header-end,
    .primary-nav {
      width: 100%;
    }

    .primary-nav {
      flex-wrap: wrap;
      row-gap: 4px;
      overflow-x: visible;
      padding: 2px 0 0;
    }

    .nav-link {
      min-height: 30px;
      padding: 0 9px;
      font-size: 0.74rem;
    }
  }
</style>
