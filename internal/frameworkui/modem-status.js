/* Read-only live fragments from this administration service only. */
(() => {
  const notice = document.querySelector('[data-modem-update]');
  const pause = document.querySelector('[data-modem-pause]');
  if (!notice || !pause || !document.querySelector('[data-modem-reports]')) return;
  let paused = false, stopped = false, pending = null, timer;
  const endpoint = new URL(location.href);
  endpoint.searchParams.set('fragment', '1');
  pause.hidden = false;

  function unavailable(message) {
    notice.textContent = message;
    const reports = document.querySelector('[data-modem-reports]');
    reports?.setAttribute('data-feed-stale', '');
    reports?.querySelectorAll('[data-modem-live-state]').forEach(cell => { cell.textContent = 'Unavailable · updates stopped'; });
  }

  async function refresh() {
    if (stopped || paused || document.hidden || pending) return;
    pending = new AbortController();
    const timeout = setTimeout(() => pending?.abort(), 5000);
    try {
      const response = await fetch(endpoint, {method: 'GET', credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: pending.signal});
      if (!response.ok) throw new Error('Refresh failed');
      const parsed = new DOMParser().parseFromString(await response.text(), 'text/html');
      const next = parsed.querySelector('[data-modem-reports]');
      if (!next) throw new Error('No modem report');
      if (stopped || paused || document.hidden) return;
      const old = document.querySelector('[data-modem-reports]');
      const focusedHelp = old.contains(document.activeElement) ? [...old.querySelectorAll('.ts-help')].indexOf(document.activeElement) : -1;
      const focused = old.contains(document.activeElement) ? document.activeElement.getAttribute('href') : null;
      old.replaceWith(next);
      if (focusedHelp >= 0) next.querySelectorAll(".ts-help")[focusedHelp]?.focus({preventScroll:true});
      if (focused) [...next.querySelectorAll('a')].find(node => node.getAttribute('href') === focused)?.focus({preventScroll: true});
      notice.textContent = 'Updating automatically';
    } catch (error) {
      if (!stopped && !paused && !document.hidden) unavailable('Live update failed · previous observations retained; current state unknown');
    } finally {
      clearTimeout(timeout);
      pending = null;
      if (!stopped && !paused && !document.hidden) timer = setTimeout(refresh, 2000);
    }
  }

  pause.addEventListener('click', () => {
    paused = !paused;
    pause.textContent = paused ? 'Resume updates' : 'Pause updates';
    clearTimeout(timer);
    if (paused) {
      pending?.abort();
      unavailable('Paused · previous observations retained; current state unknown');
    } else {
      notice.textContent = 'Refreshing observations…';
      refresh();
    }
  });
  document.addEventListener('visibilitychange', () => {
    clearTimeout(timer);
    if (document.hidden) {
      pending?.abort();
      unavailable('Updates suspended while this page is hidden');
    } else if (!paused) refresh();
  });
  window.addEventListener('pagehide', () => { stopped = true; clearTimeout(timer); pending?.abort(); });
  timer = setTimeout(refresh, 2000);
})();
