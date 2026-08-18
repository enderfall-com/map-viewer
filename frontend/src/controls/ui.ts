import type { DimensionInfo, PickResult, SearchResult } from '../api/types';
import { ICON_CLOSE, ICON_COORDS, ICON_LAYERS, ICON_PLAYER, ICON_SEARCH, ICON_WARP } from './icons';

/** Creates an element with optional class names and text. */
export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  className?: string,
  text?: string,
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/** Closes `panel` on a click outside `container` or on Escape -- the one
 * piece of behaviour every popover (dimension menu, layers popover) needs,
 * kept here once rather than copied into each. */
function closeOnOutside(container: HTMLElement, isOpen: () => boolean, close: () => void): void {
  document.addEventListener('pointerdown', (e) => {
    if (isOpen() && !container.contains(e.target as Node)) close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && isOpen()) close();
  });
}

/** Turns "minecraft:grass_block" into "Grass Block". */
export function prettyId(id: string): string {
  if (!id) return '';
  const local = id.includes(':') ? id.slice(id.indexOf(':') + 1) : id;
  return local
    .split('_')
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join(' ');
}

// ---------------------------------------------------------------------------
// Debug HUD (grid cell 1,1) -- replaces the old bottom status bar.
// ---------------------------------------------------------------------------

export type ConnectionState = 'live' | 'reconnecting' | 'offline';

const CONN_COLOR: Record<ConnectionState, string> = {
  live: '#7ee787',
  reconnecting: '#e6c07a',
  offline: '#e07b7b',
};
const CONN_LABEL: Record<ConnectionState, string> = {
  live: 'LIVE',
  reconnecting: 'RECONNECTING',
  offline: 'OFFLINE',
};

/** F3-style debug readout: dimension, cursor position, chunk, biome, zoom
 * and connection state. Purely informational -- never intercepts a click,
 * so it never needs `pointer-events: auto`. */
export class DebugHud {
  readonly root: HTMLElement;
  private readonly dimEl: HTMLElement;
  private readonly xyzEl: HTMLElement;
  private readonly chunkEl: HTMLElement;
  private readonly biomeEl: HTMLElement;
  private readonly zoomEl: HTMLElement;
  private readonly connDot: HTMLElement;
  private readonly connLabel: HTMLElement;
  private readonly connRow: HTMLElement;

  constructor(brandMarkSvg: string, brandName: string) {
    this.root = el('div', 'mm-debughud');

    const brandRow = el('div', 'mm-debughud-brand');
    const mark = el('span', 'mm-debughud-mark');
    mark.innerHTML = brandMarkSvg;
    brandRow.appendChild(mark);
    brandRow.appendChild(el('span', 'mm-debughud-name', brandName));
    this.dimEl = el('span', 'mm-debughud-dim');
    brandRow.appendChild(this.dimEl);
    this.root.appendChild(brandRow);

    this.xyzEl = el('div', 'mm-debughud-row', 'XYZ  -');
    this.chunkEl = el('div', 'mm-debughud-row', 'CHUNK  -, -');
    this.biomeEl = el('div', 'mm-debughud-muted', '');
    this.zoomEl = el('div', 'mm-debughud-muted', '');
    this.root.append(this.xyzEl, this.chunkEl, this.biomeEl, this.zoomEl);

    this.connRow = el('div', 'mm-debughud-conn');
    this.connDot = el('span', 'mm-debughud-dot');
    this.connLabel = el('span');
    this.connRow.append(this.connDot, this.connLabel);
    this.root.appendChild(this.connRow);
  }

  setDimension(id: string): void {
    this.dimEl.textContent = id;
  }

  /** `y` is omitted until a server pick has resolved it (isometric mode's
   * reference-plane estimate has no real elevation yet). */
  setPosition(x: number, z: number, y?: number): void {
    const yPart = y !== undefined && Number.isFinite(y) ? Math.floor(y) : '-';
    this.xyzEl.textContent = `XYZ  ${Math.floor(x)} / ${yPart} / ${Math.floor(z)}`;
  }

  setChunk(cx: number, cz: number): void {
    this.chunkEl.textContent = `CHUNK  ${cx}, ${cz}`;
  }

