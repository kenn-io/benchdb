<script lang="ts">
  import { appURL } from "./lib/base-path";
  import { onMount } from "svelte";

  import AccountPage from "./lib/components/AccountPage.svelte";
  import BatchPage from "./lib/components/BatchPage.svelte";
  import CIReportPage from "./lib/components/CIReportPage.svelte";
  import ComparePage from "./lib/components/ComparePage.svelte";
  import RecentRunsHome from "./lib/components/RecentRunsHome.svelte";
  import ResultPage from "./lib/components/ResultPage.svelte";
  import ResultsPage from "./lib/components/ResultsPage.svelte";
  import RunPage from "./lib/components/RunPage.svelte";
  import SeriesBrowse from "./lib/components/SeriesBrowse.svelte";
  import AppSidebar from "./lib/components/AppSidebar.svelte";
  import TrendPage from "./lib/components/TrendPage.svelte";
  import { matchRoute, NAVIGATE_EVENT, pageTitle, type Route } from "./lib/router";

  function currentRoute(): Route {
    return matchRoute(window.location.pathname, window.location.search);
  }

  let route = $state<Route>(currentRoute());
  let content: HTMLElement;

  $effect(() => {
    document.title = pageTitle(route);
  });

  // Moving focus (not just scrolling) makes the next Tab start in the page
  // content, past the sidebar.
  function skipToContent(e: MouseEvent) {
    e.preventDefault();
    content.focus();
  }

  // Re-resolve on browser back/forward and on in-app navigate() pushes.
  onMount(() => {
    const resolve = () => {
      route = currentRoute();
    };
    window.addEventListener("popstate", resolve);
    window.addEventListener(NAVIGATE_EVENT, resolve);
    return () => {
      window.removeEventListener("popstate", resolve);
      window.removeEventListener(NAVIGATE_EVENT, resolve);
    };
  });
</script>

<a class="skip-link" href="#main-content" onclick={skipToContent}>Skip to content</a>

<div class="app-shell">
  <AppSidebar {route} />

  <div class="app-content" id="main-content" tabindex="-1" bind:this={content}>
    {#if route.name === "home"}
      {#key JSON.stringify(route.query)}
        <RecentRunsHome query={route.query} />
      {/key}
    {:else if route.name === "browse"}
      <SeriesBrowse query={route.query} />
    {:else if route.name === "series-leaf"}
      {#key route.resultId}
        <TrendPage source={{ kind: "result", resultId: route.resultId }} query={route.query} />
      {/key}
    {:else if route.name === "trend"}
      {#key route.fingerprint}
        <TrendPage source={{ kind: "fingerprint", fingerprint: route.fingerprint }} query={route.query} />
      {/key}
    {:else if route.name === "benchmark-trend"}
      {#key route.benchmarkId}
        <TrendPage source={{ kind: "benchmark", benchmarkId: route.benchmarkId }} query={route.query} />
      {/key}
    {:else if route.name === "result"}
      {#key route.resultId}
        <ResultPage resultId={route.resultId} />
      {/key}
    {:else if route.name === "results-list"}
      {#key `${route.query.runID}|${route.query.batchID}|${route.query.runReason}|${route.query.earliestTimestamp}|${route.query.latestTimestamp}`}
        <ResultsPage query={route.query} />
      {/key}
    {:else if route.name === "run"}
      {#key route.runId}
        <RunPage runId={route.runId} />
      {/key}
    {:else if route.name === "batch"}
      {#key route.batchId}
        <BatchPage batchId={route.batchId} />
      {/key}
    {:else if route.name === "compare"}
      {#key `${route.query.baseline}|${route.query.contender}`}
        <ComparePage query={route.query} />
      {/key}
    {:else if route.name === "ci-report"}
      {#key `${route.query.repository}|${route.query.commit}|${route.query.runIDs}|${route.query.baselineRunIDs}|${route.query.baseline}|${route.query.threshold}|${route.query.thresholdZ}`}
        <CIReportPage query={route.query} />
      {/key}
    {:else if route.name === "account"}
      <AccountPage />
    {:else}
      <main class="not-found">
        <h1>Not found</h1>
        <p>No page for <code>{window.location.pathname}</code>. <a href={appURL("/series")}>Browse all series</a>.</p>
      </main>
    {/if}
  </div>

</div>
