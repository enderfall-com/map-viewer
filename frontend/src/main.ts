import 'ol/ol.css';
import './styles/main.css';

import DragBox from 'ol/interaction/DragBox';
import { shiftKeyOnly } from 'ol/events/condition';
import type { Extent } from 'ol/extent';

import { ApiClient } from './api/client';
import { RealtimeSocket, type ConnectionState } from './api/socket';
import type { ClientConfig, DimensionInfo, PickResult, SearchResult } from './api/types';
import { MapEngine, type MapMode } from './map/engine';
import { blockToChunk } from './coordinates/mc';
import { UrlState } from './map/urlstate';
import { GridLayer } from './layers/grid';
import { FeatureLayers } from './layers/features';
import { PlayerLayer } from './layers/players';
import { WorldBorderLayer } from './layers/worldborder';
import { SelectionLayer, type ChunkKey } from './layers/selection';
import {
  ICON_FLAT,
  ICON_FULLSCREEN,
  ICON_GRASS_BLOCK,
  ICON_HOME,
  ICON_ISO,
  ICON_LAYERS,
  ICON_MINUS,
  ICON_PLUS,
  ICON_SELECT,
} from './controls/icons';
import {
  ClaimsLegend,
  ContextMenu,
  DebugHud,
  DimensionMenu,
  Hotbar,
  InfoPanel,
  LayersPopover,
  SearchBar,
  SelectionPanel,
  el,
  type HotbarSlot,
  type LayerToggleDef,
} from './controls/ui';

/** The largest square a single drag selection may span on one axis. Matches
 * the backend's maxChunkSelection (a 12x12 chunk square, 144 total) -- a
 * drag is clamped to this live, at the anchor corner, rather than growing
 * past it and only failing once released. */
const MAX_DRAG_CHUNKS = 12;
const MAX_CHUNK_SELECTION = MAX_DRAG_CHUNKS * MAX_DRAG_CHUNKS;

/** Minimum zoom when jumping to a followed player -- close enough to
 * actually make out their model, not just land somewhere in view. */
const FOLLOW_ZOOM = 8;

/** True when none of Ctrl/Cmd/Alt are held, so a bare-letter shortcut never
 * fights a browser or OS combination that happens to share the same key
 * (Ctrl+F find, Ctrl+S save, Ctrl+A select-all, and so on). */
function noModifiers(e: KeyboardEvent): boolean {
  return !e.ctrlKey && !e.metaKey && !e.altKey;
}

/** Rounds a raw block count to a "nice" 1/2/5 x 10^n value, the way a map
 * scale bar's label conventionally does, rather than showing an oddly
 * specific number that changes on every fractional zoom tick. */
function niceScale(blocks: number): number {
  if (blocks <= 0) return 1;
  const exp = Math.floor(Math.log10(blocks));
  const base = Math.pow(10, exp);
  const frac = blocks / base;
  const nice = frac < 1.5 ? 1 : frac < 3.5 ? 2 : frac < 7.5 ? 5 : 10;
  return nice * base;
}

/**
 * Application entry point.
 *
 * Wiring order matters: configuration first, because it decides the zoom
 * ladder, the dimension list and every overlay threshold; then the engine; then
 * overlays and controls, which only observe the engine.
 */