  setBiome(biome?: string): void {
    this.biomeEl.textContent = biome ? prettyId(biome) : '';
  }

  setZoom(zoom: number, ppb: number): void {
    const scale =
      ppb >= 1 ? `${ppb.toFixed(ppb < 4 ? 1 : 0)} px/block` : `${(1 / ppb).toFixed(0)} blocks/px`;
    this.zoomEl.textContent = `ZOOM ${zoom.toFixed(2)}  ${scale}`;
  }

  /** `show` is the server's `live` flag -- realtime is entirely disabled, so
   * showing a connection indicator for it would be noise, not information. */
  setConnection(state: ConnectionState, show: boolean): void {
    this.connRow.hidden = !show;
    this.connDot.style.background = CONN_COLOR[state];
    this.connLabel.textContent = CONN_LABEL[state];
    this.connLabel.style.color = CONN_COLOR[state];
  }
}

// ---------------------------------------------------------------------------
// Search bar (grid cell 2,1) -- persistent, grouped suggestions.
// ---------------------------------------------------------------------------

const GROUP_ORDER = ['JUMP TO', 'COORDINATES', 'PLAYERS', 'PLACES', 'AREAS'];

function groupFor(type: string): string {
  if (type === 'jumpto') return 'JUMP TO';
  if (type === 'coordinates') return 'COORDINATES';
  if (type === 'player') return 'PLAYERS';
  if (type === 'claim' || type === 'region' || type === 'forceload') return 'AREAS';
  return 'PLACES'; // spawn / warp / home / waypoint / poi
}

function iconFor(type: string): string {
  if (type === 'coordinates') return ICON_COORDS;
  if (type === 'player') return ICON_PLAYER;
  if (type === 'claim' || type === 'region' || type === 'forceload') return ICON_LAYERS;
  return ICON_WARP;
}

function tintFor(type: string): string {
  if (type === 'player') return '#7ee787';
  if (type === 'coordinates') return '#e6c07a';
  return '#8ea6c8';
}

export interface SearchBarHandlers {
  onQuery(q: string): Promise<SearchResult[]>;
  onPick(r: SearchResult): void;
}

/**
 * Always-visible search, with grouped suggestions instead of the old
 * header-only search field. An empty (or just-focused) query shows the
 * caller-supplied "jump to" shortlist -- built from data already on hand
 * (spawn, currently tracked players) -- rather than hitting the API for
 * nothing typed yet; anything else queries the real `/api/search`.
 */
export class SearchBar {
  readonly root: HTMLElement;
  readonly input: HTMLInputElement;
  private readonly suggestions: HTMLElement;
  private readonly loadingChip: HTMLElement;
  private readonly connBanner: HTMLElement;
  private readonly connBannerDot: HTMLElement;
  private readonly connBannerText: HTMLElement;

  private handlers: SearchBarHandlers = { onQuery: async () => [], onPick: () => {} };
  private jumpTo: SearchResult[] = [];
  private current: SearchResult[] = [];
  private active = 0;
  private focused = false;
  private debounce: number | null = null;
  private queryToken = 0;

