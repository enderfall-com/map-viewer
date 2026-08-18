import type { MapEngine, MapMode } from './engine';

/** The shareable pieces of map state. */
export interface MapState {
  dimension: string;
  x: number;
  z: number;
  zoom: number;
  mode: MapMode;
  style?: string;
}

/**
 * Reads and writes map state in the URL.
 *
 * The URL carries Minecraft coordinates, never map-space or isometric ones, so
 * a link means the same place regardless of which projection it was copied
 * from. Switching to isometric and sharing the link lands the recipient over
 * the same blocks.
 *
 * Updates use `replaceState` rather than `pushState` so that panning does not
 * fill the browser history with hundreds of entries and make Back useless.
 */
export class UrlState {
  private engine: MapEngine | null = null;
  private writeTimer: number | null = null;

  /** Parses the current URL, returning whatever it specifies. */
  read(): Partial<MapState> {
    const p = new URLSearchParams(window.location.search);
    const out: Partial<MapState> = {};

    const dim = p.get('dimension') ?? p.get('dim');
    if (dim) out.dimension = dim;

    const x = num(p.get('x'));
    const z = num(p.get('z'));
    if (x !== null) out.x = x;
    if (z !== null) out.z = z;

    const zoom = num(p.get('zoom') ?? p.get('z_'));
    if (zoom !== null) out.zoom = zoom;

    const mode = p.get('mode');
    if (mode === 'top' || mode === 'iso') out.mode = mode;

    const style = p.get('style');
    if (style) out.style = style;

    return out;
  }

  /** Starts mirroring the engine's state into the URL. */
  attach(engine: MapEngine): void {
    this.engine = engine;
    const schedule = () => this.scheduleWrite();
    engine.on('moveend', schedule);
    engine.on('mode', schedule);
    engine.on('dimension', schedule);
    engine.on('style', schedule);
  }

  private scheduleWrite(): void {
    if (this.writeTimer !== null) window.clearTimeout(this.writeTimer);
    this.writeTimer = window.setTimeout(() => {
      this.writeTimer = null;
      this.write();
    }, 250);
  }

  /** Writes the engine's current state to the address bar. */
  write(): void {
    const e = this.engine;
    if (!e) return;

    const [x, z] = e.centerBlock();
    const p = new URLSearchParams();
    p.set('dimension', e.getDimension().id);
    p.set('x', String(Math.round(x)));
    p.set('z', String(Math.round(z)));
    p.set('zoom', e.zoom().toFixed(2));
    p.set('mode', e.getMode());
    if (e.getStyle() && e.getStyle() !== 'terrain') p.set('style', e.getStyle());

    const url = `${window.location.pathname}?${p.toString()}`;
    window.history.replaceState(null, '', url);
  }

  /** Builds a shareable absolute URL for the current view. */
  shareUrl(): string {
    this.write();
    return window.location.href;
  }
}

function num(v: string | null): number | null {
  if (v === null || v.trim() === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}
