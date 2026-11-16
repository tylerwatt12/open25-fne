/* Site-owned read-only observations. Hardware discovery is explicit only. */
(() => {
  const notice = document.querySelector('[data-hardware-update]');
  const pause = document.querySelector('[data-hardware-pause]');
  const refreshHardware = document.querySelector('[data-hardware-refresh]');
  if (!notice || !pause || !refreshHardware) return;
  let paused = false, stopped = false, pending, timer, selected, chart = 'activity';
  let chartsOpen = false, diagnosticsOpen = false, requestID = 0;
  const inventory = () => document.querySelector('[data-hardware-inventory]');
  const paths = row => JSON.parse(row?.dataset.hardwarePaths || '[]').filter(Boolean);
  const detailRow = row => document.getElementById(row?.querySelector('[data-hardware-toggle]')?.getAttribute('aria-controls'));
  pause.hidden = false;

  function unavailable(message) {
    notice.textContent = message;
    inventory()?.querySelectorAll('[data-hardware-current]').forEach(cell => { cell.textContent = 'Status unavailable'; });
    inventory()?.querySelectorAll('[data-hardware-live-state]').forEach(cell => { cell.textContent = 'Unavailable'; });
  }
  function rememberSections() {
    const detail = detailRow(selected);
    if (!detail) return;
    const charts = detail.querySelector('[data-hardware-section=charts]');
    const diagnostics = detail.querySelector('[data-hardware-section=diagnostics]');
    if (charts) chartsOpen = charts.open;
    if (diagnostics) diagnosticsOpen = diagnostics.open;
    chart = detail.querySelector('[data-hardware-chart]')?.value || chart;
  }
  function schedule() {
    clearTimeout(timer);
    if (!stopped && !paused && !document.hidden) timer = setTimeout(() => load(), 2000);
  }
  function applyActivity(next) {
    rememberSections();
    const focusChart = document.activeElement?.matches('[data-hardware-chart]');
    const focusSection = document.activeElement?.closest('[data-hardware-section]')?.dataset.hardwareSection;
    const templates = [...next.querySelectorAll('template[data-hardware-path]')];
    inventory().querySelectorAll('.hardware-device').forEach(row => {
      const button = row.querySelector('[data-hardware-toggle]');
      if (!button) return;
      const matches = templates.filter(template => paths(row).includes(template.dataset.hardwarePath));
      const cell = row.querySelector('[data-hardware-live-status]');
      cell.replaceChildren();
      const labels = new Set();
      for (const template of matches) {
        const current = template.content.querySelector('[data-hardware-current]');
        if (!current || labels.has(current.textContent)) continue;
        if (labels.size) cell.append(document.createElement('br'));
        labels.add(current.textContent); cell.append(current.cloneNode(true));
      }
      if (!labels.size) cell.textContent = 'Status unavailable';
      if (row !== selected) return;
      const target = detailRow(row).firstElementChild;
      const details = matches.flatMap(template => [...template.content.querySelectorAll('.hardware-detail')]);
      if (!details.length) { target.textContent = 'Current modem activity is unavailable. Refresh hardware to update assignments.'; return; }
      target.replaceChildren(...details.map(detail => detail.cloneNode(true)));
      target.querySelectorAll('[data-hardware-section=charts]').forEach(section => { section.open = chartsOpen; });
      target.querySelectorAll('[data-hardware-section=diagnostics]').forEach(section => { section.open = diagnosticsOpen; });
      if (focusChart) target.querySelector('[data-hardware-chart]')?.focus({preventScroll:true});
      else if (focusSection) target.querySelector(`[data-hardware-section="${focusSection}"] summary`)?.focus({preventScroll:true});
    });
    notice.textContent = 'Activity updated just now';
    if (paused || document.hidden) unavailable(paused ? 'Activity paused · previous observations' : 'Activity updates suspended');
  }
  async function load(kind = 'activity') {
    if (stopped || (kind === 'activity' && (paused || document.hidden))) return;
    clearTimeout(timer); pending?.abort();
    const controller = new AbortController(); pending = controller;
    const id = ++requestID, timeout = setTimeout(() => controller.abort(), 5000);
    const endpoint = new URL('/hardware', location.origin);
    endpoint.searchParams.set('fragment', kind);
    if (selected) {
      // The server supplies a path per modem; retain the matching alias rather
      // than treating a tty number or list position as a hardware identity.
      endpoint.searchParams.set('detail', selected.dataset.activityPath || paths(selected)[0]);
      if (chartsOpen) endpoint.searchParams.set('chart', chart);
    }
    try {
      const response = await fetch(endpoint, {credentials:'same-origin', cache:'no-store', redirect:'error', signal:controller.signal});
      if (!response.ok) throw new Error('Observations unavailable');
      const parsed = new DOMParser().parseFromString(await response.text(), 'text/html');
      if (id !== requestID || stopped) return;
      if (kind === 'inventory') {
        const next = parsed.querySelector('[data-hardware-inventory]');
        if (!next) throw new Error('Hardware inventory unavailable');
        rememberSections();
        const previousPaths = paths(selected);
        const previousDetails = detailRow(selected)?.firstElementChild.cloneNode(true);
        inventory().replaceWith(next);
        selected = [...next.querySelectorAll('.hardware-device')].find(row => row.querySelector('[data-hardware-toggle]') && paths(row).some(path => previousPaths.includes(path)));
        if (selected) { selected.querySelector('[data-hardware-toggle]').setAttribute('aria-expanded','true'); selected.classList.add('selected'); detailRow(selected).hidden = false; if (previousDetails) detailRow(selected).firstElementChild.replaceWith(previousDetails); }
        const refreshed = document.querySelector('[data-hardware-refreshed]');
        refreshed.hidden = false; refreshed.textContent = 'Hardware refreshed just now';
        if (paused) unavailable('Activity paused · previous observations');
      } else {
        const next = parsed.querySelector('[data-hardware-updates]');
        if (!next) throw new Error('Activity unavailable');
        // Remember the actual operating/saved alias for the selected physical row.
        for (const row of inventory().querySelectorAll('.hardware-device')) {
          const match = [...next.querySelectorAll('template[data-hardware-path]')].find(template => paths(row).includes(template.dataset.hardwarePath));
          if (match) row.dataset.activityPath = match.dataset.hardwarePath;
        }
        applyActivity(next);
      }
    } catch (error) {
      if (id === requestID && !stopped) unavailable(kind === 'inventory' ? 'Hardware refresh failed · previous inventory retained' : 'Activity updates unavailable · previous reports retained');
    } finally {
      clearTimeout(timeout);
      if (id === requestID) { pending = null; if (kind === 'inventory' && selected && !paused) load(); else schedule(); }
    }
  }
  document.addEventListener('click', event => {
    const button = event.target.closest('[data-hardware-toggle]');
    if (!button) return;
    const row = button.closest('.hardware-device'), closing = row === selected;
    if (selected) { selected.classList.remove('selected'); selected.querySelector('[data-hardware-toggle]').setAttribute('aria-expanded','false'); detailRow(selected).hidden = true; }
    selected = closing ? undefined : row;
    if (!selected) { pending?.abort(); ++requestID; pending = null; schedule(); return; }
    selected.classList.add('selected'); button.setAttribute('aria-expanded','true');
    const target = detailRow(selected); target.hidden = false;
    target.firstElementChild.textContent = 'Loading activity…';
    if (paused) { target.firstElementChild.textContent = 'Activity paused. Resume activity to load reports.'; return; }
    load();
  });
  document.addEventListener('toggle', event => {
    if (!selected || !detailRow(selected)?.contains(event.target)) return;
    const section = event.target.dataset.hardwareSection;
    if (section === 'charts' && chartsOpen !== event.target.open) { chartsOpen = event.target.open; if (chartsOpen && !paused) load(); }
    if (section === 'diagnostics') diagnosticsOpen = event.target.open;
  }, true);
  document.addEventListener('change', event => {
    if (event.target.matches('[data-hardware-chart]')) { chart = event.target.value; if (!paused) load(); }
  });
  refreshHardware.addEventListener('click', event => { event.preventDefault(); load('inventory'); });
  pause.addEventListener('click', () => {
    paused = !paused; pause.textContent = paused ? 'Resume activity' : 'Pause activity';
    if (paused) { clearTimeout(timer); pending?.abort(); ++requestID; pending = null; unavailable('Activity paused · previous observations'); }
    else load();
  });
  document.addEventListener('visibilitychange', () => {
    clearTimeout(timer);
    if (document.hidden) { pending?.abort(); ++requestID; pending = null; unavailable('Activity updates suspended'); }
    else if (!paused) load();
  });
  // Do not leave an old live indication visible during slow or failed requests.
  const ageTimer = setInterval(() => {
    inventory()?.querySelectorAll('[data-hardware-current]').forEach(cell => {
      if (cell.dataset.hardwareCurrent && Date.now() - Date.parse(cell.dataset.hardwareCurrent) > 3000) cell.textContent = 'Status unavailable';
    });
    inventory()?.querySelectorAll('[data-hardware-observed]').forEach(group => {
      if (group.dataset.hardwareObserved && Date.now() - Date.parse(group.dataset.hardwareObserved) > 3000) group.querySelectorAll('[data-hardware-live-state]').forEach(cell => { cell.textContent = 'Unavailable'; });
    });
  }, 1000);
  window.addEventListener('pagehide', () => { stopped = true; clearTimeout(timer); clearInterval(ageTimer); pending?.abort(); });
  schedule();
})();