  constructor() {
    this.root = el('div', 'mm-search-wrap');

    const bar = el('div', 'mm-search-bar mm-panel');
    const icon = el('span', 'mm-search-icon');
    icon.innerHTML = ICON_SEARCH;
    bar.appendChild(icon);

    this.input = el('input', 'mm-search-input2');
    this.input.type = 'text';
    this.input.placeholder = 'Jump to a player, warp, structure, or 1250 -8220';
    this.input.autocomplete = 'off';
    this.input.spellcheck = false;
    bar.appendChild(this.input);
    bar.appendChild(el('span', 'mm-search-slash', '/'));
    this.root.appendChild(bar);

    this.suggestions = el('div', 'mm-suggestions mm-panel');
    this.suggestions.hidden = true;
    this.root.appendChild(this.suggestions);

    this.loadingChip = el('div', 'mm-loading-chip mm-panel');
    const dot = el('span', 'mm-loading-dot');
    this.loadingChip.append(dot, el('span', undefined, 'Loading tiles…'));
    this.loadingChip.hidden = true;
    this.root.appendChild(this.loadingChip);

    this.connBanner = el('div', 'mm-conn-banner mm-panel');
    this.connBannerDot = el('span', 'mm-conn-banner-dot');
    this.connBannerText = el('span', 'mm-conn-banner-text');
    this.connBanner.append(this.connBannerDot, this.connBannerText);
    this.connBanner.hidden = true;
    this.root.appendChild(this.connBanner);

    this.input.addEventListener('input', () => this.schedule());
    this.input.addEventListener('focus', () => {
      this.focused = true;
      this.renderCurrentQuery();
    });
    this.input.addEventListener('blur', () => {
      // Delayed so a mousedown on a suggestion row (which fires before blur)
      // still registers -- see the row's own onPick listener below.
      window.setTimeout(() => {
        this.focused = false;
        this.updateVisibility();
      }, 120);
    });
    this.input.addEventListener('keydown', (e) => this.onKeyDown(e));
  }

  setHandlers(handlers: SearchBarHandlers): void {
    this.handlers = handlers;
  }

  /** The empty-query shortlist: spawn plus whichever players are on hand. */
  setJumpToSuggestions(items: SearchResult[]): void {
    this.jumpTo = items;
    if (!this.input.value.trim()) this.renderCurrentQuery();
  }

  setLoading(v: boolean): void {
    this.loadingChip.hidden = !v;
  }

  /** Pass `null` to hide the banner entirely (the live/connected case). */
  setConnectionBanner(text: string | null, color: string): void {
    this.connBanner.hidden = !text;
    if (text) {
      this.connBannerText.textContent = text;
      this.connBannerDot.style.background = color;
    }
  }

  private schedule(): void {
    if (this.debounce !== null) window.clearTimeout(this.debounce);
    this.debounce = window.setTimeout(() => {
      this.debounce = null;
      void this.renderCurrentQuery();
    }, 160);
  }

  private async renderCurrentQuery(): Promise<void> {
    const q = this.input.value.trim();
    if (!q) {
      this.current = this.jumpTo;
      this.active = 0;
      this.renderList();
      return;
    }
    const token = ++this.queryToken;
    let results: SearchResult[];
    try {
      results = await this.handlers.onQuery(q);
    } catch {
      results = [];
    }
    if (token !== this.queryToken) return; // a newer query has superseded this one
    this.current = results;
    this.active = 0;
    this.renderList();
  }

  private renderList(): void {
    this.suggestions.replaceChildren();
    const sorted = [...this.current].sort((a, b) => {
      const ga = GROUP_ORDER.indexOf(groupFor(a.type));
      const gb = GROUP_ORDER.indexOf(groupFor(b.type));
      return ga - gb;
    });

    let lastGroup: string | null = null;
    sorted.forEach((r, i) => {
      const group = groupFor(r.type);
      if (group !== lastGroup) {
        this.suggestions.appendChild(el('div', 'mm-suggestions-group', group));
        lastGroup = group;
      }
      const row = el('button', 'mm-suggestions-row');
      row.type = 'button';
      row.classList.toggle('is-active', i === this.active);

      const icon = el('span', 'mm-suggestions-icon');
      icon.style.color = tintFor(r.type);
      icon.innerHTML = iconFor(r.type);

      const text = el('span', 'mm-suggestions-text');
      text.appendChild(el('span', 'mm-suggestions-name', r.name));
      text.appendChild(el('span', 'mm-suggestions-sub', r.dimension));

      const coords = el('span', 'mm-suggestions-coords', `${Math.round(r.x)}, ${Math.round(r.z)}`);

      row.append(icon, text, coords);
      row.addEventListener('mouseenter', () => this.setActive(i));
      // mousedown (not click) fires before the input's blur handler, so a
      // suggestion is still there to pick when the click lands.
      row.addEventListener('mousedown', (e) => {
        e.preventDefault();
        this.pick(r);
      });
      this.suggestions.appendChild(row);
    });

    this.updateVisibility();
  }