async function boot(): Promise<void> {
  const root = document.getElementById('app');
  if (!root) throw new Error('#app missing');

  const api = new ApiClient();
  let config: ClientConfig;
  try {
    config = await api.config();
  } catch (err) {
    showFatal(root, err);
    return;
  }
  if (!config.dimensions.length) {
    showFatal(root, new Error('The server reports no enabled dimensions.'));
    return;
  }

  const urlState = new UrlState();
  const initial = urlState.read();

  const dimension =
    config.dimensions.find((d) => d.id === initial.dimension) ??
    config.dimensions.find((d) => d.id === config.defaultDimension) ??
    config.dimensions[0];

  // ---- Layout -------------------------------------------------------------
  //
  // Every control lives in one absolutely positioned CSS grid over the full-
  // bleed map (see .mm-chrome in main.css) rather than as layout siblings
  // that would shrink it -- the grid itself is pointer-events:none so gaps
  // between panels still hit the map, and each real panel opts back into
  // pointer-events via .mm-panel.

  const shell = el('div', 'mm-shell');
  const mapHost = el('div', 'mm-map');
  const chrome = el('div', 'mm-chrome');
  mapHost.appendChild(chrome);
  shell.appendChild(mapHost);
  root.replaceChildren(shell);

  // ---- Engine -------------------------------------------------------------

  const engine = new MapEngine(mapHost, config, api, dimension);

  const grid = new GridLayer(engine);
  const features = new FeatureLayers(engine);
  const players = new PlayerLayer(engine);
  const border = new WorldBorderLayer(engine);
  const selection = new SelectionLayer(engine, api);

  engine.map.addLayer(border.layer);
  engine.map.addLayer(features.areas);
  engine.map.addLayer(grid);
  engine.map.addLayer(selection);
  engine.map.addLayer(features.markers);
  engine.map.addLayer(players.layer);

  // Restore whatever the URL specified.
  if (initial.zoom !== undefined) engine.setZoom(initial.zoom);
  if (initial.x !== undefined && initial.z !== undefined) {
    engine.centerOnBlock(initial.x, initial.z, undefined, false);
  }
  if (initial.style) engine.setStyle(initial.style);

  // ---- Debug HUD (1,1) -----------------------------------------------------

  const debugHud = new DebugHud(ICON_GRASS_BLOCK, config.title || 'Minecraft Map');
  debugHud.setDimension(dimension.id);
  chrome.appendChild(debugHud.root);

  // ---- Search (2,1) ---------------------------------------------------------

  const searchBar = new SearchBar();
  chrome.appendChild(searchBar.root);

  let loadingTimer: number | null = null;
  function flashLoading(): void {
    if (loadingTimer !== null) window.clearTimeout(loadingTimer);
    searchBar.setLoading(true);
    loadingTimer = window.setTimeout(() => {
      loadingTimer = null;
      searchBar.setLoading(false);
    }, 650);
  }

  function refreshJumpTargets(): void {
    const d = engine.getDimension();
    const items: SearchResult[] = [
      { type: 'jumpto', id: 'spawn', name: 'Spawn', dimension: d.id, x: d.spawnX, z: d.spawnZ },
      ...players.list().map((p) => ({
        type: 'jumpto',
        id: p.uuid,
        name: p.name,
        dimension: p.dimension,
        x: p.x,
        z: p.z,
      })),
    ];
    searchBar.setJumpToSuggestions(items);
  }
  refreshJumpTargets();

  // ---- Dimension + fullscreen (3,1) -----------------------------------------

  const dimWrap = el('div', 'mm-dim-wrap');
  const dimensionMenu = new DimensionMenu(config.dimensions, dimension, ICON_GRASS_BLOCK, dimWrap);
  const fullscreenBtn = el('button', 'mm-full-btn');
  fullscreenBtn.innerHTML = ICON_FULLSCREEN;
  fullscreenBtn.type = 'button';
  fullscreenBtn.title = 'Fullscreen';
  dimWrap.append(dimensionMenu.button, fullscreenBtn, dimensionMenu.panel);
  chrome.appendChild(dimWrap);

  // ---- Left column: selection panel + claims legend --------------------
  //
  // Kept instantiated but deliberately never mounted into `chrome` -- the
  // selection count/owner/action panel and the claims colour legend were
  // both removed from the visible UI (claim/unclaim/force-load/unload are
  // still reachable via the right-click context menu and the C/U/F/L
  // shortcuts). Every call into either object below is therefore acting on
  // a detached, invisible node, which is harmless.
  const selectionPanel = new SelectionPanel();
  const claimsLegend = new ClaimsLegend();

  // ---- Layers popover (2,2) --------------------------------------------

  const overlayState = {
    players: true,
    claims: true,
    regions: true,
    markers: true,
    forceLoaded: true,
    chunkGrid: false,
    blockGrid: false,
    worldBorder: false,
  };

  const layerDefs: Array<LayerToggleDef & { key: keyof typeof overlayState }> = [
    { key: 'players', name: 'Players', isOn: () => overlayState.players, onToggle: () => {
      overlayState.players = !overlayState.players;
      players.setVisible(overlayState.players);
    } },
    { key: 'claims', name: 'Claims', isOn: () => overlayState.claims, onToggle: () => {
      overlayState.claims = !overlayState.claims;
      features.setClaimsVisible(overlayState.claims);
      claimsLegend.setVisible(overlayState.claims);
    } },
    { key: 'regions', name: 'Regions', isOn: () => overlayState.regions, onToggle: () => {
      overlayState.regions = !overlayState.regions;
      features.setRegionsVisible(overlayState.regions);
    } },
    { key: 'markers', name: 'Markers', isOn: () => overlayState.markers, onToggle: () => {
      overlayState.markers = !overlayState.markers;
      features.setMarkersVisible(overlayState.markers);
    } },
    { key: 'forceLoaded', name: 'Force loaded', isOn: () => overlayState.forceLoaded, onToggle: () => {
      overlayState.forceLoaded = !overlayState.forceLoaded;
      features.setForceLoadedVisible(overlayState.forceLoaded);
    } },
    { key: 'chunkGrid', name: 'Chunk grid', isOn: () => overlayState.chunkGrid, onToggle: () => setChunkGrid(!overlayState.chunkGrid) },
    { key: 'blockGrid', name: 'Block grid', isOn: () => overlayState.blockGrid, onToggle: () => {
      overlayState.blockGrid = !overlayState.blockGrid;
      grid.setBlockGrid(overlayState.blockGrid);
    } },
    { key: 'worldBorder', name: 'World border', isOn: () => overlayState.worldBorder, onToggle: () => {
      overlayState.worldBorder = !overlayState.worldBorder;
      border.setVisible(overlayState.worldBorder);
    } },
  ];

  const layersPopover = new LayersPopover(layerDefs, chrome);
  chrome.appendChild(layersPopover.root);

  /** Single place that flips the chunk-grid flag, so the Layers toggle, the
   * 'g' shortcut and selection mode (which also shows the grid) never drift
   * out of sync about the grid's real state. */
  function setChunkGrid(on: boolean): void {
    overlayState.chunkGrid = on;
    grid.setChunkGrid(on);
    layersPopover.refresh();
  }

  // ---- Block info panel (3,2) ---------------------------------------------

  const infoPanel = new InfoPanel();
  chrome.appendChild(infoPanel.root);

  // ---- Chunk selection ------------------------------------------------------

  let selectionMode = false;
  // The chunk a plain click last landed on, so a following shift+click can
  // select the rectangle between the two -- the standard file-manager
  // range-select gesture, offered alongside the drag-box for a single quick
  // click-shift-click without needing to draw a box at all.
  let lastClickedChunk: ChunkKey | null = null;

  function setSelectionMode(on: boolean): void {
    selectionMode = on;
    mapHost.classList.toggle('mm-selecting', on);
    selectionPanel.setVisible(on);
    hotbar.refresh();
    if (!on) {
      selectionPanel.clearError();
      selection.setHover(null);
      lastClickedChunk = null;
    }
  }

  /** Adds `chunk` to the selection if it is not already there. Used by
   * gestures (right-click actions, keyboard shortcuts) that need something
   * selected to act on and should not silently no-op just because the user
   * hadn't clicked anything yet. */
  function ensureChunkSelected(chunk: ChunkKey): void {
    if (!selectionMode) setSelectionMode(true);
    if (!selection.has(chunk)) selection.toggle(chunk);
  }

  selection.onChange(() => selectionPanel.setCount(selection.count()));

  // Runs a claim/unclaim/force-load action, disabling the panel's buttons for
  // its duration so a slow response cannot let a double-click submit the same
  // action twice, and surfacing whatever error message it returns.
  async function runSelectionAction(action: () => Promise<string | null>): Promise<void> {
    selectionPanel.setBusy(true);
    selectionPanel.clearError();
    try {
      const errorMessage = await action();
      if (errorMessage) selectionPanel.showError(errorMessage);
    } catch {
      selectionPanel.showError('Request failed. Please try again.');
    } finally {
      selectionPanel.setBusy(false);
    }
  }

  // Named rather than inlined into setActions() below so the context menu
  // and keyboard shortcuts can trigger the exact same logic instead of a
  // separate copy that could drift from what the panel's own buttons do.
  function doClaim(): void {
    const n = selection.count();
    void runSelectionAction(async () => {
      const owner = selectionPanel.ownerName() || undefined;
      const res = await api.claimChunks(engine.getDimension().id, selection.list(), undefined, owner);
      if (res.ok) {
        selection.clear();
        selectionPanel.showFlash(`Claimed ${n} chunk${n === 1 ? '' : 's'}`);
        void refreshFeatures();
        return null;
      }
      // The conflicting chunks are known to still be selected -- they came
      // from this same request's selection.list() -- so toggling them is
      // safe as a targeted removal, leaving the clean chunks selected for
      // an immediate retry.
      res.conflicts?.forEach((c) => selection.toggle(c));
      return res.message;
    });
  }

  function doUnclaim(): void {
    const n = selection.count();
    void runSelectionAction(async () => {
      const res = await api.unclaimChunks(engine.getDimension().id, selection.list());
      if (!res.ok) return res.message;
      selection.clear();
      selectionPanel.showFlash(`Unclaimed ${n} chunk${n === 1 ? '' : 's'}`);
      void refreshFeatures();
      return null;
    });
  }

  function doForceLoad(): void {
    const n = selection.count();
    void runSelectionAction(async () => {
      const res = await api.setForceLoaded(engine.getDimension().id, selection.list(), true);
      if (!res.ok) return res.message;
      selection.clear();
      selectionPanel.showFlash(`Force-loaded ${n} chunk${n === 1 ? '' : 's'}`);
      void refreshFeatures();
      return null;
    });
  }

  function doUnload(): void {
    const n = selection.count();
    void runSelectionAction(async () => {
      const res = await api.setForceLoaded(engine.getDimension().id, selection.list(), false);
      if (!res.ok) return res.message;
      selection.clear();
      selectionPanel.showFlash(`Unloaded ${n} chunk${n === 1 ? '' : 's'}`);
      void refreshFeatures();
      return null;
    });
  }

  selectionPanel.setActions({
    onClaim: doClaim,
    onUnclaim: doUnclaim,
    onForceLoad: doForceLoad,
    onUnload: doUnload,
    onClear: () => {
      selection.clear();
      selectionPanel.clearError();
    },
  });

  // Typing a name and pressing Enter is the expected shorthand for a form
  // like this, rather than having to reach for the mouse to hit Claim.
  selectionPanel.onOwnerEnter(() => {
    if (!selection.isEmpty()) doClaim();
  });

  // Keyboard equivalents of the panel's own buttons, looked up by key in the
  // keydown handler below.
  const SELECTION_KEY_ACTIONS: Record<string, () => void> = {
    c: doClaim,
    u: doUnclaim,
    f: doForceLoad,
    l: doUnload,
  };

  /** Adds an inclusive chunk range to the selection, or shows an error
   * instead if it's larger than the backend will accept in one request. */
  function addChunkRangeChecked(minCX: number, minCZ: number, maxCX: number, maxCZ: number): void {
    const count = (maxCX - minCX + 1) * (maxCZ - minCZ + 1);
    if (count > MAX_CHUNK_SELECTION) {
      selectionPanel.setVisible(true);
      selectionPanel.showError(
        `That area covers ${count} chunks; one action can cover at most ${MAX_CHUNK_SELECTION}. Try a smaller area.`,
      );
      return;
    }
    selectionPanel.clearError();
    selection.addRange(minCX, minCZ, maxCX, maxCZ);
  }

  /**
   * Converts a drawn box's extent into an inclusive chunk range.
   *
   * Every corner is projected, not just two opposite ones: in isometric mode
   * the screen-space box maps to a rotated parallelogram in block space, and
   * its bounding box is only correct once every corner is accounted for.
   */
  function extentToChunkRange(extent: Extent): { minX: number; minZ: number; maxX: number; maxZ: number } {
    const corners: Array<[number, number]> = [
      [extent[0], extent[1]],
      [extent[2], extent[1]],
      [extent[0], extent[3]],
      [extent[2], extent[3]],
    ];
    let minBX = Infinity;
    let minBZ = Infinity;
    let maxBX = -Infinity;
    let maxBZ = -Infinity;
    for (const corner of corners) {
      const [bx, bz] = engine.viewToBlockApprox(corner);
      minBX = Math.min(minBX, bx);
      maxBX = Math.max(maxBX, bx);
      minBZ = Math.min(minBZ, bz);
      maxBZ = Math.max(maxBZ, bz);
    }
    return {
      minX: blockToChunk(minBX),
      minZ: blockToChunk(minBZ),
      maxX: blockToChunk(maxBX),
      maxZ: blockToChunk(maxBZ),
    };
  }

  /** The chunk the current drag started from, i.e. the corner that stays
   * fixed while the box grows -- set on boxstart, read by every boxdrag tick
   * and by the final boxend commit, cleared once the drag ends. */
  let dragAnchorChunk: { x: number; z: number } | null = null;

  /** Remembers a drag's starting chunk as the anchor {@link dragAnchorChunk}
   * clamping grows away from. A box just starting is a single point, so its
   * own chunk range names that chunk on both ends. */
  function rememberDragAnchor(box: DragBox): void {
    const r = extentToChunkRange(box.getGeometry().getExtent());
    dragAnchorChunk = { x: r.minX, z: r.minZ };
  }

  /**
   * Clamps a chunk range to at most {@link MAX_DRAG_CHUNKS} on each axis,
   * keeping `anchor` fixed -- the range only ever shrinks back toward
   * whichever corner the drag actually started from, rather than being cut
   * from an arbitrary edge.
   */
  function clampRangeToAnchor(
    range: { minX: number; minZ: number; maxX: number; maxZ: number },
    anchor: { x: number; z: number },
  ): { minX: number; minZ: number; maxX: number; maxZ: number } {
    let { minX, minZ, maxX, maxZ } = range;
    if (maxX - minX + 1 > MAX_DRAG_CHUNKS) {
      if (anchor.x <= minX) maxX = minX + MAX_DRAG_CHUNKS - 1;
      else minX = maxX - MAX_DRAG_CHUNKS + 1;
    }
    if (maxZ - minZ + 1 > MAX_DRAG_CHUNKS) {
      if (anchor.z <= minZ) maxZ = minZ + MAX_DRAG_CHUNKS - 1;
      else minZ = maxZ - MAX_DRAG_CHUNKS + 1;
    }
    return { minX, minZ, maxX, maxZ };
  }

  /** A drawn box's extent, converted to a chunk range and clamped to the
   * current drag's anchor -- what both the live preview and the eventual
   * commit should show/use. */
  function draggedChunkRange(box: DragBox): { minX: number; minZ: number; maxX: number; maxZ: number } {
    const r = extentToChunkRange(box.getGeometry().getExtent());
    return dragAnchorChunk ? clampRangeToAnchor(r, dragAnchorChunk) : r;
  }

  /**
   * Commits a drawn box's extent to the selection, turning selection mode on
   * first if it was off -- so a drag gesture is usable on its own without
   * first hunting for the hotbar.
   */
  function applyBoxSelection(box: DragBox): void {
    if (!selectionMode) setSelectionMode(true);
    const r = draggedChunkRange(box);
    addChunkRangeChecked(r.minX, r.minZ, r.maxX, r.maxZ);
  }

  /** Selects every chunk currently in view -- the keyboard equivalent of
   * drawing a drag-box around the whole visible map. */
  function selectVisibleChunks(): void {
    if (!selectionMode) setSelectionMode(true);
    const b = engine.visibleBlockBounds();
    addChunkRangeChecked(
      blockToChunk(b.minX),
      blockToChunk(b.minZ),
      blockToChunk(b.maxX),
      blockToChunk(b.maxZ),
    );
  }

  // Shift-drag draws a rectangle (plain drag still pans the map), which is
  // how a large claim gets selected without clicking every chunk individually.
  // The drawn rectangle itself is invisible (see .mm-dragbox in main.css) --
  // what the user actually sees is the live chunk-by-chunk preview below,
  // which is what will really get selected rather than an arbitrary
  // freehand box that doesn't line up with anything.
  const dragBox = new DragBox({ className: 'mm-dragbox', condition: shiftKeyOnly });
  engine.map.addInteraction(dragBox);
  dragBox.on('boxstart', () => rememberDragAnchor(dragBox));
  dragBox.on('boxdrag', () => {
    if (!selectionMode) return;
    selection.setPreviewRange(draggedChunkRange(dragBox));
  });
  dragBox.on('boxend', () => {
    selection.setPreviewRange(null);
    if (selectionMode) applyBoxSelection(dragBox);
    dragAnchorChunk = null;
  });
  dragBox.on('boxcancel', () => {
    selection.setPreviewRange(null);
    dragAnchorChunk = null;
  });

  // Right-drag is the same rectangle select, but needs no modifier key -- a
  // right button held down is unambiguously not a pan or a left-click pick.
  // A right press that does NOT turn into a drag falls through to boxcancel
  // below, which is what pops the context menu instead. Either way, right
  // button up always ends at the context menu -- it's the one commit point
  // for whatever got selected, so dismissing it without picking an action
  // (see showMapContextMenu's onDismiss) discards that selection again.
  const dragBoxRight = new DragBox({
    className: 'mm-dragbox',
    condition: (e) => 'button' in e.originalEvent && e.originalEvent.button === 2,
  });
  engine.map.addInteraction(dragBoxRight);
  dragBoxRight.on('boxstart', () => rememberDragAnchor(dragBoxRight));
  dragBoxRight.on('boxdrag', () => {
    selection.setPreviewRange(draggedChunkRange(dragBoxRight));
  });
  dragBoxRight.on('boxend', (e) => {
    selection.setPreviewRange(null);
    applyBoxSelection(dragBoxRight);
    dragAnchorChunk = null;
    openContextMenuAt(e.coordinate as [number, number]);
  });
  dragBoxRight.on('boxcancel', (e) => {
    selection.setPreviewRange(null);
    dragAnchorChunk = null;
    openContextMenuAt(e.coordinate as [number, number]);
  });

  // The browser's own context menu would otherwise appear on every right
  // click and right-drag; the app's menu (or nothing, mid-drag) replaces it.
  // Scoped to the whole document, not just the map viewport -- a right-click
  // on the selection panel, the hotbar, or the app's own context menu (e.g.
  // right-clicking again while it's already open) is still a right-click
  // inside this app, not a request for Chrome's inspect/reload/etc. menu.
  // Text inputs are the one exception: right-clicking one is almost always
  // "I want to paste/cut", so native behaviour survives there.
  document.addEventListener('contextmenu', (e) => {
    const target = e.target as HTMLElement | null;
    if (target?.closest('input, textarea, [contenteditable="true"]')) return;
    e.preventDefault();
  });

  const contextMenu = new ContextMenu();

  /** Opens the map context menu at a map coordinate, converting it to the
   * viewport pixel showMapContextMenu wants. Derived from the map pixel
   * rather than the original DOM event's clientX/Y, because that event's
   * type varies (pointer/keyboard/wheel) and this route works the same
   * regardless. */
  function openContextMenuAt(coord: [number, number]): void {
    const pixel = engine.map.getPixelFromCoordinate(coord);
    if (!pixel) return;
    const rect = engine.map.getViewport().getBoundingClientRect();
    showMapContextMenu(rect.left + pixel[0], rect.top + pixel[1], coord);
  }

  /** Builds and opens the app's context menu for a right-click at `coord`. */
  function showMapContextMenu(clientX: number, clientY: number, coord: [number, number]): void {
    const [bx, bz] = engine.viewToBlockApprox(coord);
    const chunk = { x: blockToChunk(bx), z: blockToChunk(bz) };
    const chunkSelected = selection.has(chunk);

    contextMenu.show(clientX, clientY, [
      {
        label: 'Copy coordinates',
        onSelect: () => {
          void navigator.clipboard.writeText(`${Math.floor(bx)}, ${Math.floor(bz)}`);
        },
      },
      {
        label: 'Show block info',
        onSelect: () => {
          void engine
            .pickAt(coord, 'click')
            .then((pick) => {
              if (pick) infoPanel.show(pick);
            })
            .catch(() => {});
        },
      },
      { label: 'Center map here', onSelect: () => engine.centerOnBlock(bx, bz) },
      { separator: true },
      {
        label: chunkSelected ? 'Remove chunk from selection' : 'Add chunk to selection',
        onSelect: () => {
          if (!selectionMode) setSelectionMode(true);
          selection.toggle(chunk);
        },
      },
      // These act on the whole current selection, not just this one chunk --
      // ensureChunkSelected only guarantees the clicked chunk is included so
      // a right-click with nothing selected yet still has something to act on.
      { label: 'Claim', onSelect: () => { ensureChunkSelected(chunk); doClaim(); } },
      { label: 'Unclaim', onSelect: () => { ensureChunkSelected(chunk); doUnclaim(); } },
      { label: 'Force load', onSelect: () => { ensureChunkSelected(chunk); doForceLoad(); } },
      { label: 'Unload', onSelect: () => { ensureChunkSelected(chunk); doUnload(); } },
      { separator: true },
      {
        label: 'Clear selection',
        disabled: selection.isEmpty(),
        onSelect: () => selection.clear(),
      },
      // Right-click is a propose-then-commit gesture: nothing this menu
      // offers has happened yet, so backing out without picking an action
      // (clicking elsewhere, Escape, losing focus) drops the selection it
      // was about to act on rather than leaving it sitting on the map.
    ], () => selection.clear());
  }

  // ---- Hotbar (2,3) ---------------------------------------------------------

  const hotbarSlots: HotbarSlot[] = [
    {
      icon: ICON_FLAT, key: '1', title: 'Top-down view (M)',
      isOn: () => engine.getMode() === 'top',
      run: () => void switchMode('top'),
    },
    {
      icon: ICON_ISO, key: '2', title: 'Isometric view (M)',
      isOn: () => engine.getMode() === 'iso',
      run: () => void switchMode('iso'),
    },
    {
      icon: ICON_SELECT, key: '3', title: 'Select chunks (S)',
      isOn: () => selectionMode,
      run: () => setSelectionMode(!selectionMode),
    },
    {
      icon: ICON_LAYERS, key: '4', title: 'Layers (G for grid)',
      isOn: () => layersPopover.isVisible(),
      run: () => layersPopover.setVisible(!layersPopover.isVisible()),
    },
    {
      icon: ICON_HOME, key: '5', title: 'Go to spawn',
      isOn: () => false,
      run: () => goToSpawn(),
    },
  ];
  const hotbar = new Hotbar(hotbarSlots);
  chrome.appendChild(hotbar.root);

  function goToSpawn(): void {
    players.stopFollowing();
    const d = engine.getDimension();
    engine.centerOnBlock(d.spawnX, d.spawnZ);
    flashLoading();
  }

  let modeSwitchBusy = false;
  async function switchMode(mode: MapMode): Promise<void> {
    if (modeSwitchBusy || engine.getMode() === mode) return;
    modeSwitchBusy = true;
    hotbar.refresh();
    try {
      await engine.setMode(mode);
    } catch {
      /* the engine keeps the previous mode on failure */
    } finally {
      modeSwitchBusy = false;
      features.reproject();
      players.reproject();
      border.rebuild();
      hotbar.refresh();
    }
  }

  // ---- Compass, scale bar, zoom, spawn (3,3) --------------------------------

  const rightCol = el('div', 'mm-right-col');
  const compass = el('div', 'mm-compass');
  compass.append(el('span', 'mm-compass-n', 'N'), el('span', 'mm-compass-needle'));
  const scaleBar = el('div', 'mm-scalebar');
  const scaleBarLine = el('span', 'mm-scalebar-line');
  const scaleBarLabel = el('span', 'mm-scalebar-label mm-mono');
  scaleBar.append(scaleBarLine, scaleBarLabel);
  const zoomIn = el('button', 'mm-zoom-btn');
  const zoomOut = el('button', 'mm-zoom-btn');
  const homeBtn = el('button', 'mm-zoom-btn');
  zoomIn.innerHTML = ICON_PLUS;
  zoomOut.innerHTML = ICON_MINUS;
  homeBtn.innerHTML = ICON_HOME;
  zoomIn.type = zoomOut.type = homeBtn.type = 'button';
  zoomIn.title = 'Zoom in';
  zoomOut.title = 'Zoom out';
  homeBtn.title = 'Go to spawn';
  rightCol.append(compass, scaleBar, zoomIn, zoomOut, homeBtn);
  chrome.appendChild(rightCol);

  function updateScaleBar(): void {
    const blocks = niceScale(80 / engine.pixelsPerBlock());
    scaleBarLabel.textContent = `${blocks} blocks`;
  }
  updateScaleBar();

  zoomIn.addEventListener('click', () => {
    engine.zoomBy(1);
    flashLoading();
  });
  zoomOut.addEventListener('click', () => {
    engine.zoomBy(-1);
    flashLoading();
  });
  homeBtn.addEventListener('click', goToSpawn);

  // ---- Overlay data loading ----------------------------------------------

  let featuresToken = 0;
  async function refreshFeatures(): Promise<void> {
    const token = ++featuresToken;
    const bounds = engine.visibleBlockBounds();
    try {
      const set = await api.features(engine.getDimension().id, bounds, engine.zoom());
      // A newer request has superseded this one; discard the stale answer.
      if (!set || token !== featuresToken) return;
      features.setAreas(set.areas);
      features.setMarkers(set.markers);
      claimsLegend.setEntries(features.legendEntries());
    } catch {
      // A transient failure must not break the map; the next move retries.
    }
  }

  async function refreshPlayers(): Promise<void> {
    // A backgrounded tab cannot show anything, so polling it only burns the
    // server's time and the user's battery. The socket still delivers updates
    // when the tab returns, and a refresh runs on visibility change.
    if (document.hidden) return;
    try {
      const list = await api.players(engine.getDimension().id);
      players.update(list);
      refreshJumpTargets();
    } catch {
      /* the next poll retries */
    }
  }

  // ---- Interaction --------------------------------------------------------

  debugHud.setZoom(engine.zoom(), engine.pixelsPerBlock());

  engine.on('zoom', () => {
    debugHud.setZoom(engine.zoom(), engine.pixelsPerBlock());
    updateScaleBar();
    // Only rebuilds when an integer zoom boundary is actually crossed, so a
    // zoom gesture's many fractional resolution ticks don't each reallocate
    // every area and marker feature.
    features.onZoomChanged();
  });

  engine.on('moveend', () => {
    void refreshFeatures();
  });

  /**
   * Cursor readout.
   *
   * Top-down needs no server round trip, so it updates on every mouse move. In
   * isometric mode the X/Z estimate from the reference plane appears instantly
   * and a throttled ray march refines it with the true block and biome, which
   * keeps the bar responsive without ever showing an unverified Y.
   */
  let hoverTimer: number | null = null;
  let lastHover: [number, number] | null = null;

  engine.on('pointermove', (coord) => {
    const c = coord as [number, number];
    lastHover = c;

    // The immediate estimate: exact in top-down, reference-plane in isometric.
    const [x, z] = engine.viewToBlockApprox(c);
    debugHud.setPosition(x, z);
    debugHud.setChunk(blockToChunk(x), blockToChunk(z));
    scheduleHoverPick();

    // Preview which chunk a click would hit, so the effect of clicking is
    // obvious before it happens rather than only after.
    if (selectionMode) selection.setHover({ x: blockToChunk(x), z: blockToChunk(z) });

    // A player marker under the cursor gets a pointer cursor, the same
    // affordance a link gets, so "this is clickable" doesn't rely on the
    // user discovering it by accident.
    if (!selectionMode) {
      const pixel = engine.map.getPixelFromCoordinate(c);
      const hit = pixel ? players.hitTest(pixel) : null;
      engine.map.getViewport().classList.toggle('mm-followable', !!hit);
    }
  });

  // The cursor leaving the map entirely gets no further pointermove events,
  // so the hover preview would otherwise be left showing the last chunk
  // visited instead of clearing.
  engine.map.getViewport().addEventListener('pointerleave', () => {
    selection.setHover(null);
    engine.map.getViewport().classList.remove('mm-followable');
  });

  /**
   * Refines the readout with the true surface block.
   *
   * Throttled, and issued under its own request key so a stream of hover probes
   * can never cancel a click the user just made. In isometric mode this is what
   * turns the reference-plane estimate into the block genuinely visible under
   * the cursor, resolved by the server's ray march against terrain heights.
   */
  function scheduleHoverPick(): void {
    if (hoverTimer !== null) return;
    hoverTimer = window.setTimeout(() => {
      hoverTimer = null;
      if (!lastHover) return;
      void engine
        .pickAt(lastHover, 'hover')
        .then((pick) => {
          if (!pick || !pick.found) return;
          const surfaceY = pick.water && pick.waterY !== undefined ? pick.waterY : pick.y;
          debugHud.setPosition(pick.x, pick.z, surfaceY);
          debugHud.setBiome(pick.biome);
          debugHud.setChunk(pick.chunkX, pick.chunkZ);
        })
        .catch(() => {});
    }, 70);
  }

  engine.on('click', (info) => {
    const c = info.coordinate as [number, number];

    // A player marker (icon or its nametag -- OpenLayers hit-tests text the
    // same as any other style component) takes priority over both selection
    // and picking: clicking one means "follow this player", which jumps the
    // view to them immediately rather than waiting for their next position
    // update to quietly recentre.
    const pixel = engine.map.getPixelFromCoordinate(c);
    const hitPlayer = pixel ? players.hitTest(pixel) : null;
    if (hitPlayer) {
      const following = players.toggleFollow(hitPlayer.uuid);
      searchBar.clear();
      if (following) {
        // Close enough to actually see them, not just "somewhere in view".
        const zoom = Math.max(engine.zoom(), FOLLOW_ZOOM);
        engine.setZoom(zoom, true);
        engine.centerOnBlock(hitPlayer.x, hitPlayer.z, hitPlayer.y);
      } else {
        engine.map.render();
      }
      return;
    }

    // Ctrl/Cmd+click toggles a chunk even outside selection mode -- the same
    // shorthand file managers use for ad hoc multi-select, so a user doesn't
    // have to find the hotbar first just to grab one chunk.
    const wantsSelect = selectionMode || info.ctrlKey || info.metaKey;
    if (wantsSelect) {
      if (!selectionMode) setSelectionMode(true);
      const [bx, bz] = engine.viewToBlockApprox(c);
      const chunk = { x: blockToChunk(bx), z: blockToChunk(bz) };
      if (info.shiftKey && lastClickedChunk) {
        // The rectangle between the last click and this one -- the standard
        // spreadsheet/file-manager range-select gesture.
        selection.addRange(
          Math.min(lastClickedChunk.x, chunk.x),
          Math.min(lastClickedChunk.z, chunk.z),
          Math.max(lastClickedChunk.x, chunk.x),
          Math.max(lastClickedChunk.z, chunk.z),
        );
      } else {
        selection.toggle(chunk);
      }
      lastClickedChunk = chunk;
      return;
    }
    void engine
      .pickAt(c, 'click')
      .then((pick: PickResult | null) => {
        if (pick) infoPanel.show(pick);
      })
      .catch(() => {});
  });

  dimensionMenu.onChange((d: DimensionInfo) => {
    api.abortAll();
    players.clear();
    // A chunk selection is meaningless once the dimension underneath it
    // changes -- the same X/Z now names a completely different place.
    selection.clear();
    debugHud.setDimension(d.id);
    dimensionMenu.render(config.dimensions, d);
    // setDimension() emits 'moveend' internally, which the listener below
    // already turns into a refreshFeatures() call -- no need to repeat it.
    engine.setDimension(d);
    border.rebuild();
    socket.subscribe(d.id);
    void refreshPlayers();
    flashLoading();
  });

  searchBar.setHandlers({
    onQuery: (q) => api.search(engine.getDimension().id, q),
    onPick: (r) => {
      players.stopFollowing();
      if (r.dimension && r.dimension !== engine.getDimension().id) {
        const target = config.dimensions.find((d) => d.id === r.dimension);
        if (target) {
          // Same guard as the dimension menu: without it, a slow request
          // still in flight for the dimension being left can resolve after
          // this switch and briefly render data for the wrong dimension.
          api.abortAll();
          players.clear();
          selection.clear();
          debugHud.setDimension(target.id);
          engine.setDimension(target);
          dimensionMenu.render(config.dimensions, target);
          socket.subscribe(target.id);
          void refreshPlayers();
        }
      }
      // Coordinate searches should land close enough to see the target block.
      const zoom = Math.max(engine.zoom(), r.type === 'coordinates' ? 7 : 6);
      engine.setZoom(zoom, true);
      engine.centerOnBlock(r.x, r.z);
      searchBar.clear();
      void refreshFeatures();
      flashLoading();
    },
  });

  // Keyboard shortcuts, kept minimal and non-intrusive.
  document.addEventListener('keydown', (e) => {
    if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;
    const hotbarSlot = noModifiers(e) ? hotbarSlots.find((s) => s.key === e.key) : undefined;
    if (e.key === '/') {
      e.preventDefault();
      searchBar.input.focus();
    } else if (hotbarSlot) {
      hotbarSlot.run();
    } else if (e.key === 'g') {
      setChunkGrid(!overlayState.chunkGrid);
    } else if (e.key === 'm') {
      void switchMode(engine.getMode() === 'top' ? 'iso' : 'top');
    } else if (e.key === 'Escape') {
      if (players.isFollowing()) {
        players.stopFollowing();
      } else if (contextMenu.isOpen()) {
        contextMenu.close();
      } else if (layersPopover.isVisible()) {
        layersPopover.setVisible(false);
      } else if (selectionMode && !selection.isEmpty()) {
        selection.clear();
      } else if (selectionMode) {
        setSelectionMode(false);
      } else if (infoPanel.isVisible()) {
        infoPanel.hide();
      }
    } else if (selectionMode && noModifiers(e) && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      selectVisibleChunks();
    } else if (selectionMode && noModifiers(e) && !selection.isEmpty()) {
      const action = SELECTION_KEY_ACTIONS[e.key.toLowerCase()];
      if (action) action();
    }
  });

  // ---- Realtime -----------------------------------------------------------

  const socket = new RealtimeSocket();
  socket.on({
    onConnectionState: (state: ConnectionState) => {
      debugHud.setConnection(state, config.live);
      searchBar.setConnectionBanner(
        state === 'live'
          ? null
          : state === 'reconnecting'
            ? 'Lost the server — retrying every few seconds. Tiles shown are the last received.'
            : 'Offline. Showing the last cached tiles; claims and players may be stale.',
        state === 'reconnecting' ? '#e6c07a' : '#e07b7b',
      );
    },
    onPlayers: (list, dim) => {
      if (dim === engine.getDimension().id) {
        players.update(list);
        refreshJumpTargets();
      }
    },
    onChunkUpdated: (ev, dim) => {
      if (dim !== engine.getDimension().id) return;
      engine.applyTileRevisions(ev.tiles ?? []);
    },
    onFeatureUpdated: (_kind, dim) => {
      if (dim === engine.getDimension().id) void refreshFeatures();
    },
  });
  if (config.live) {
    socket.connect(engine.getDimension().id);
    // Polling is the fallback when the socket is down, and is cheap enough to
    // leave running as a safety net.
    window.setInterval(() => void refreshPlayers(), 5000);
  }

  // Catch up immediately when the tab comes back to the foreground.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) {
      void refreshPlayers();
      void refreshFeatures();
      engine.updateSize();
    }
  });

  urlState.attach(engine);

  // A handle for debugging and automated checks. It exposes no capability the
  // page does not already have; it just saves fishing the instance out of the DOM.
  (window as unknown as Record<string, unknown>).__minecraftMap = {
    engine, api, players, features, selection,
  };

  await refreshFeatures();
  await refreshPlayers();
  debugHud.setZoom(engine.zoom(), engine.pixelsPerBlock());
}

/** Renders a fatal startup error instead of a blank page. */
function showFatal(root: HTMLElement, err: unknown): void {
  const box = el('div', 'mm-fatal mm-panel');
  box.appendChild(el('h1', undefined, 'Map unavailable'));
  box.appendChild(
    el(
      'p',
      undefined,
      'The map server could not be reached or returned no usable configuration.',
    ),
  );
  const detail = el('pre', 'mm-mono');
  detail.textContent = err instanceof Error ? err.message : String(err);
  box.appendChild(detail);
  root.replaceChildren(box);
}

void boot();
