// Static export of the real app's UI CHROME for design review.
//
// This reproduces the interaction design (dropdowns, mode switch, chunk
// selection with mouse+keyboard, the right-click menu, search, the info
// panel) using fake/local data only -- there is no map engine, no backend,
// no network call anywhere in this file. Where a real handler would call the
// API or reproject real coordinates, a comment says so and this does
// something plausible-looking instead. The goal is to let another AI judge
// and iterate on the UI/interaction design, not the map rendering.
//
// Real source this mirrors (for the AI picking this up):
//   frontend/src/main.ts            -- wiring, keyboard shortcuts, selection logic
//   frontend/src/controls/ui.ts     -- the Dropdown/SelectionPanel/InfoPanel/ContextMenu components
//   frontend/src/controls/icons.ts  -- the pixel-icon generator these icons come from
//   frontend/src/layers/selection.ts -- the real (canvas-drawn) chunk highlight layer

(() => {
  'use strict';

  // ---------------------------------------------------------------------
  // Pixel icons -- identical generator to frontend/src/controls/icons.ts.
  // Edit the ASCII grids below to change a glyph; '#' is a filled pixel.
  // ---------------------------------------------------------------------

  function pixelIcon(art) {
    const h = art.length;
    const w = art[0]?.length ?? 0;
    let rects = '';
    for (let y = 0; y < h; y++) {
      for (let x = 0; x < w; x++) {
        if (art[y][x] !== '.') rects += `<rect x="${x}" y="${y}" width="1" height="1"/>`;
      }
    }
    return `<svg viewBox="0 0 ${w} ${h}" fill="currentColor" shape-rendering="crispEdges" aria-hidden="true">${rects}</svg>`;
  }

  function pixelIconMulti(art, palette) {
    const h = art.length;
    const w = art[0]?.length ?? 0;
    let rects = '';
    for (let y = 0; y < h; y++) {
      for (let x = 0; x < w; x++) {
        const ch = art[y][x];
        if (ch === '.') continue;
        const color = palette[ch];
        if (!color) continue;
        rects += `<rect x="${x}" y="${y}" width="1" height="1" fill="${color}"/>`;
      }
    }
    return `<svg viewBox="0 0 ${w} ${h}" shape-rendering="crispEdges" aria-hidden="true">${rects}</svg>`;
  }

  const ICON_PLUS = pixelIcon(['.......', '...#...', '...#...', '.#####.', '...#...', '...#...', '.......']);
  const ICON_MINUS = pixelIcon(['.......', '.......', '.......', '.#####.', '.......', '.......', '.......']);
  const ICON_HOME = pixelIcon(['...#...', '..###..', '.#####.', '#######', '#.###.#', '#.###.#', '#######']);
  const ICON_FULLSCREEN = pixelIcon(['##...##', '#.....#', '.......', '.......', '.......', '#.....#', '##...##']);
  const ICON_SELECT = pixelIcon(['.#.#.#.', '.......', '#.....#', '.......', '#.....#', '.......', '.#.#.#.']);
  const ICON_CLOSE = pixelIcon(['#.....#', '.#...#.', '..#.#..', '...#...', '..#.#..', '.#...#.', '#.....#']);
  const ICON_CARET_DOWN = pixelIcon(['#####', '.###.', '..#..']);
  const ICON_GRASS_BLOCK = pixelIconMulti(
    ['gggggggg', 'gggggggg', 'gGgggGgg', 'ddddDddd', 'ddDdddDd', 'dddddDdd', 'dDdddddd', 'ddddDddd'],
    { g: '#6fcf6f', G: '#4fae4f', d: '#9a6a3d', D: '#7c5230' },
  );

  const $ = (id) => document.getElementById(id);

  $('brandMark').innerHTML = ICON_GRASS_BLOCK;
  $('zoomIn').innerHTML = ICON_PLUS;
  $('zoomOut').innerHTML = ICON_MINUS;
  $('homeBtn').innerHTML = ICON_HOME;
  $('selectBtn').innerHTML = ICON_SELECT;
  $('fullscreenBtn').innerHTML = ICON_FULLSCREEN;
  $('infoCloseBtn').innerHTML = ICON_CLOSE;
  $('dimCaret').innerHTML = ICON_CARET_DOWN;
  $('layersCaret').innerHTML = ICON_CARET_DOWN;

  // ---------------------------------------------------------------------
  // Dropdowns (dimension selector, layers menu) -- closes on outside
  // click/Escape. Mirrors ui.ts's Dropdown class.
  // ---------------------------------------------------------------------

  function wireDropdown(rootId, btnId, panelId) {
    const root = $(rootId);
    const btn = $(btnId);
    const panel = $(panelId);
    function setOpen(open) {
      panel.hidden = !open;
      root.classList.toggle('is-open', open);
    }
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      setOpen(panel.hidden);
    });
    document.addEventListener('click', (e) => {
      if (!panel.hidden && !root.contains(e.target)) setOpen(false);
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && !panel.hidden) setOpen(false);
    });
    return { setOpen };
  }

  const dimDropdown = wireDropdown('dimDropdown', 'dimDropdownBtn', 'dimDropdownPanel');
  $('dimDropdownPanel').querySelectorAll('.mm-item').forEach((item) => {
    item.addEventListener('click', () => {
      $('dimDropdownPanel').querySelectorAll('.mm-item').forEach((i) => i.classList.remove('is-active'));
      item.classList.add('is-active');
      $('dimDropdownBtn').querySelector('.mm-dropdown-label').textContent = item.dataset.dim;
      dimDropdown.setOpen(false);
      // Real app: engine.setDimension(d) -- reloads tiles for the new
      // dimension and re-centres on its spawn. No map engine here.
    });
  });

  wireDropdown('layersDropdown', 'layersDropdownBtn', 'layersDropdownPanel');
  $('layersDropdownPanel').querySelectorAll('input[data-layer]').forEach((input) => {
    input.addEventListener('change', () => {
      if (input.dataset.layer === 'chunkGrid') setChunkGridVisible(input.checked);
      // Every other layer toggle just flips a real overlay's visibility in
      // the live app (players, claims, markers, ...) -- nothing to show here.
    });
  });

  // ---------------------------------------------------------------------
  // Mode switch (2D / ISO) -- UI state only; the real switch reprojects the
  // whole map between a top-down and isometric projection, which needs an
  // actual map engine to demonstrate.
  // ---------------------------------------------------------------------

  $('modeSwitch').querySelectorAll('.mm-seg-btn').forEach((btn) => {
    btn.addEventListener('click', () => {
      $('modeSwitch').querySelectorAll('.mm-seg-btn').forEach((b) => b.classList.remove('is-active'));
      btn.classList.add('is-active');
    });
  });

  // ---------------------------------------------------------------------
  // Fullscreen -- this one's real, the Fullscreen API works standalone.
  // ---------------------------------------------------------------------

  $('fullscreenBtn').addEventListener('click', () => {
    if (document.fullscreenElement) document.exitFullscreen();
    else document.documentElement.requestFullscreen?.();
  });

  // ---------------------------------------------------------------------
  // The fake map: a CSS placeholder for OpenLayers + real terrain tiles.
  // Chunks are simple 64px grid cells; "block coordinates" below are a
  // plausible-looking fabrication (pixel position x4), not real projection
  // math -- see coordinates/mc.ts and map/engine.ts for the real thing.
  // ---------------------------------------------------------------------

  const CELL = 64;
  const fakeMap = $('fakeMap');
  const fakeTerrain = $('fakeTerrain');
  const fakeGrid = $('fakeGrid');
  let panX = 0;
  let panY = 0;
  let zoomLevel = 4;

  function applyTerrainTransform() {
    fakeTerrain.style.backgroundPosition = `${panX}px ${panY}px`;
    fakeGrid.style.backgroundPosition = `${panX}px ${panY}px`;
  }

  function cellKey(cx, cz) {
    return `${cx},${cz}`;
  }

  function pixelToCell(px, py) {
    return [Math.floor((px - panX) / CELL), Math.floor((py - panY) / CELL)];
  }

  function cellRect(cx, cz) {
    return { left: cx * CELL + panX, top: cz * CELL + panY };
  }

  const selected = new Map();
  let hoverEl = null;
  let lastClickedCell = null;

  function renderCell(cx, cz, kind) {
    const el = document.createElement('div');
    el.className = `mm-cell is-${kind}`;
    const { left, top } = cellRect(cx, cz);
    el.style.left = `${left}px`;
    el.style.top = `${top}px`;
    fakeMap.appendChild(el);
    return el;
  }

  function setHover(cx, cz) {
    if (hoverEl) hoverEl.remove();
    hoverEl = renderCell(cx, cz, 'hover');
  }

  function clearHover() {
    if (hoverEl) hoverEl.remove();
    hoverEl = null;
  }

  function toggleCell(cx, cz) {
    const k = cellKey(cx, cz);
    const existing = selected.get(k);
    if (existing) {
      existing.remove();
      selected.delete(k);
    } else {
      selected.set(k, renderCell(cx, cz, 'selected'));
    }
    updateSelectionCount();
  }

  function addRange(minCX, minCZ, maxCX, maxCZ) {
    for (let z = minCZ; z <= maxCZ; z++) {
      for (let x = minCX; x <= maxCX; x++) {
        const k = cellKey(x, z);
        if (!selected.has(k)) selected.set(k, renderCell(x, z, 'selected'));
      }
    }
    updateSelectionCount();
  }

  function clearSelection() {
    selected.forEach((el) => el.remove());
    selected.clear();
    updateSelectionCount();
  }

  function updateSelectionCount() {
    const n = selected.size;
    $('selectionCount').textContent = n === 1 ? '1 chunk selected' : `${n} chunks selected`;
    const disabled = n === 0;
    ['claimBtn', 'unclaimBtn', 'forceLoadBtn', 'unloadBtn'].forEach((id) => ($(id).disabled = disabled));
  }
  updateSelectionCount();

  // ---- Selection mode -----------------------------------------------------

  let selectionMode = false;

  function setSelectionMode(on) {
    selectionMode = on;
    $('selectBtn').classList.toggle('is-active', on);
    fakeMap.classList.toggle('mm-selecting', on);
    $('selectionPanel').hidden = !on;
    if (!on) {
      clearHover();
      lastClickedCell = null;
    }
  }

  $('selectBtn').addEventListener('click', () => setSelectionMode(!selectionMode));

  function ensureCellSelected(cx, cz) {
    if (!selectionMode) setSelectionMode(true);
    if (!selected.has(cellKey(cx, cz))) toggleCell(cx, cz);
  }

  // ---- Panel actions (demo only -- no backend to call) --------------------

  function flashSelectionMessage(text, isError) {
    const el = $('selectionError');
    el.textContent = text;
    el.hidden = false;
    el.style.color = isError ? 'var(--offline)' : 'var(--live)';
    window.setTimeout(() => {
      el.hidden = true;
    }, 1800);
  }

  function demoAction(verb) {
    const n = selected.size;
    if (n === 0) return;
    flashSelectionMessage(`${verb} ${n} chunk${n === 1 ? '' : 's'} (demo -- no backend here)`, false);
    clearSelection();
  }

  $('claimBtn').addEventListener('click', () => demoAction('Claimed'));
  $('unclaimBtn').addEventListener('click', () => demoAction('Unclaimed'));
  $('forceLoadBtn').addEventListener('click', () => demoAction('Force-loaded'));
  $('unloadBtn').addEventListener('click', () => demoAction('Unloaded'));
  $('clearSelectionBtn').addEventListener('click', () => {
    clearSelection();
    $('selectionError').hidden = true;
  });

  // ---- Info panel (demo data -- the real panel shows a server "pick") -----

  const FAKE_BLOCKS = ['minecraft:grass_block', 'minecraft:stone', 'minecraft:oak_log', 'minecraft:sand', 'minecraft:water'];
  const FAKE_BIOMES = ['minecraft:plains', 'minecraft:forest', 'minecraft:desert', 'minecraft:ocean'];

  function prettyId(id) {
    const local = id.includes(':') ? id.slice(id.indexOf(':') + 1) : id;
    return local
      .split('_')
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(' ');
  }

  function showInfoPanel(bx, bz) {
    const block = FAKE_BLOCKS[Math.abs(bx + bz) % FAKE_BLOCKS.length];
    const biome = FAKE_BIOMES[Math.abs(bx - bz) % FAKE_BIOMES.length];
    const y = 64 + (Math.abs(bx * 3 + bz) % 20);
    const dl = document.createElement('dl');
    dl.className = 'mm-info-list';
    const rows = [
      ['Position', `${bx}, ${y}, ${bz}`],
      ['Chunk', `${Math.floor(bx / 16)}, ${Math.floor(bz / 16)}`],
      ['Block', prettyId(block)],
      ['Biome', prettyId(biome)],
      ['Light', '15'],
    ];
    rows.forEach(([k, v]) => {
      const dt = document.createElement('dt');
      dt.textContent = k;
      const dd = document.createElement('dd');
      dd.textContent = v;
      dl.append(dt, dd);
    });
    $('infoBody').replaceChildren(dl);
    $('infoPanel').hidden = false;
  }

  $('infoCloseBtn').addEventListener('click', () => {
    $('infoPanel').hidden = true;
  });

  // ---- Context menu ---------------------------------------------------

  const contextMenu = $('contextMenu');

  function closeContextMenu() {
    contextMenu.hidden = true;
  }

  document.addEventListener('pointerdown', (e) => {
    if (!contextMenu.hidden && !contextMenu.contains(e.target)) closeContextMenu();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !contextMenu.hidden) closeContextMenu();
  });

  function showContextMenu(clientX, clientY, cx, cz, bx, bz) {
    contextMenu.replaceChildren();
    const chunkSelected = selected.has(cellKey(cx, cz));

    function addItem(label, onSelect, disabled) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'mm-item mm-contextmenu-item';
      btn.textContent = label;
      btn.disabled = !!disabled;
      btn.addEventListener('click', () => {
        closeContextMenu();
        onSelect();
      });
      contextMenu.appendChild(btn);
    }
    function addSep() {
      const sep = document.createElement('div');
      sep.className = 'mm-contextmenu-sep';
      contextMenu.appendChild(sep);
    }

    addItem('Copy coordinates', () => navigator.clipboard?.writeText(`${bx}, ${bz}`));
    addItem('Show block info', () => showInfoPanel(bx, bz));
    addItem('Center map here', () => {
      /* real app: engine.centerOnBlock(bx, bz) */
    });
    addSep();
    addItem(chunkSelected ? 'Remove chunk from selection' : 'Add chunk to selection', () => ensureCellSelected(cx, cz));
    addItem('Claim', () => {
      ensureCellSelected(cx, cz);
      demoAction('Claimed');
    });
    addItem('Unclaim', () => {
      ensureCellSelected(cx, cz);
      demoAction('Unclaimed');
    });
    addItem('Force load', () => {
      ensureCellSelected(cx, cz);
      demoAction('Force-loaded');
    });
    addItem('Unload', () => {
      ensureCellSelected(cx, cz);
      demoAction('Unloaded');
    });
    addSep();
    addItem('Clear selection', () => clearSelection(), selected.size === 0);

    contextMenu.hidden = false;
    const { width, height } = contextMenu.getBoundingClientRect();
    contextMenu.style.left = `${Math.max(4, Math.min(clientX, window.innerWidth - width - 4))}px`;
    contextMenu.style.top = `${Math.max(4, Math.min(clientY, window.innerHeight - height - 4))}px`;
  }

  // ---------------------------------------------------------------------
  // Map pointer interactions: click / ctrl+click / shift+click / drag-box
  // (both shift+left-drag and plain right-drag) / right-click for the menu.
  // Mirrors main.ts's engine.on('click', ...) and the two DragBox
  // interactions almost exactly, just against fake cell math instead of the
  // real projection.
  // ---------------------------------------------------------------------

  fakeMap.addEventListener('contextmenu', (e) => e.preventDefault());

  // Every left or right press is tracked from pointerdown so a click and a
  // drag of the *same* button can be told apart at pointerup by movement
  // distance alone -- gating on shiftKey up front (as an earlier version of
  // this file did) breaks a plain shift+click that never moves, since a
  // press that starts a drag-box conversation can still end as a click.
  let pressButton = null;
  let pressStart = null;
  let pressDragged = false;
  const DRAG_THRESHOLD = 8;

  fakeMap.addEventListener('pointerdown', (e) => {
    if (e.button !== 0 && e.button !== 2) return;
    pressButton = e.button;
    pressStart = { x: e.clientX, y: e.clientY };
    pressDragged = false;
  });

  fakeMap.addEventListener('pointermove', (e) => {
    const rect = fakeMap.getBoundingClientRect();
    const px = e.clientX - rect.left;
    const py = e.clientY - rect.top;
    const [bx, bz] = [Math.round((px - rect.width / 2) * 4), Math.round((py - rect.height / 2) * 4)];
    $('statusCoords').textContent = `X: ${bx}   Z: ${bz}`;
    const [cx, cz] = pixelToCell(px, py);
    $('statusChunk').textContent = `Chunk: ${cx}, ${cz}`;

    if (selectionMode && pressButton === null) setHover(cx, cz);

    if (pressStart) {
      const dx = e.clientX - pressStart.x;
      const dy = e.clientY - pressStart.y;
      if (dx * dx + dy * dy >= DRAG_THRESHOLD * DRAG_THRESHOLD) pressDragged = true;
    }
  });

  fakeMap.addEventListener('pointerleave', clearHover);

  fakeMap.addEventListener('pointerup', (e) => {
    const rect = fakeMap.getBoundingClientRect();
    const px = e.clientX - rect.left;
    const py = e.clientY - rect.top;
    const [cx, cz] = pixelToCell(px, py);
    const bx = Math.round((px - rect.width / 2) * 4);
    const bz = Math.round((py - rect.height / 2) * 4);

    if (pressButton !== null && pressDragged && (pressButton === 2 || e.shiftKey)) {
      // A real drag with the right button, or shift+left -- fill the
      // rectangle between down and up, same as main.ts's applyBoxSelection().
      if (!selectionMode) setSelectionMode(true);
      const [scx, scz] = pixelToCell(pressStart.x - rect.left, pressStart.y - rect.top);
      addRange(Math.min(scx, cx), Math.min(scz, cz), Math.max(scx, cx), Math.max(scz, cz));
    } else if (pressButton === 2 && !pressDragged) {
      // A right press that didn't turn into a drag -- the context menu.
      showContextMenu(e.clientX, e.clientY, cx, cz, bx, bz);
    } else if (pressButton === 0 && !pressDragged) {
      const wantsSelect = selectionMode || e.ctrlKey || e.metaKey;
      if (wantsSelect) {
        if (!selectionMode) setSelectionMode(true);
        if (e.shiftKey && lastClickedCell) {
          addRange(
            Math.min(lastClickedCell[0], cx),
            Math.min(lastClickedCell[1], cz),
            Math.max(lastClickedCell[0], cx),
            Math.max(lastClickedCell[1], cz),
          );
        } else {
          toggleCell(cx, cz);
        }
        lastClickedCell = [cx, cz];
      } else {
        showInfoPanel(bx, bz);
      }
    }

    pressButton = null;
    pressStart = null;
    pressDragged = false;
  });

  // ---------------------------------------------------------------------
  // Search -- a fixed fake result list, filtered client-side. The real
  // search hits /api/search on the backend.
  // ---------------------------------------------------------------------

  const FAKE_RESULTS = [
    { type: 'player', name: 'Notch', x: 120, z: -45 },
    { type: 'player', name: 'jeb_', x: -900, z: 240 },
    { type: 'warp', name: 'spawn', x: 0, z: 0 },
    { type: 'warp', name: 'mining base', x: 512, z: 512 },
  ];

  $('searchInput').addEventListener('input', (e) => {
    const q = e.target.value.trim().toLowerCase();
    const results = $('searchResults');
    if (!q) {
      results.hidden = true;
      return;
    }
    const coordMatch = q.match(/^(-?\d+)[\s,]+(-?\d+)$/);
    const matches = coordMatch
      ? [{ type: 'coordinates', name: `${coordMatch[1]}, ${coordMatch[2]}`, x: Number(coordMatch[1]), z: Number(coordMatch[2]) }]
      : FAKE_RESULTS.filter((r) => r.name.toLowerCase().includes(q));

    results.replaceChildren();
    if (matches.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'mm-search-empty';
      empty.textContent = 'No matches';
      results.appendChild(empty);
    } else {
      matches.forEach((r) => {
        const row = document.createElement('button');
        row.type = 'button';
        row.className = 'mm-search-row';
        row.innerHTML = `<span class="mm-search-type">${r.type}</span><span class="mm-search-name">${r.name}</span><span class="mm-search-coords mm-mono">${r.x}, ${r.z}</span>`;
        row.addEventListener('click', () => {
          results.hidden = true;
          $('searchInput').value = r.name;
        });
        results.appendChild(row);
      });
    }
    results.hidden = false;
  });

  document.addEventListener('click', (e) => {
    if (!$('search').contains(e.target)) $('searchResults').hidden = true;
  });

  // ---------------------------------------------------------------------
  // Zoom controls -- purely cosmetic here (updates the status text and a
  // fake CSS scale); the real controls change the map engine's resolution.
  // ---------------------------------------------------------------------

  function setZoom(z) {
    zoomLevel = Math.max(0, Math.min(13, z));
    const ppb = zoomLevel; // stand-in only; real formula is 2^zoom based
    $('statusZoom').textContent = `Zoom ${zoomLevel.toFixed(2)}   ${Math.max(1, ppb)} blocks/px`;
  }
  setZoom(4);

  $('zoomIn').addEventListener('click', () => setZoom(zoomLevel + 1));
  $('zoomOut').addEventListener('click', () => setZoom(zoomLevel - 1));
  $('homeBtn').addEventListener('click', () => {
    panX = 0;
    panY = 0;
    applyTerrainTransform();
  });

  // ---------------------------------------------------------------------
  // Chunk grid overlay toggle, shared by the Layers checkbox and the 'g'
  // keyboard shortcut -- same "single source of truth" pattern as main.ts.
  // ---------------------------------------------------------------------

  function setChunkGridVisible(on) {
    fakeGrid.classList.toggle('is-visible', on);
    const checkbox = document.querySelector('input[data-layer="chunkGrid"]');
    if (checkbox.checked !== on) checkbox.checked = on;
  }

  // ---------------------------------------------------------------------
  // Keyboard shortcuts -- mirrors main.ts's keydown handler, including the
  // "no modifier keys" guard on the new letter shortcuts so they never fight
  // a browser/OS combination sharing the same key.
  // ---------------------------------------------------------------------

  function noModifiers(e) {
    return !e.ctrlKey && !e.metaKey && !e.altKey;
  }

  const SELECTION_KEY_ACTIONS = {
    c: () => demoAction('Claimed'),
    u: () => demoAction('Unclaimed'),
    f: () => demoAction('Force-loaded'),
    l: () => demoAction('Unloaded'),
  };

  document.addEventListener('keydown', (e) => {
    if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;

    if (e.key === '/') {
      e.preventDefault();
      $('searchInput').focus();
    } else if (e.key === 'g') {
      setChunkGridVisible(!fakeGrid.classList.contains('is-visible'));
    } else if (e.key === 'm') {
      const buttons = [...$('modeSwitch').querySelectorAll('.mm-seg-btn')];
      const next = buttons.find((b) => !b.classList.contains('is-active'));
      if (next) next.click();
    } else if (e.key === 'Escape') {
      if (selectionMode && selected.size > 0) clearSelection();
      else if (selectionMode) setSelectionMode(false);
      else $('infoPanel').hidden = true;
    } else if (noModifiers(e) && e.key.toLowerCase() === 's') {
      setSelectionMode(!selectionMode);
    } else if (selectionMode && noModifiers(e) && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      if (!selectionMode) setSelectionMode(true);
      const rect = fakeMap.getBoundingClientRect();
      const [minCX, minCZ] = pixelToCell(0, 0);
      const [maxCX, maxCZ] = pixelToCell(rect.width, rect.height);
      addRange(minCX, minCZ, maxCX, maxCZ);
    } else if (selectionMode && noModifiers(e) && selected.size > 0) {
      const action = SELECTION_KEY_ACTIONS[e.key.toLowerCase()];
      if (action) action();
    }
  });

  applyTerrainTransform();
})();