  private setActive(i: number): void {
    this.active = i;
    [...this.suggestions.querySelectorAll('.mm-suggestions-row')].forEach((rowEl, idx) =>
      rowEl.classList.toggle('is-active', idx === i),
    );
  }

  private updateVisibility(): void {
    const open = (this.focused || this.input.value.trim().length > 0) && this.current.length > 0;
    this.suggestions.hidden = !open;
  }

  private pick(r: SearchResult): void {
    this.suggestions.hidden = true;
    this.input.blur();
    this.handlers.onPick(r);
  }

  clear(): void {
    this.input.value = '';
    this.current = this.jumpTo;
    this.suggestions.hidden = true;
  }

  private onKeyDown(e: KeyboardEvent): void {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      this.setActive(Math.min(this.active + 1, this.current.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      this.setActive(Math.max(this.active - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const pick = this.current[this.active];
      if (pick) this.pick(pick);
    } else if (e.key === 'Escape') {
      this.input.blur();
      this.clear();
    }
  }
}

// ---------------------------------------------------------------------------
// Dimension menu (grid cell 3,1, shared with the fullscreen button).
// ---------------------------------------------------------------------------

export class DimensionMenu {
  readonly button: HTMLButtonElement;
  readonly panel: HTMLElement;
  private open = false;
  private onSelect: (d: DimensionInfo) => void = () => {};

  /**
   * `container` must be an element that already contains (or will contain)
   * both `button` and `panel` -- e.g. the shared `.mm-dim-wrap` -- so the
   * outside-click check below correctly treats a click on the toggle button
   * itself as "inside" and never fights the button's own open/close logic.
   */
  constructor(dimensions: DimensionInfo[], current: DimensionInfo, brandIconSvg: string, container: HTMLElement) {
    this.button = el('button', 'mm-dim-btn');
    this.button.type = 'button';
    const icon = el('span', 'mm-dim-icon');
    icon.innerHTML = brandIconSvg;
    const label = el('span', undefined, current.name);
    label.dataset.role = 'label';
    this.button.append(icon, label);
    this.button.addEventListener('click', () => this.setOpen(!this.open));

    this.panel = el('div', 'mm-dim-menu mm-panel');
    this.panel.hidden = true;

    closeOnOutside(container, () => this.open, () => this.setOpen(false));

    this.render(dimensions, current);
  }

  onChange(fn: (d: DimensionInfo) => void): void {
    this.onSelect = fn;
  }

  setOpen(v: boolean): void {
    this.open = v;
    this.panel.hidden = !v;
  }

  render(dimensions: DimensionInfo[], current: DimensionInfo): void {
    this.panel.replaceChildren();
    for (const d of dimensions) {
      const item = el('button', 'mm-dim-item');
      item.type = 'button';
      if (d.id === current.id) item.classList.add('is-active');
      item.appendChild(el('span', 'mm-dim-item-name', d.name));
      item.appendChild(el('span', 'mm-dim-item-id', d.id));
      item.addEventListener('click', () => {
        this.setOpen(false);
        this.onSelect(d);
      });
      this.panel.appendChild(item);
    }
    const label = this.button.querySelector('[data-role="label"]');
    if (label) label.textContent = current.name;
  }
}

// ---------------------------------------------------------------------------
// Hotbar (grid cell 2,3) -- 2D / ISO / select / layers / spawn, number-keyed.
// ---------------------------------------------------------------------------

export interface HotbarSlot {
  icon: string;
  key: string;
  title: string;
  isOn(): boolean;
  run(): void;
}

export class Hotbar {
  readonly root: HTMLElement;
  private readonly tip: HTMLElement;
  private readonly slots: Array<{ def: HotbarSlot; button: HTMLButtonElement }> = [];

  constructor(defs: HotbarSlot[]) {
    this.root = el('div', 'mm-hotbar-wrap');
    this.tip = el('div', 'mm-hotbar-tip mm-panel');
    this.tip.hidden = true;
    this.root.appendChild(this.tip);

    const bar = el('div', 'mm-hotbar mm-panel');
    for (const def of defs) {
      const button = el('button', 'mm-hotbar-slot');
      button.type = 'button';
      const iconSpan = el('span');
      iconSpan.innerHTML = def.icon;
      const keySpan = el('span', 'mm-hotbar-key', def.key);
      button.append(iconSpan, keySpan);
      button.addEventListener('click', def.run);
      button.addEventListener('mouseenter', () => this.showTip(def.title));
      button.addEventListener('mouseleave', () => this.hideTip());
      bar.appendChild(button);
      this.slots.push({ def, button });
    }
    this.root.appendChild(bar);
    this.refresh();
  }

  private showTip(text: string): void {
    this.tip.textContent = text;
    this.tip.hidden = false;
  }

  private hideTip(): void {
    this.tip.hidden = true;
  }

  /** Re-reads every slot's `isOn()`, e.g. after the mode or selection state
   * changed somewhere other than a click on the hotbar itself. */
  refresh(): void {
    for (const { def, button } of this.slots) {
      button.classList.toggle('is-on', def.isOn());
    }
  }
}

// ---------------------------------------------------------------------------
// Layers popover (grid cell 2,2).
// ---------------------------------------------------------------------------

export interface LayerToggleDef {
  key: string;
  name: string;
  isOn(): boolean;
  onToggle(): void;
}

export class LayersPopover {
  readonly root: HTMLElement;
  private readonly grid: HTMLElement;
  private readonly toggles: Array<{ def: LayerToggleDef; button: HTMLButtonElement; dot: HTMLElement }> = [];

  /**
   * `boundary` must contain both this popover's panel and whatever button
   * toggles it (the hotbar's "layers" slot lives in a different grid cell
   * than the panel, so their nearest shared ancestor is the whole chrome
   * overlay) -- otherwise a click on the toggle button would count as
   * "outside", close the popover, and then the button's own click handler
   * would immediately reopen it.
   */
  constructor(defs: LayerToggleDef[], boundary: HTMLElement) {
    this.root = el('div', 'mm-layers-popover mm-panel');
    this.root.hidden = true;
    this.root.appendChild(el('div', 'mm-layers-title', 'OVERLAYS'));
    this.grid = el('div', 'mm-layers-grid');
    this.root.appendChild(this.grid);

    for (const def of defs) {
      const button = el('button', 'mm-layers-toggle');
      button.type = 'button';
      const dot = el('span', 'mm-layers-dot');
      button.append(dot, el('span', undefined, def.name));
      button.addEventListener('click', () => {
        def.onToggle();
        this.refresh();
      });
      this.grid.appendChild(button);
      this.toggles.push({ def, button, dot });
    }
    this.refresh();

    closeOnOutside(boundary, () => !this.root.hidden, () => this.setVisible(false));
  }

  setVisible(v: boolean): void {
    this.root.hidden = !v;
  }

  isVisible(): boolean {
    return !this.root.hidden;
  }

  refresh(): void {
    for (const { def, button } of this.toggles) {
      button.classList.toggle('is-on', def.isOn());
    }
  }
}

// ---------------------------------------------------------------------------
// Claims legend (bottom of the left column).
// ---------------------------------------------------------------------------

export interface LegendEntry {
  owner: string;
  color: string;
  chunks: number;
}

export class ClaimsLegend {
  readonly root: HTMLElement;
  private readonly list: HTMLElement;

  constructor() {
    this.root = el('div', 'mm-legend mm-panel');
    this.root.appendChild(el('div', 'mm-legend-title', 'CLAIMS'));
    this.list = el('div', 'mm-legend-list');
    this.root.appendChild(this.list);
  }

  setVisible(v: boolean): void {
    this.root.hidden = !v;
  }

  setEntries(entries: LegendEntry[]): void {
    this.list.replaceChildren();
    if (!entries.length) {
      this.list.appendChild(el('div', 'mm-legend-empty', 'No claims in view'));
      return;
    }
    for (const e of entries) {
      const row = el('div', 'mm-legend-row');
      const dot = el('span', 'mm-legend-dot');
      dot.style.background = e.color;
      row.appendChild(dot);
      row.appendChild(el('span', 'mm-legend-owner', e.owner));
      row.appendChild(el('span', 'mm-legend-count', `${e.chunks} chunk${e.chunks === 1 ? '' : 's'}`));
      this.list.appendChild(row);
    }
  }
}

// ---------------------------------------------------------------------------
// Selection panel (top of the left column).
// ---------------------------------------------------------------------------

/** Callbacks the selection panel's buttons drive. */
export interface SelectionActions {
  onClaim(): void;
  onUnclaim(): void;
  onForceLoad(): void;
  onUnload(): void;
  onClear(): void;
}

/**
 * The floating panel shown while chunk-selection mode is active: how many
 * chunks are selected, an optional owner name for claiming, and the action
 * buttons themselves.
 */
export class SelectionPanel {
  readonly root: HTMLElement;
  private readonly countEl: HTMLElement;
  private readonly ownerInput: HTMLInputElement;
  private readonly errorEl: HTMLElement;
  private readonly flashEl: HTMLElement;
  private readonly claimBtn: HTMLButtonElement;
  private readonly unclaimBtn: HTMLButtonElement;
  private readonly forceLoadBtn: HTMLButtonElement;
  private readonly unloadBtn: HTMLButtonElement;
  private actions: SelectionActions = {
    onClaim: () => {},
    onUnclaim: () => {},
    onForceLoad: () => {},
    onUnload: () => {},
    onClear: () => {},
  };
  private lastCount = 0;
  private flashTimer: number | null = null;

  constructor() {
    this.root = el('div', 'mm-selection mm-panel');
    this.root.hidden = true;

    const head = el('div', 'mm-selection-head');
    this.countEl = el('span', 'mm-selection-count', '0 CHUNKS');
    const clear = el('button', 'mm-selection-clear', 'clear');
    clear.type = 'button';
    clear.addEventListener('click', () => this.actions.onClear());
    head.append(this.countEl, clear);
    this.root.appendChild(head);

    this.ownerInput = el('input', 'mm-selection-owner');
    this.ownerInput.type = 'text';
    this.ownerInput.placeholder = 'Owner name (optional)';
    this.ownerInput.autocomplete = 'off';
    // Remembered locally so a returning claimant does not retype it every
    // session -- this is a convenience only, not an identity of any kind.
    try {
      this.ownerInput.value = localStorage.getItem('mm-owner-name') ?? '';
    } catch {
      /* storage may be unavailable (private browsing, disabled cookies) */
    }
    this.ownerInput.addEventListener('input', () => {
      try {
        localStorage.setItem('mm-owner-name', this.ownerInput.value);
      } catch {
        /* not worth failing the input over */
      }
    });
    this.root.appendChild(this.ownerInput);

    const actionsRow = el('div', 'mm-selection-actions');
    this.claimBtn = el('button', 'mm-selection-btn mm-claim', 'CLAIM');
    this.unclaimBtn = el('button', 'mm-selection-btn', 'UNCLAIM');
    this.forceLoadBtn = el('button', 'mm-selection-btn mm-forceload', 'FORCE');
    this.unloadBtn = el('button', 'mm-selection-btn', 'UNLOAD');
    this.claimBtn.title = 'Claim selected chunks (C)';
    this.unclaimBtn.title = 'Unclaim selected chunks (U)';
    this.forceLoadBtn.title = 'Force-load selected chunks (F)';
    this.unloadBtn.title = 'Stop force-loading selected chunks (L)';
    for (const b of [this.claimBtn, this.unclaimBtn, this.forceLoadBtn, this.unloadBtn]) {
      b.type = 'button';
    }
    this.claimBtn.addEventListener('click', () => this.actions.onClaim());
    this.unclaimBtn.addEventListener('click', () => this.actions.onUnclaim());
    this.forceLoadBtn.addEventListener('click', () => this.actions.onForceLoad());
    this.unloadBtn.addEventListener('click', () => this.actions.onUnload());
    actionsRow.append(this.claimBtn, this.unclaimBtn, this.forceLoadBtn, this.unloadBtn);
    this.root.appendChild(actionsRow);

    const hint = el(
      'div',
      'mm-selection-hint',
      'Click, shift/ctrl+click, or shift/right-drag to multi-select · C/U/F/L to act · A takes the view',
    );
    this.root.appendChild(hint);

    this.errorEl = el('div', 'mm-selection-error');
    this.errorEl.hidden = true;
    this.root.appendChild(this.errorEl);

    this.flashEl = el('div', 'mm-flash');
    this.flashEl.hidden = true;
    this.root.appendChild(this.flashEl);
  }

  /** Fires when Enter is pressed in the owner-name field -- typing a name and
   * hitting Enter to claim is the expected shorthand for a form like this. */
  onOwnerEnter(fn: () => void): void {
    this.ownerInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') fn();
    });
  }

  setActions(actions: SelectionActions): void {
    this.actions = actions;
  }

  setVisible(v: boolean): void {
    this.root.hidden = !v;
  }

  /** Updates the displayed count and enables/disables actions accordingly. */
  setCount(n: number): void {
    this.lastCount = n;
    this.countEl.textContent = n === 1 ? '1 CHUNK' : `${n} CHUNKS`;
    this.updateButtons(false);
  }

  /** Disables the action buttons while a request is in flight, so a slow
   * network cannot let a double-click submit the same action twice. */
  setBusy(busy: boolean): void {
    this.updateButtons(busy);
  }

  private updateButtons(busy: boolean): void {
    const disabled = busy || this.lastCount === 0;
    for (const b of [this.claimBtn, this.unclaimBtn, this.forceLoadBtn, this.unloadBtn]) {
      b.disabled = disabled;
    }
  }

  showError(message: string): void {
    this.errorEl.textContent = message;
    this.errorEl.hidden = false;
  }

  clearError(): void {
    this.errorEl.hidden = true;
  }

  /** A transient positive confirmation (e.g. "Claimed 12 chunks"), separate
   * from `showError` so a success doesn't have to borrow the red error slot. */
  showFlash(message: string): void {
    this.flashEl.textContent = message;
    this.flashEl.hidden = false;
    if (this.flashTimer !== null) window.clearTimeout(this.flashTimer);
    this.flashTimer = window.setTimeout(() => {
      this.flashEl.hidden = true;
    }, 1800);
  }

  ownerName(): string {
    return this.ownerInput.value.trim();
  }
}

// ---------------------------------------------------------------------------
// Context menu.
// ---------------------------------------------------------------------------

/** One entry in a {@link ContextMenu}, or a visual divider between groups. */
export type ContextMenuItem =
  | { separator: true }
  | { label: string; disabled?: boolean; onSelect: () => void };

/**
 * The app's own right-click menu.
 *
 * Rendered fresh at `show()` time from whatever items the caller passes, so
 * every right-click can offer a different set of actions for the point
 * clicked (e.g. whether a chunk is already selected) without the menu having
 * to know about app state itself. Appended directly to `document.body`
 * rather than the map host so `position: fixed` placement is never affected
 * by an ancestor's `overflow: hidden`.
 */
export class ContextMenu {
  readonly root: HTMLDivElement;
  private open = false;
  /** Fires when the menu closes without an item being picked -- clicking
   * elsewhere, Escape, losing focus, or scrolling. Cleared before an item's
   * own close() so choosing an action never counts as dismissing one. */
  private onDismiss: (() => void) | null = null;

