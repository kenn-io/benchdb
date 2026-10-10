<script lang="ts">
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui/select-dropdown";
  import { trapFocus } from "@kenn-io/kit-ui/utils/focus-trap";
  import BookOpenIcon from "@lucide/svelte/icons/book-open";
  import ChartLineIcon from "@lucide/svelte/icons/chart-line";
  import FolderGitIcon from "@lucide/svelte/icons/folder-git-2";
  import GitCompareIcon from "@lucide/svelte/icons/git-compare-arrows";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import MenuIcon from "@lucide/svelte/icons/menu";
  import PanelLeftCloseIcon from "@lucide/svelte/icons/panel-left-close";
  import PanelLeftOpenIcon from "@lucide/svelte/icons/panel-left-open";
  import SearchIcon from "@lucide/svelte/icons/search";
  import UserRoundIcon from "@lucide/svelte/icons/user-round";
  import XIcon from "@lucide/svelte/icons/x";
  import { onMount, tick, type Component } from "svelte";

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

  interface NavItem {
    label: string;
    href: string;
    route: Route["name"];
    active: Array<Route["name"]>;
    icon: Component;
  }

  const nav = $derived<NavItem[]>([
    { label: "Runs", href: runsHref, route: "home", active: ["home", "run", "batch", "ci-report"], icon: HistoryIcon },
    { label: "Benchmarks", href: benchmarksHref, route: "browse", active: BENCHMARK_ROUTES, icon: ChartLineIcon },
    { label: "Compare", href: "/compare", route: "compare", active: ["compare"], icon: GitCompareIcon },
  ]);
  const account: NavItem = { label: "Account", href: "/account", route: "account", active: ["account"], icon: UserRoundIcon };

  // Collapsing to an icon rail is a per-browser preference. Storage can be
  // unavailable (private windows, blocked site data); the sidebar then starts
  // expanded and the choice lasts for the page view.
  const COLLAPSED_KEY = "benchdb.sidebar.collapsed";
  let collapsed = $state(readCollapsed());
  // On narrow screens the full sidebar is a drawer behind the menu button and
  // the collapse preference does not apply.
  const NARROW_QUERY = "(max-width: 899px)";
  let narrow = $state(window.matchMedia(NARROW_QUERY).matches);
  onMount(() => {
    const query = window.matchMedia(NARROW_QUERY);
    const update = () => (narrow = query.matches);
    query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  });
  const rail = $derived(collapsed && !narrow);
  let drawerOpen = $state(false);
  let searchInput = $state<HTMLInputElement>();
  let menuButton = $state<HTMLButtonElement>();
  let drawerCloseButton = $state<HTMLButtonElement>();
  let sidebarElement = $state<HTMLElement>();
  const drawerShown = $derived(narrow && drawerOpen);

  // While the drawer is open, the page behind it can take neither focus nor
  // clicks. The backdrop stays live because tapping it closes the drawer.
  $effect(() => {
    if (!drawerShown || sidebarElement?.parentElement == null) return;
    const background = [...sidebarElement.parentElement.children].filter(
      (element): element is HTMLElement =>
        element instanceof HTMLElement && element !== sidebarElement && !element.classList.contains("drawer-backdrop"),
    );
    for (const element of background) element.toggleAttribute("inert", true);
    return () => {
      for (const element of background) element.toggleAttribute("inert", false);
    };
  });

  async function openDrawer() {
    drawerOpen = true;
    await tick();
    drawerCloseButton?.focus();
  }

  // Dismissing the drawer returns focus to the menu button; navigating away
  // leaves focus with the new page.
  async function closeDrawer() {
    drawerOpen = false;
    // The menu button is inert until the drawer's state has settled.
    await tick();
    menuButton?.focus();
  }

  function readCollapsed(): boolean {
    try {
      return localStorage.getItem(COLLAPSED_KEY) === "true";
    } catch {
      return false;
    }
  }

  function setCollapsed(next: boolean) {
    collapsed = next;
    try {
      localStorage.setItem(COLLAPSED_KEY, String(next));
    } catch {
      // The preference only lasts for this page view.
    }
  }

  async function expandAndFocusSearch() {
    setCollapsed(false);
    await tick();
    searchInput?.focus();
  }

  // Any navigation closes the drawer.
  $effect(() => {
    void route;
    drawerOpen = false;
  });

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Escape" && drawerOpen) closeDrawer();
  }

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

<svelte:window onkeydown={onKeydown} />

