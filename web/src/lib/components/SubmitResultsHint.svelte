<script lang="ts">
  import { appURL } from "../base-path";
  import { copyPlainText } from "../clipboard";
  import { interceptNavClick, navigate } from "../router";

  const DOCS_URL = "https://benchdb.io/docs/submitting-results/";

  // The command names this server, so it works when pasted as is.
  const serverURL = window.location.origin + appURL("/").replace(/\/$/, "");
  const command = `benchdb results submit "bench-results/*.json" --server ${serverURL}`;

  let copied = $state(false);

  function openAccount(e: MouseEvent) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate("/account");
  }

  async function copyCommand() {
    try {
      copied = await copyPlainText(command);
    } catch {
      copied = false;
    }
  }
</script>

<div class="submit-hint">
  <p>
    Send benchmark results with the <code>benchdb</code> CLI. Set <code>BENCHDB_TOKEN</code> to an
    API token from your <a href={appURL("/account")} onclick={openAccount}>account</a> first.
  </p>
  <div class="command">
    <code>{command}</code>
    <button type="button" class="button-pill" onclick={copyCommand}>Copy command</button>
    {#if copied}<span class="copied" role="status">copied</span>{/if}
  </div>
  <p><a href={DOCS_URL} target="_blank" rel="noreferrer">How to format and submit results</a></p>
</div>

<style>
  .submit-hint {
    display: grid;
    gap: 8px;
    justify-items: start;
    max-width: 72ch;
    text-align: left;
  }
  .submit-hint p { margin: 0; color: var(--c-text-muted); }
  .command {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }
  .command code {
    padding: 6px 8px;
    border-radius: var(--radius-sm);
    background: var(--c-bg-inset);
    color: var(--c-text);
    font-size: 0.8rem;
    overflow-wrap: anywhere;
  }
  .copied { color: var(--c-text-muted); font-size: 0.8rem; }
</style>