  constructor() {
    this.root = el('div', 'mm-contextmenu mm-panel');
    this.root.hidden = true;
    this.root.setAttribute('role', 'menu');
    document.body.appendChild(this.root);

    // Any interaction outside the menu dismisses it. pointerdown (rather
    // than click) is what makes a right-click elsewhere close this menu and
    // not immediately reopen it.
    document.addEventListener('pointerdown', (e) => {
      if (this.open && !this.root.contains(e.target as Node)) this.close();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && this.open) this.close();
    });
    window.addEventListener('blur', () => this.close());
    // Capture phase: a scroll anywhere (not just the window) invalidates the
    // menu's fixed position relative to whatever it was opened next to.
    window.addEventListener('scroll', () => this.close(), true);
  }

  isOpen(): boolean {
    return this.open;
  }

  close(): void {
    if (!this.open) return;
    this.open = false;
    this.root.hidden = true;
    const dismiss = this.onDismiss;
    this.onDismiss = null;
    dismiss?.();
  }

  /**
   * Opens the menu with its top-left at the given viewport coordinates.
   *
   * `onDismiss`, if given, fires only when the menu closes *without* one of
   * `items` being picked -- the caller's cue that whatever it staged for
   * this menu (e.g. a provisional chunk selection) was never acted on and
   * should be undone.
   */
  show(clientX: number, clientY: number, items: ContextMenuItem[], onDismiss?: () => void): void {
    this.onDismiss = onDismiss ?? null;
    this.root.replaceChildren();
    for (const item of items) {
      if ('separator' in item) {
        this.root.appendChild(el('div', 'mm-contextmenu-sep'));
        continue;
      }
      const btn = el('button', 'mm-contextmenu-item', item.label);
      btn.type = 'button';
      btn.disabled = !!item.disabled;
      btn.addEventListener('click', () => {
        // Picking an action is not a dismissal, even though it also closes
        // the menu -- clear the hook first so close() doesn't fire it.
        this.onDismiss = null;
        this.close();
        item.onSelect();
      });
      this.root.appendChild(btn);
    }

    this.root.style.left = '0';
    this.root.style.top = '0';
    this.root.hidden = false;
    this.open = true;

    // Clamped so a menu opened near an edge stays fully on screen instead of
    // spilling off it.
    const { width, height } = this.root.getBoundingClientRect();
    const left = Math.max(4, Math.min(clientX, window.innerWidth - width - 4));
    const top = Math.max(4, Math.min(clientY, window.innerHeight - height - 4));
    this.root.style.left = `${left}px`;
    this.root.style.top = `${top}px`;
  }
}