{#snippet navLink(item: NavItem)}
  <a
    class="nav-link"
    class:active={active(item)}
    aria-current={ariaCurrent(item)}
    href={appURL(item.href)}
    title={rail ? item.label : undefined}
    onclick={(e) => go(e, item.href)}
  >
    <item.icon size={17} strokeWidth={1.75} aria-hidden="true" />
    <span class="nav-label">{item.label}</span>
  </a>
{/snippet}

{#if narrow}
  <header class="mobile-bar">
    <button type="button" class="icon-button" aria-label="Open navigation" aria-expanded={drawerOpen}
      aria-controls="app-sidebar" bind:this={menuButton} onclick={openDrawer}>
      <MenuIcon size={18} strokeWidth={1.75} aria-hidden="true" />
    </button>
    <a class="brand" href={appURL("/")} onclick={(e) => go(e, "/")}>
      <span class="brand-mark" aria-hidden="true">B</span>
      <span class="brand-name">BenchDB</span>
    </a>
    {#if repository !== ""}<span class="mobile-project">{repositoryLabel(repository)}</span>{/if}
  </header>
{/if}

{#if drawerShown}
  <button type="button" class="drawer-backdrop" aria-label="Close navigation" tabindex="-1"
    onclick={closeDrawer}></button>
{/if}

<aside
  id="app-sidebar"
  class="sidebar"
  class:rail
  class:drawer-open={drawerOpen}
  aria-label="Sidebar"
  bind:this={sidebarElement}
  {@attach drawerShown ? trapFocus : undefined}
>
  <div class="sidebar-head">
    <a class="brand" href={appURL("/")} onclick={(e) => go(e, "/")} title={rail ? "BenchDB" : undefined}>
      <span class="brand-mark" aria-hidden="true">B</span>
      <span class="brand-name">BenchDB</span>
    </a>
    {#if narrow}
      <button type="button" class="icon-button" aria-label="Close navigation" bind:this={drawerCloseButton}
        onclick={closeDrawer}>
        <XIcon size={16} strokeWidth={1.75} aria-hidden="true" />
      </button>
    {:else}
      <button type="button" class="icon-button" aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
        title={collapsed ? "Expand sidebar" : "Collapse sidebar"} onclick={() => setCollapsed(!collapsed)}>
        {#if collapsed}
          <PanelLeftOpenIcon size={16} strokeWidth={1.75} aria-hidden="true" />
        {:else}
          <PanelLeftCloseIcon size={16} strokeWidth={1.75} aria-hidden="true" />
        {/if}
      </button>
    {/if}
  </div>

  {#if rail}
    <div class="rail-tools">
      {#if showProjects}
        <button type="button" class="icon-button" onclick={() => setCollapsed(false)}
          aria-label={`Project: ${repository === "" ? "All projects" : repositoryLabel(repository)} (expand sidebar)`}
          title={repository === "" ? "All projects" : repositoryLabel(repository)}>
          <FolderGitIcon size={17} strokeWidth={1.75} aria-hidden="true" />
        </button>
      {/if}
      <button type="button" class="icon-button" aria-label="Search benchmarks (expand sidebar)" title="Search benchmarks"
        onclick={expandAndFocusSearch}>
        <SearchIcon size={17} strokeWidth={1.75} aria-hidden="true" />
      </button>
    </div>
  {:else}
    <div class="sidebar-tools">
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
      <form class="search" role="search" aria-label="Global series search" onsubmit={submit}>
        <label class="sr-only" for="sidebar-series-search">Series search query</label>
        <SearchIcon class="search-icon" size={14} strokeWidth={1.75} aria-hidden="true" />
        <input
          id="sidebar-series-search"
          type="search"
          placeholder="Search benchmarks"
          autocomplete="off"
          spellcheck="false"
          bind:value={term}
          bind:this={searchInput}
        />
        <button type="submit" class="sr-only">Search series</button>
      </form>
    </div>
  {/if}

  <nav class="primary-nav" aria-label="Primary navigation">
    {#each nav as item (item.label)}
      {@render navLink(item)}
    {/each}
  </nav>

  <div class="sidebar-foot">
    {@render navLink(account)}
    <a class="nav-link" href={appURL("/docs")} title={rail ? "API Docs" : undefined}>
      <BookOpenIcon size={17} strokeWidth={1.75} aria-hidden="true" />
      <span class="nav-label">API Docs</span>
    </a>
    <ThemeToggle compact={rail} />
  </div>
</aside>

<style>
  .sidebar {
    width: 216px;
    flex: 0 0 auto;
    height: 100%;
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 10px 8px;
    overflow-y: auto;
    background: var(--c-shell);
    border-right: 1px solid var(--c-border);
  }

  .sidebar.rail {
    width: 56px;
    align-items: center;
  }

  .sidebar-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    padding: 2px 2px 2px 4px;
  }

  .rail .sidebar-head {
    flex-direction: column;
    padding: 2px 0;
  }

  .brand {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    color: var(--c-text);
    font-weight: 700;
    font-size: 0.95rem;
    text-decoration: none;
    white-space: nowrap;
  }

  .brand-mark {
    width: 26px;
    height: 26px;
    flex: 0 0 auto;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-md);
    background: var(--c-accent);
    color: var(--c-on-accent);
    font-size: 0.78rem;
    font-weight: 800;
  }

  .rail .brand-name,
  .rail .nav-label {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
  }

  .icon-button {
    width: 30px;
    height: 30px;
    flex: 0 0 auto;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 0;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--c-text-muted);
    cursor: pointer;
  }

  .icon-button:hover {
    background: var(--c-surface-hover);
    color: var(--c-text);
  }

  .icon-button:focus-visible,
  .nav-link:focus-visible,
  .brand:focus-visible {
    outline: 2px solid var(--c-accent);
    outline-offset: 2px;
  }

  .sidebar-tools,
  .rail-tools {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .rail-tools {
    align-items: center;
  }

  .project-switcher :global(.kit-select-dropdown__trigger) {
    width: 100%;
    font-weight: 650;
  }

  .search {
    position: relative;
    display: flex;
    align-items: center;
  }

  .search :global(.search-icon) {
    position: absolute;
    left: 9px;
    color: var(--c-text-faint);
    pointer-events: none;
  }

  .search input {
    width: 100%;
    height: 30px;
    padding: 0 8px 0 28px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-md);
    background: var(--c-surface);
    color: var(--c-text);
    font-size: 0.8rem;
  }

  .search input::placeholder {
    color: var(--c-text-faint);
  }

  .search input:focus-visible {
    outline: none;
    border-color: var(--c-accent);
    box-shadow: 0 0 0 3px var(--c-focus-ring);
  }

  @media (forced-colors: active) {
    .search input:focus-visible {
      outline: 2px solid Highlight;
      outline-offset: 2px;
    }
  }

  .primary-nav,
  .sidebar-foot {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .rail .primary-nav,
  .rail .sidebar-foot {
    align-items: center;
  }

  .sidebar-foot {
    margin-top: auto;
    padding-top: 8px;
    border-top: 1px solid var(--c-border-muted);
  }

  .rail .sidebar-foot {
    width: 100%;
  }

  .nav-link {
    position: relative;
    height: 34px;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    color: var(--c-text-muted);
    font-size: 0.82rem;
    font-weight: 600;
    text-decoration: none;
    white-space: nowrap;
  }

  .nav-link:hover {
    background: var(--c-surface-hover);
    color: var(--c-text);
  }

  .nav-link.active {
    background: color-mix(in srgb, var(--c-accent) 11%, transparent);
    color: var(--c-text);
  }

  .nav-link.active::before {
    content: "";
    position: absolute;
    left: -8px;
    top: 7px;
    bottom: 7px;
    width: 3px;
    border-radius: 0 3px 3px 0;
    background: var(--c-accent);
  }

  .rail .nav-link {
    width: 38px;
    justify-content: center;
    padding: 0;
  }


  .mobile-bar,
  .drawer-backdrop {
    display: none;
  }

  @media (max-width: 899px) {
    .mobile-bar {
      height: 48px;
      flex: 0 0 auto;
      display: flex;
      align-items: center;
      gap: 10px;
      padding: 0 10px;
      background: var(--c-shell);
      border-bottom: 1px solid var(--c-border);
    }

    .mobile-project {
      min-width: 0;
      margin-left: auto;
      overflow: hidden;
      color: var(--c-text-muted);
      font-size: 0.78rem;
      font-weight: 600;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .sidebar {
      position: fixed;
      inset: 0 auto 0 0;
      z-index: 40;
      width: min(280px, 86vw);
      transform: translateX(-100%);
      visibility: hidden;
      transition: transform 0.18s ease, visibility 0s linear 0.18s;
      box-shadow: 0 12px 32px rgba(16, 24, 40, 0.18);
    }

    .sidebar.drawer-open {
      transform: none;
      visibility: visible;
      transition: transform 0.18s ease;
    }

    .drawer-backdrop {
      position: fixed;
      inset: 0;
      z-index: 39;
      display: block;
      padding: 0;
      border: 0;
      background: rgba(10, 14, 20, 0.38);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .sidebar,
    .sidebar.drawer-open {
      transition: none;
    }
  }
</style>
