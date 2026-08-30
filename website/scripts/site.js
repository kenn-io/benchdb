const api = 'https://api.github.com/repos/kenn-io/benchdb';
const hour = 60 * 60 * 1000;

function formatCount(count) {
  if (count < 1000) return String(count);
  const thousands = count / 1000;
  const rounded = thousands >= 10
    ? Math.round(thousands)
    : Math.round(thousands * 10) / 10;
  return `${rounded}k`;
}

async function cachedJson(key, url) {
  const now = Date.now();
  const raw = localStorage.getItem(key);
  if (raw !== null) {
    try {
      const cached = JSON.parse(raw);
      if (now - cached.at <= hour) return cached.value;
    } catch {
      // Fetch a fresh value when the cache is invalid.
    }
  }

  const response = await fetch(url, {
    headers: { Accept: 'application/vnd.github+json' },
  });
  if (!response.ok) return null;
  const value = await response.json();
  localStorage.setItem(key, JSON.stringify({ at: now, value }));
  return value;
}

function setFact(name, text) {
  for (const fact of document.querySelectorAll(`[data-repo-fact="${name}"]`)) {
    const label = fact.querySelector('[data-repo-fact-text]');
    if (label === null) continue;
    label.textContent = text;
    fact.hidden = false;
    const row = fact.closest('[data-repo-facts]');
    if (row !== null) row.hidden = false;
  }
}

async function renderRepoFacts() {
  const [repo, release] = await Promise.all([
    cachedJson('benchdb:repo:v1', api),
    cachedJson('benchdb:release:v1', `${api}/releases/latest`),
  ]);
  if (repo !== null) {
    setFact('stars', formatCount(repo.stargazers_count));
    setFact('forks', formatCount(repo.forks_count));
  }
  if (release !== null) setFact('version', release.tag_name);
}

renderRepoFacts().catch((error) => {
  console.warn('github api unavailable, using static links', error);
});