// ---------------------------------------------------------------------------
// Block info panel (grid cell 3,2).
// ---------------------------------------------------------------------------

/** The panel showing details of a clicked block. */
export class InfoPanel {
  readonly root: HTMLElement;
  private readonly body: HTMLDivElement;

  constructor() {
    this.root = el('aside', 'mm-info mm-panel');
    this.root.hidden = true;

    const header = el('div', 'mm-info-header');
    header.appendChild(el('h2', 'mm-info-title', 'BLOCK'));
    const close = el('button', 'mm-info-close');
    close.innerHTML = ICON_CLOSE;
    close.type = 'button';
    close.title = 'Close';
    close.addEventListener('click', () => this.hide());
    header.appendChild(close);

    this.body = el('div', 'mm-info-body');
    this.root.appendChild(header);
    this.root.appendChild(this.body);
  }

  hide(): void {
    this.root.hidden = true;
  }

  isVisible(): boolean {
    return !this.root.hidden;
  }

  /** Renders a pick result, including the raw JSON the API returned. */
  show(pick: PickResult): void {
    this.body.replaceChildren();

    if (!pick.found) {
      this.body.appendChild(
        el('p', 'mm-info-empty', 'This area has not been generated yet.'),
      );
      this.root.hidden = false;
      return;
    }

    const rows: Array<[string, string]> = [
      ['Position', `${pick.x}, ${pick.y}, ${pick.z}`],
      ['Chunk', `${pick.chunkX}, ${pick.chunkZ}`],
      ['Region', `r.${pick.regionX}.${pick.regionZ}.mca`],
      ['Block', prettyId(pick.block)],
      ['Biome', prettyId(pick.biome)],
    ];
    if (pick.water) rows.push(['Water surface', `Y ${pick.waterY}`]);
    rows.push(['Light', String(pick.light)]);
    rows.push(['Dimension', pick.dimension]);

    const dl = el('dl', 'mm-info-list');
    for (const [k, v] of rows) {
      dl.appendChild(el('dt', undefined, k));
      dl.appendChild(el('dd', 'mm-mono', v));
    }
    this.body.appendChild(dl);

    const details = el('details', 'mm-info-raw');
    details.appendChild(el('summary', undefined, 'Raw response'));
    const pre = el('pre', 'mm-mono');
    pre.textContent = JSON.stringify(pick, null, 2);
    details.appendChild(pre);
    this.body.appendChild(details);

    this.root.hidden = false;
  }
}
