import OlMap from 'ol/Map';
import View from 'ol/View';
import TileLayer from 'ol/layer/Tile';
import XYZ from 'ol/source/XYZ';
import Projection from 'ol/proj/Projection';
import TileGrid from 'ol/tilegrid/TileGrid';
import { getCacheKey } from 'ol/tilecoord';
import { defaults as defaultInteractions } from 'ol/interaction';
import { defaults as defaultControls } from 'ol/control';
import type { Coordinate } from 'ol/coordinate';
import type { Extent } from 'ol/extent';

import {
  BASE_ZOOM,
  TILE_SIZE,
  blockToMap,
  blocksPerPixel,
  mapToBlock,
  type BlockBounds,
} from '../coordinates/mc';
import {
  DEFAULT_CAMERA,
  ISO_BLOCK_HEIGHT,
  ISO_HALF_HEIGHT,
  ISO_HALF_WIDTH,
  blockToIsoMap,
  isoToMap,
  mapToIso,
  unproject,
  type IsoCamera,
} from '../coordinates/iso';
import type { ApiClient } from '../api/client';
import type { ClientConfig, DimensionInfo, TileRevision } from '../api/types';

export type MapMode = 'top' | 'iso';

/** The camera corner and Y-slice one isometric tile source's requests carry.
 * Each of the engine's two iso slots owns one of these, mutated in place by
 * beginIsoTransition so its tileUrlFunction closure picks up the change on
 * the very next request without needing to be rebuilt. */
interface IsoTileParams {
  camera: IsoCamera;
  sliceY: number | null;
}

/** The reference elevation overlays are drawn on when terrain height is unknown. */
const DEFAULT_REFERENCE_Y = 64;

/**
 * Safety valve on how many per-tile revisions a session accumulates.
 *
 * Ordinary use never approaches this -- it exists for a server left running
 * for days under constant editing, where an unbounded map would otherwise
 * grow for as long as the tab stays open. Evicting the least-recently-changed
 * entry trades an exact but unbounded cache for one that could, in the rare
 * case an evicted tile both still sits in the browser's own HTTP cache and is
 * scrolled back into view, show that one tile stale until it next changes --
 * a far smaller cost than growing forever.
 */
const MAX_TRACKED_REVISIONS = 20_000;

/**
 * Engine events and their payload types, keyed by event name.
 *
 * A payload of `undefined` means the event carries no data. Declaring this up
 * front is what lets {@link MapEngine.on} catch a typo'd event name or a
 * wrong-shaped handler at compile time instead of the listener silently never
 * firing.
 */
/** A click's map coordinate plus the modifier keys held, so callers can tell
 * a plain click from a shift- or ctrl/cmd-click without listening to the DOM
 * event themselves. */
export interface ClickInfo {
  coordinate: Coordinate;
  shiftKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
}

export interface EngineEventMap {
  zoom: undefined;
  moveend: undefined;
  pointermove: Coordinate;
  click: ClickInfo;
  mode: MapMode;
  camera: IsoCamera;
  slice: number | null;
  style: string;
  dimension: DimensionInfo;
  /** Whether the isometric source has tile requests in flight. A cold tile
   * (PERF_PLAN.md §1) can take seconds, so this reflects real completion
   * rather than a fixed-duration flash -- long enough that a flash would
   * either cut off early or linger after the tiles are already in. */
  isoLoading: boolean;
}

/**
 * The map engine: projection, view, tile layers and mode switching.
 *
 * It owns nothing about the UI. Controls and overlays observe it through
 * {@link on}, so the core stays framework-independent and reusable.
 */
export class MapEngine {
  readonly map: OlMap;
  readonly view: View;
  readonly projection: Projection;
  readonly config: ClientConfig;
  /** The corner the isometric view is taken from. Mutable via
   * {@link setCamera}; overlays read it every frame, so they follow a rotation
   * without needing to be told about it. */
  camera: IsoCamera;

  /** The Y level the isometric view is cut off above, or null for the whole
   * world. See {@link setSliceY}. */
  private sliceY: number | null = null;

  private readonly api: ApiClient;
  private readonly layers: { top: TileLayer };
  private readonly sources: { top: XYZ };
  private readonly contourLayers: { top: TileLayer; iso: [TileLayer, TileLayer] };
  private readonly contourSources: { top: XYZ; iso: [XYZ, XYZ] };
  /**
   * Two independent iso layers/sources so a camera or slice change never
   * blanks the map: the new params load into whichever slot is currently
   * hidden while the other keeps showing its already-rendered tiles, and
   * only once the new one is ready does it become the visible one. A single
   * shared source can't do this -- `source.refresh()` bumps its cache
   * revision, which OpenLayers treats as "discard every tile" (see
   * evictRevisedTiles's doc comment for the same quirk), so reusing one
   * source for a param change means the view goes blank the instant the
   * change is requested, not once the replacement is ready.
   */
  private readonly isoLayers: [TileLayer, TileLayer];
  private readonly isoSources: [XYZ, XYZ];
  private readonly isoParams: [IsoTileParams, IsoTileParams];
  /** Index into isoLayers/isoSources/isoParams of the slot currently on top
   * when no transition is in progress. */
  private isoFront: 0 | 1 = 0;
  /** The slot a crossfade is currently loading into, or null when idle. */
  private pendingIsoSwap: 0 | 1 | null = null;
  private isoSwapTimer: number | null = null;

  private mode: MapMode = 'top';
  private style: string;
  private contours = false;
  private dimension: DimensionInfo;

  /**
   * Per-tile revisions learned from realtime updates. Tiles absent from this
   * map use the dimension baseline, so the map stays proportional to recent
   * edits rather than to world size.
   */
  private readonly revisions = new Map<string, number>();
  private baseRevision = Math.floor(Date.now() / 1000);
  /** Tile keys ("mode/z/x/y") revised since the last debounced eviction. */
  private readonly pendingRevisionKeys = new Set<string>();

  private readonly listeners = new Map<keyof EngineEventMap, Set<(payload: unknown) => void>>();
  private refreshTimer: number | null = null;
  /** Guards setMode() against a stale in-flight call clobbering a newer one. */
  private modeSwitchToken = 0;
  /** The last viewport centre resolved against real terrain, in isometric mode. */
  private resolvedCenter: { key: string; x: number; z: number; y: number } | null = null;
  /** The elevation from the most recent such resolution, kept even once the
   * view has moved on so {@link referenceY} has a real height to fall back on
   * rather than a fixed plane. Cleared when the dimension changes, since a
   * height from another world says nothing about this one. */
  private lastResolvedY: number | null = null;

  /** Isometric tile requests currently in flight, per slot, so
   * {@link EngineEventMap}'s `isoLoading` reflects real completion and a
   * crossfade knows when its back slot is ready to become the front one. */
  private isoPending: [number, number] = [0, 0];

  constructor(target: HTMLElement, config: ClientConfig, api: ApiClient, dimension: DimensionInfo) {
    this.config = config;
    this.api = api;
    this.dimension = dimension;
    this.style = config.defaultStyle || 'terrain';
    this.camera = (config.isoCamera as IsoCamera) || DEFAULT_CAMERA;

    // A Minecraft world reaches +/-30,000,000 blocks. The extent is generous
    // rather than tight so that a server with an unusual border still pans
    // freely; the world border overlay shows the real limit.
    const limit = 30_000_000;
    const extent: Extent = [-limit, -limit, limit, limit];

    // A plain cartesian projection whose units are Minecraft blocks. Declaring
    // it explicitly, rather than reusing a geographic one, is what keeps
    // latitude and longitude out of the system entirely.
    this.projection = new Projection({
      code: 'minecraft:blocks',
      units: 'm',
      extent,
      global: false,
      metersPerUnit: 1,
      axisOrientation: 'enu',
    });

    // The view ladder runs to the configured maximum so the user can keep
    // zooming past the deepest rendered tiles; the tile grid stops at the data
    // and OpenLayers magnifies from there.
    const viewResolutions = this.resolutionLadder(config.minZoom, config.maxZoom);

    this.view = new View({
      projection: this.projection,
      center: blockToMap(dimension.spawnX, dimension.spawnZ),
      resolutions: viewResolutions,
      zoom: config.defaultZoom,
      // Fractional zoom is the whole point: the camera interpolates smoothly
      // between pyramid levels instead of snapping between them.
      constrainResolution: false,
      smoothResolutionConstraint: true,
      enableRotation: false,
      // Panning far outside the world would leave the user staring at nothing.
      extent,
      showFullExtent: true,
    });

    this.sources = { top: this.createTopSource(config.topMaxDataZoom) };
    this.layers = { top: new TileLayer({ source: this.sources.top, visible: true, zIndex: 0 }) };

    this.isoParams = [
      { camera: this.camera, sliceY: null },
      { camera: this.camera, sliceY: null },
    ];
    this.isoSources = [
      this.createIsoSource(config.isoMaxDataZoom, this.isoParams[0]),
      this.createIsoSource(config.isoMaxDataZoom, this.isoParams[1]),
    ];
    this.isoLayers = [
      new TileLayer({ source: this.isoSources[0], visible: false, zIndex: 1 }),
      new TileLayer({ source: this.isoSources[1], visible: false, zIndex: 2 }),
    ];
    this.contourSources = {
      top: this.createTopSource(config.topMaxDataZoom, 'contour'),
      iso: [
        this.createIsoSource(config.isoMaxDataZoom, this.isoParams[0], 'contour'),
        this.createIsoSource(config.isoMaxDataZoom, this.isoParams[1], 'contour'),
      ],
    };
    this.contourLayers = {
      top: new TileLayer({ source: this.contourSources.top, visible: false, zIndex: 5 }),
      iso: [
        new TileLayer({ source: this.contourSources.iso[0], visible: false, zIndex: 6 }),
        new TileLayer({ source: this.contourSources.iso[1], visible: false, zIndex: 7 }),
      ],
    };
    for (const slot of [0, 1] as const) {
      this.isoSources[slot].on('tileloadstart', () => this.trackIsoTileLoad(slot, 1));
      this.isoSources[slot].on('tileloadend', () => this.trackIsoTileLoad(slot, -1));
      this.isoSources[slot].on('tileloaderror', () => this.trackIsoTileLoad(slot, -1));
      this.contourSources.iso[slot].on('tileloadstart', () => this.trackIsoTileLoad(slot, 1));
      this.contourSources.iso[slot].on('tileloadend', () => this.trackIsoTileLoad(slot, -1));
      this.contourSources.iso[slot].on('tileloaderror', () => this.trackIsoTileLoad(slot, -1));
    }

    this.map = new OlMap({
      target,
      layers: [
        this.layers.top,
        this.isoLayers[0],
        this.isoLayers[1],
        this.contourLayers.top,
        this.contourLayers.iso[0],
        this.contourLayers.iso[1],
      ],
      view: this.view,
      // OpenLayers' stock zoom buttons, attribution and rotate control are
      // replaced by the app's own chrome.
      controls: defaultControls({ zoom: false, attribution: false, rotate: false }),
      interactions: defaultInteractions({
        altShiftDragRotate: false,
        pinchRotate: false,
        // Trackpad and wheel zoom feel best with a short constant duration
        // rather than OpenLayers' default inertia.
        zoomDuration: 200,
      }),
      // Keeping a generous ring of tiles either side of the viewport is what
      // makes fast panning feel seamless: the tile is usually already there.
      moveTolerance: 1,
    });

    // A container whose size isn't settled yet at construction time (a CSS
    // layout pass not yet resolved on first paint) leaves OpenLayers sizing
    // its initial render to whatever the target measured at that instant --
    // if that was wrong, only the tiles that happened to fall inside the
    // stale viewport ever get painted, showing as small disconnected
    // fragments surrounded by nothing rather than a full map, even though
    // every tile fetch itself succeeds. A plain `resize` event listener only
    // fires for later window resizes and would miss exactly this case;
    // ResizeObserver also fires once for the element's size at the moment
    // observation starts, so it catches the initial layout settling too.
    new ResizeObserver(() => this.map.updateSize()).observe(target);

    this.view.on('change:resolution', () => {
      this.enforceIsoMinZoom();
      this.emit('zoom');
    });
    this.map.on('moveend', () => {
      void this.resolveCenter();
      this.emit('moveend');
    });
    this.map.on('pointermove', (e) => {
      if (!e.dragging) this.emit('pointermove', e.coordinate);
    });
    this.map.on('click', (e) => {
      const original = e.originalEvent;
      this.emit('click', {
        coordinate: e.coordinate,
        shiftKey: original.shiftKey,
        ctrlKey: original.ctrlKey,
        metaKey: original.metaKey,
      });
    });
  }

  /** Builds the resolution ladder for a zoom range. */
  private resolutionLadder(minZoom: number, maxZoom: number): number[] {
    const out: number[] = [];
    for (let z = minZoom; z <= maxZoom; z++) out.push(blocksPerPixel(z));
    return out;
  }

  /** Creates the top-down tile source. See {@link buildSource} for what the
   * tile grid and loading options mean. */
  private createTopSource(maxDataZoom: number, style?: string): XYZ {
    return this.buildSource(maxDataZoom, (z, x, y) => this.tileUrl('top', z, x, y, undefined, style));
  }

  /** Creates one isometric tile source bound to a specific slot's camera/
   * slice params object, read fresh on every tile request -- not `this.camera`
   * /`this.sliceY` -- so the two iso slots can each keep asking for tiles
   * under their own params independently of whichever one the engine's
   * public getters currently report (see the isoLayers field doc comment). */
  private createIsoSource(maxDataZoom: number, params: IsoTileParams, style?: string): XYZ {
    return this.buildSource(maxDataZoom, (z, x, y) => this.tileUrl('iso', z, x, y, params, style));
  }

  /**
   * Creates a tile source.
   *
   * The tile grid stops at `maxDataZoom` -- the deepest level for which the
   * server renders tiles. Beyond that OpenLayers reuses the deepest tiles and
   * scales them up, which costs no storage and, with interpolation disabled, is
   * exactly the crisp block magnification Minecraft terrain should have.
   */
  private buildSource(maxDataZoom: number, urlFor: (z: number, x: number, y: number) => string): XYZ {
    const limit = 30_000_000;
    const tileGrid = new TileGrid({
      extent: [-limit, -limit, limit, limit],
      // Tile (0,0) starts at the world origin with rows growing southward, so
      // OpenLayers' tile indices are identical to the server's.
      origin: [0, 0],
      resolutions: this.resolutionLadder(this.config.minZoom, maxDataZoom),
      tileSize: TILE_SIZE,
    });

    return new XYZ({
      projection: this.projection,
      tileGrid,
      // Minecraft worlds do not wrap; without this, negative X would fold back
      // onto positive X and show the wrong terrain west of the origin.
      wrapX: false,
      // Nearest-neighbour magnification keeps block edges hard instead of
      // smearing them, which is the whole point of a pixel-art terrain map.
      interpolate: false,
      transition: 180,
      // A few extra parallel requests smooth out fast panning.
      tileUrlFunction: (tileCoord) => {
        if (!tileCoord) return undefined;
        const [z, x, y] = tileCoord;
        return urlFor(z, x, y);
      },
    });
  }

  /**
   * Builds a tile URL from the server-supplied template, substituting the
   * tile's current cache-busting revision.
   *
   * The template (e.g. "/tiles/{dimension}/{mode}/{revision}/{z}/{x}/{y}.webp")
   * is authoritative precisely so the URL shape can change server-side --
   * a different path structure or file extension -- without a frontend
   * rebuild. Hard-coding the path here and only reading the template for its
   * file extension would silently drift from it instead.
   *
   * `iso` is the specific slot's own camera/slice params, not necessarily
   * `this.camera`/`this.sliceY` -- see createIsoSource's doc comment for why
   * that distinction is what makes the crossfade work. Omitted for top-down,
   * which has no viewing corner or slice.
   */
  private tileUrl(mode: MapMode, z: number, x: number, y: number, iso?: IsoTileParams, styleOverride?: string): string {
    const rev = this.revisions.get(`${mode}/${z}/${x}/${y}`) ?? this.baseRevision;
    const params = new URLSearchParams();
    const style = styleOverride ?? this.style;
    if (style && (styleOverride || style !== 'terrain')) params.set('style', style);
    // Only a non-default camera needs saying -- so the default view's URLs
    // stay byte-identical to before rotation existed, and keep hitting tiles
    // already cached under them.
    if (iso && iso.camera !== DEFAULT_CAMERA) params.set('cam', iso.camera);
    if (iso && iso.sliceY !== null) params.set('sliceY', String(iso.sliceY));
    const path = this.config.tileUrlTemplate
      .replace('{dimension}', encodeURIComponent(this.dimension.id))
      .replace('{mode}', mode)
      .replace('{revision}', String(rev))
      .replace('{z}', String(z))
      .replace('{x}', String(x))
      .replace('{y}', String(y));
    const query = params.toString();
    return query ? `${path}?${query}` : path;
  }

  // -------------------------------------------------------------------------
  // Events
  // -------------------------------------------------------------------------

  /** Subscribes to an engine event. Returns an unsubscribe function. */
  on<K extends keyof EngineEventMap>(event: K, fn: (payload: EngineEventMap[K]) => void): () => void {
    let set = this.listeners.get(event);
    if (!set) {
      set = new Set();
      this.listeners.set(event, set);
    }
    // The map's own storage is erased to `unknown` because a single Map
    // cannot carry a different value type per key; on/emit's generics are
    // what keep every call site fully typed despite that.
    const erased = fn as (payload: unknown) => void;
    set.add(erased);
    return () => set!.delete(erased);
  }

  private emit<K extends keyof EngineEventMap>(
    event: K,
    ...payload: EngineEventMap[K] extends undefined ? [] : [EngineEventMap[K]]
  ): void {
    this.listeners.get(event)?.forEach((fn) => fn(payload[0]));
  }

  // -------------------------------------------------------------------------
  // State
  // -------------------------------------------------------------------------

  getMode(): MapMode {
    return this.mode;
  }

  getStyle(): string {
    return this.style;
  }

  getDimension(): DimensionInfo {
    return this.dimension;
  }

  /** The deepest zoom with real tile data for the active mode. */
  maxDataZoom(): number {
    return this.mode === 'iso' ? this.config.isoMaxDataZoom : this.config.topMaxDataZoom;
  }

  /** The current fractional zoom. */
  zoom(): number {
    return this.view.getZoom() ?? this.config.defaultZoom;
  }

  /**
   * Clamps the view back to config.isoMinZoom the instant it would zoom out
   * further than that in isometric mode, leaving top-down free to zoom out
   * as far as MinZoom allows.
   *
   * This is enforced here rather than on the shared View's own min/max zoom
   * because the View spans one resolutions ladder for both modes (switching
   * mode only swaps which tile layer is visible), and because MinZoom itself
   * must stay 0 for {@link zoom} to report a value that matches the
   * server's own zoom numbering -- a resolutions-array View's zoom is an
   * index counted from MinZoom, not an absolute level, so raising MinZoom
   * globally would silently misalign every tile request.
   */
  private enforceIsoMinZoom(): void {
    const floor = this.config.isoMinZoom;
    if (this.mode !== 'iso' || floor <= 0) return;
    const z = this.view.getZoom();
    if (z !== undefined && z < floor) this.view.setZoom(floor);
  }

  /** Pixels covering one Minecraft block at the current zoom. */
  pixelsPerBlock(): number {
    const res = this.view.getResolution() ?? 1;
    // In isometric mode a block spans two iso units horizontally, so the same
    // resolution shows a block at twice the width it has in top-down.
    const unitsPerBlock = this.mode === 'iso' ? ISO_HALF_WIDTH * 2 : 1;
    return unitsPerBlock / res;
  }

  /**
   * The reference elevation used to place overlays, and to invert a screen
   * position back to a block, in isometric mode.
   *
   * This has to track the terrain actually on screen. In isometric an
   * elevation error of N blocks slides the inverted X and Z by N blocks each
   * (see {@link viewToBlockApprox}), so assuming a fixed plane while the
   * surface sits tens of blocks higher puts every cursor-driven gesture --
   * chunk hover, chunk click, drag-box corners -- multiple chunks away from
   * what the user is pointing at. The centre of the view is resolved against
   * real terrain on every settled move, so that height is used when it is
   * current, and the last one seen is kept as the fallback afterwards:
   * elevation changes gradually while panning, so a slightly stale real
   * height beats a fixed plane that may be nowhere near the ground.
   */
  referenceY(): number {
    if (this.mode === 'iso') {
      const center = this.view.getCenter() ?? [0, 0];
      if (this.resolvedCenter?.key === centerKey(center)) return this.resolvedCenter.y;
      if (this.lastResolvedY !== null) return this.lastResolvedY;
    }
    const d = this.dimension;
    if (d.minY >= 0 && d.maxY <= 128) return Math.floor((d.minY + d.maxY) / 2);
    return DEFAULT_REFERENCE_Y;
  }

  // -------------------------------------------------------------------------
  // Coordinate conversion
  // -------------------------------------------------------------------------

  /**
   * Converts a Minecraft position to map coordinates for the active mode.
   *
   * In top-down mode this is exact. In isometric mode the result depends on
   * elevation, so callers that know a block's real Y should pass it; the
   * reference plane is only a fallback for overlays whose height is unknown.
   */
  blockToView(x: number, z: number, y?: number): Coordinate {
    if (this.mode === 'iso') {
      return blockToIsoMap(this.camera, x, y ?? this.referenceY(), z);
    }
    return blockToMap(x, z);
  }

  /**
   * Converts a map coordinate to a Minecraft block position.
   *
   * In isometric mode this uses the reference plane and is therefore only an
   * estimate; {@link pickAt} resolves the true block by asking the server to
   * ray-march the terrain.
   */
  viewToBlockApprox(coord: Coordinate): [number, number] {
    if (this.mode === 'iso') {
      const [u, v] = mapToIso(coord[0], coord[1]);
      const [x, z] = unproject(this.camera, u, v, this.referenceY() + 1);
      return [Math.floor(x), Math.floor(z)];
    }
    const [x, z] = mapToBlock(coord[0], coord[1]);
    return [Math.floor(x), Math.floor(z)];
  }

  /**
   * Resolves a map coordinate to a real Minecraft block via the server.
   *
   * Top-down needs no world data, but isometric does: the pixel under the
   * cursor could belong to any column along the view ray, and only terrain
   * heights disambiguate it.
   */
  async pickAt(coord: Coordinate, key: string | null = 'pick') {
    if (this.mode === 'iso') {
      const [u, v] = mapToIso(coord[0], coord[1]);
      // The camera and the slice both have to travel with the pick: the same
      // (u, v) names a different world column from each corner, and under a
      // slice the visible surface is the cut face rather than the real
      // terrain, so a ray march without them resolves the click to a block
      // the user cannot even see.
      return this.api.pickIso(this.dimension.id, u, v, key, this.camera, this.sliceY);
    }
    const [x, z] = mapToBlock(coord[0], coord[1]);
    return this.api.pickTop(this.dimension.id, Math.floor(x), Math.floor(z), key);
  }

  /**
   * Returns the Minecraft block region currently visible.
   *
   * In isometric mode the visible rectangle in iso space corresponds to a much
   * larger, diagonal region of the world, because elevation shifts terrain
   * along the view ray. Inverting across the dimension's whole height range
   * gives the superset that overlay queries must cover so nothing pops in late.
   */
  visibleBlockBounds(): BlockBounds {
    const extent = this.view.calculateExtent(this.map.getSize() ?? [800, 600]);
    if (this.mode !== 'iso') {
      const [minX, minZ] = mapToBlock(extent[0], extent[3]);
      const [maxX, maxZ] = mapToBlock(extent[2], extent[1]);
      return { minX, minZ, maxX, maxZ };
    }

    const [u0, v0] = mapToIso(extent[0], extent[3]);
    const [u1, v1] = mapToIso(extent[2], extent[1]);
    const minY = this.dimension.minY;
    const maxY = this.dimension.maxY + 1;

    let minX = Infinity;
    let minZ = Infinity;
    let maxX = -Infinity;
    let maxZ = -Infinity;
    for (const u of [u0, u1]) {
      for (const v of [v0, v1]) {
        for (const y of [minY, maxY]) {
          const [x, z] = unproject(this.camera, u, v, y);
          minX = Math.min(minX, x);
          maxX = Math.max(maxX, x);
          minZ = Math.min(minZ, z);
          maxZ = Math.max(maxZ, z);
        }
      }
    }
    return {
      minX: Math.floor(minX),
      minZ: Math.floor(minZ),
      maxX: Math.ceil(maxX),
      maxZ: Math.ceil(maxZ),
    };
  }

  // -------------------------------------------------------------------------
  // Navigation
  // -------------------------------------------------------------------------

  /** Centres the map on a Minecraft position. */
  centerOnBlock(x: number, z: number, y?: number, animate = true): void {
    const center = this.blockToView(x, z, y);
    if (animate) {
      this.view.animate({ center, duration: 350 });
    } else {
      this.view.setCenter(center);
    }
  }

  /**
   * Returns the Minecraft position at the centre of the viewport.
   *
   * In top-down mode this is exact. In isometric mode the reference-plane
   * inverse can be dozens of blocks off wherever terrain is not at that
   * elevation, which would quietly corrupt shared URLs and make a
   * top-iso-top round trip walk the map away from where it started. So the
   * engine keeps the last centre it resolved against real terrain and returns
   * that whenever it still corresponds to the current view.
   */
  centerBlock(): [number, number] {
    const center = this.view.getCenter() ?? [0, 0];
    if (this.mode === 'iso' && this.resolvedCenter) {
      const r = this.resolvedCenter;
      if (r.key === centerKey(center)) return [r.x, r.z];
    }
    return this.viewToBlockApprox(center);
  }

  /**
   * Resolves the centre of the viewport against real terrain.
   *
   * Called after movement settles in isometric mode. It is a single request per
   * settled view, issued under its own key so it never competes with hover or
   * click picks.
   */
  private async resolveCenter(): Promise<void> {
    if (this.mode !== 'iso') {
      this.resolvedCenter = null;
      return;
    }
    const center = this.view.getCenter() ?? [0, 0];
    const key = centerKey(center);
    if (this.resolvedCenter?.key === key) return;

    const pick = await this.pickAt(center, 'center').catch(() => null);
    if (!pick || !pick.found) return;
    // Discard the answer if the view moved on while it was in flight.
    const now = this.view.getCenter() ?? [0, 0];
    if (centerKey(now) !== key) return;

    this.resolvedCenter = { key, x: pick.x, z: pick.z, y: pick.y };
    this.lastResolvedY = pick.y;
  }

  /** Zooms by a whole level. */
  zoomBy(delta: number): void {
    const z = this.view.getZoom() ?? this.config.defaultZoom;
    this.view.animate({ zoom: z + delta, duration: 200 });
  }

  setZoom(z: number, animate = false): void {
    if (animate) this.view.animate({ zoom: z, duration: 250 });
    else this.view.setZoom(z);
  }

  // -------------------------------------------------------------------------
  // Mode, style and dimension
  // -------------------------------------------------------------------------

  /**
   * Switches projection while keeping the camera over the same Minecraft
   * location.
   *
   * The two modes use different map coordinate spaces, so the centre must be
   * translated rather than left alone. Because that translation depends on the
   * terrain elevation at the centre, the server is asked for the real surface
   * height first; if it cannot answer, the reference plane is used and the view
   * lands close enough that the user does not lose their place.
   */
  async setMode(mode: MapMode): Promise<void> {
    if (mode === this.mode) return;
    // Every call gets a token; only the most recent one is allowed to apply
    // its result. Without this, two overlapping setMode() calls (e.g. a
    // double-tap of the mode shortcut) race independently, and the older
    // one's continuation can finish after the newer one and overwrite its
    // already-applied mode and centre with stale values.
    const token = ++this.modeSwitchToken;
    const centerCoord = this.view.getCenter() ?? [0, 0];

    // Resolve the current centre to a real block before changing spaces.
    let x: number;
    let z: number;
    let y = this.referenceY();

    // If the centre has already been resolved for this exact view, reuse it.
    // Re-picking would ray-march the centre pixel afresh and could land on a
    // neighbouring column, so a top-iso-top round trip would creep a few blocks
    // each time instead of returning exactly where it started.
    const cached = this.resolvedCenter;
    if (cached && cached.key === centerKey(centerCoord)) {
      x = cached.x;
      z = cached.z;
      y = cached.y;
    } else {
      // A dedicated key (rather than null) so a second overlapping setMode()
      // call cancels this pick instead of the two racing to completion in
      // whatever order the network happens to resolve them.
      const pick = await this.pickAt(centerCoord, 'modeswitch').catch(() => null);
      if (pick && pick.found) {
        x = pick.x;
        z = pick.z;
        y = pick.water && pick.waterY !== undefined ? pick.waterY : pick.y;
      } else {
        [x, z] = this.viewToBlockApprox(centerCoord);
      }
    }

    if (token !== this.modeSwitchToken) return; // superseded while awaiting the pick

    this.mode = mode;
    this.resolvedCenter = null;
    this.layers.top.setVisible(mode === 'top');
    this.contourLayers.top.setVisible(this.contours && mode === 'top');
    // Coming from top-down there is no prior iso content worth crossfading
    // from, so this shows the front slot directly rather than going through
    // beginIsoTransition; leaving iso mode hides both slots, cancelling
    // whatever transition (if any) was still in flight.
    this.isoLayers[this.isoFront].setVisible(mode === 'iso');
    this.isoLayers[1 - this.isoFront].setVisible(false);
    this.contourLayers.iso[this.isoFront].setVisible(this.contours && mode === 'iso');
    this.contourLayers.iso[1 - this.isoFront].setVisible(false);
    this.pendingIsoSwap = null;
    if (this.isoSwapTimer !== null) {
      window.clearTimeout(this.isoSwapTimer);
      this.isoSwapTimer = null;
    }
    this.enforceIsoMinZoom();

    const newCenter = this.blockToView(x, z, y);
    this.view.setCenter(newCenter);
    // The block that was just centred is known exactly, so record it rather
    // than making the next URL write fall back to the reference plane.
    if (mode === 'iso') {
      this.resolvedCenter = { key: centerKey(newCenter), x, z, y };
      this.lastResolvedY = y;
    }
    this.emit('mode', mode);
    this.emit('moveend');
  }

  /**
   * Rotates the isometric view to a different corner.
   *
   * Every corner has its own map-coordinate space -- the same world column
   * projects to a different (u, v) from each one -- so the centre has to be
   * translated through world coordinates rather than left alone, exactly as
   * {@link setMode} does between projections. The block at the centre is
   * resolved against real terrain first (reusing the already-resolved centre
   * when it is current) so the view stays put across a rotation instead of
   * drifting by the elevation error.
   *
   * A no-op outside isometric mode, which has no viewing corner.
   */
  async setCamera(camera: IsoCamera): Promise<void> {
    if (this.mode !== 'iso' || camera === this.camera) return;
    const token = ++this.modeSwitchToken;
    const centerCoord = this.view.getCenter() ?? [0, 0];

    let x: number;
    let z: number;
    let y = this.referenceY();
    const cached = this.resolvedCenter;
    if (cached && cached.key === centerKey(centerCoord)) {
      x = cached.x;
      z = cached.z;
      y = cached.y;
    } else {
      const pick = await this.pickAt(centerCoord, 'modeswitch').catch(() => null);
      if (pick && pick.found) {
        x = pick.x;
        z = pick.z;
        y = pick.water && pick.waterY !== undefined ? pick.waterY : pick.y;
      } else {
        [x, z] = this.viewToBlockApprox(centerCoord);
      }
    }
    if (token !== this.modeSwitchToken) return; // superseded while awaiting the pick

    this.camera = camera;
    this.resolvedCenter = null;
    // Every isometric tile URL now names a different camera, so the ones
    // already loaded are for the previous corner and must be replaced --
    // via a crossfade, not a same-source refresh, so the old corner stays on
    // screen until the new one is actually ready (see the isoLayers field).
    this.beginIsoTransition({ camera, sliceY: this.sliceY });

    const newCenter = this.blockToView(x, z, y);
    this.view.setCenter(newCenter);
    this.resolvedCenter = { key: centerKey(newCenter), x, z, y };
    this.lastResolvedY = y;
    this.emit('camera', camera);
    this.emit('moveend');
  }

  /** The Y level the isometric view is currently cut at, or null for none. */
  getSliceY(): number | null {
    return this.sliceY;
  }

  /**
   * Cuts the isometric view off above a Y level, or removes the cut with null.
   *
   * Unlike a rotation this does not disturb the view: the projection and the
   * map-coordinate space are unchanged, only which blocks are drawn, so the
   * tiles are simply re-requested in place -- via a crossfade, so the
   * previous slice stays visible until the new one is ready rather than the
   * map blanking out for however long the (potentially cold, server-side
   * rendered) new level takes. Sliced tiles are rendered on demand and
   * cached only in memory server-side, which is why dragging the control is
   * a live re-render rather than a lookup.
   */
  setSliceY(y: number | null): void {
    if (y === this.sliceY) return;
    this.sliceY = y;
    this.beginIsoTransition({ camera: this.camera, sliceY: y });
    this.emit('slice', y);
  }

  /**
   * Starts loading new camera/slice params into whichever iso slot is
   * currently hidden, on top of (but not yet replacing) the one still
   * showing the old params. See the isoLayers field doc comment for why this
   * exists; see completeIsoSwap for how it finishes.
   */
  private beginIsoTransition(params: IsoTileParams): void {
    const back = (1 - this.isoFront) as 0 | 1;
    this.isoParams[back].camera = params.camera;
    this.isoParams[back].sliceY = params.sliceY;
    // The back slot's own cache may hold tiles from whatever params it
    // carried the *previous* time it was a back slot; those are for neither
    // the old nor the new view and must go. This never touches the front
    // slot, so nothing currently on screen is affected.
    this.isoSources[back].refresh();
    this.contourSources.iso[back].refresh();
    this.isoLayers[back].setZIndex(2);
    this.isoLayers[this.isoFront].setZIndex(1);
    this.contourLayers.iso[back].setZIndex(7);
    this.contourLayers.iso[this.isoFront].setZIndex(6);
    this.isoLayers[back].setVisible(true);
    this.contourLayers.iso[back].setVisible(this.contours && this.mode === 'iso');
    this.pendingIsoSwap = back;
    if (this.isoSwapTimer !== null) window.clearTimeout(this.isoSwapTimer);
    // Fallback in case tiles never finish loading (offline, a request that
    // never resolves): complete the swap anyway after a bounded wait rather
    // than leaving the old view stuck on top forever. Chosen well above a
    // typical cold isoVoxel render (PERF_PLAN.md §1: usually well under 2s).
    this.isoSwapTimer = window.setTimeout(() => this.completeIsoSwap(back), 6000);
    this.map.render();
    // If every tile the back slot needs was already cached, tileloadstart/end
    // never fire at all (no network request happens), so trackIsoTileLoad
    // would never see the 0 that normally triggers completion. Checking once
    // more on the next frame, after OpenLayers has had a chance to actually
    // request anything, catches that all-cached case too.
    requestAnimationFrame(() => {
      if (this.pendingIsoSwap === back && this.isoPending[back] === 0) {
        this.completeIsoSwap(back);
      }
    });
  }

  /** Promotes slot to the front (visible, authoritative) and hides the other
   * one, completing a crossfade started by beginIsoTransition. A no-op if
   * a newer transition has already superseded this one. */
  private completeIsoSwap(slot: 0 | 1): void {
    if (this.pendingIsoSwap !== slot) return;
    this.pendingIsoSwap = null;
    if (this.isoSwapTimer !== null) {
      window.clearTimeout(this.isoSwapTimer);
      this.isoSwapTimer = null;
    }
    this.isoFront = slot;
    this.isoLayers[(1 - slot) as 0 | 1].setVisible(false);
    this.contourLayers.iso[(1 - slot) as 0 | 1].setVisible(false);
    this.map.render();
  }

  /** Updates the in-flight iso tile count for one slot and emits
   * `isoLoading` on 0<->>0 transitions only, so listeners get a clean
   * start/stop signal instead of a spammy count on every single tile. Also
   * completes a pending crossfade once its slot's tiles have all arrived. */
  private trackIsoTileLoad(slot: 0 | 1, delta: number): void {
    const wasAny = this.isoPending[0] > 0 || this.isoPending[1] > 0;
    this.isoPending[slot] = Math.max(0, this.isoPending[slot] + delta);
    const isAny = this.isoPending[0] > 0 || this.isoPending[1] > 0;
    if (wasAny !== isAny) this.emit('isoLoading', isAny);

    if (this.pendingIsoSwap === slot && this.isoPending[slot] === 0) {
      this.completeIsoSwap(slot);
    }
  }

  /** Changes the render style, which is a different tile variant. */
  setStyle(style: string): void {
    if (style === this.style) return;
    this.style = style;
    this.refreshTiles();
    this.emit('style', style);
  }

  /** Shows or hides the independent contour raster overlay. */
  setContours(enabled: boolean): void {
    this.contours = enabled;
    this.contourLayers.top.setVisible(enabled && this.mode === 'top');
    this.contourLayers.iso[this.isoFront].setVisible(enabled && this.mode === 'iso');
    const back = (1 - this.isoFront) as 0 | 1;
    this.contourLayers.iso[back].setVisible(enabled && this.mode === 'iso' && this.pendingIsoSwap === back);
    this.map.render();
  }

  /**
   * Switches dimension without reloading the application.
   *
   * Each dimension has its own tile pyramid and its own height range, so the
   * tile caches are dropped and the view is re-centred on that dimension's
   * spawn.
   */
  setDimension(dimension: DimensionInfo, keepPosition = false): void {
    if (dimension.id === this.dimension.id) return;
    const [x, z] = keepPosition ? this.centerBlock() : [dimension.spawnX, dimension.spawnZ];

    this.dimension = dimension;
    this.resolvedCenter = null;
    // Before blockToView() below consults referenceY() for the new dimension:
    // the old world's surface height is meaningless here.
    this.lastResolvedY = null;
    this.revisions.clear();
    this.baseRevision = Math.floor(Date.now() / 1000);
    this.refreshTiles();
    this.view.setCenter(this.blockToView(x, z));
    this.emit('dimension', dimension);
    this.emit('moveend');
  }

  // -------------------------------------------------------------------------
  // Tile invalidation
  // -------------------------------------------------------------------------

  /**
   * Applies revisions pushed by the realtime channel.
   *
   * Only the named tiles change URL, so the browser refetches exactly those and
   * serves everything else from its cache. Refreshing is debounced because a
   * player mining a wall produces a burst of updates that all resolve to the
   * same handful of tiles.
   */
  applyTileRevisions(revs: TileRevision[]): void {
    let changed = false;
    for (const r of revs) {
      const key = `${r.mode}/${r.z}/${r.x}/${r.y}`;
      if (this.revisions.get(key) !== r.revision) {
        // Delete before set so re-touching a key also moves it to the end of
        // the map's iteration order, which is what makes the trim below evict
        // the least-recently-changed tile rather than a fixed startup order.
        this.revisions.delete(key);
        this.revisions.set(key, r.revision);
        this.pendingRevisionKeys.add(key);
        changed = true;
      }
    }
    this.trimRevisions();
    if (!changed) return;

    if (this.refreshTimer !== null) window.clearTimeout(this.refreshTimer);
    this.refreshTimer = window.setTimeout(() => {
      this.refreshTimer = null;
      this.evictRevisedTiles();
    }, 300);
  }

  /** Enforces MAX_TRACKED_REVISIONS, dropping the oldest entries first. */
  private trimRevisions(): void {
    while (this.revisions.size > MAX_TRACKED_REVISIONS) {
      const oldest = this.revisions.keys().next().value;
      if (oldest === undefined) break;
      this.revisions.delete(oldest);
    }
  }

  /**
   * Drops exactly the tiles named by pendingRevisionKeys from each layer's
   * render cache, then requests a redraw so only those are refetched.
   *
   * `source.refresh()` looks like the obvious tool here but is not: OpenLayers'
   * canvas tile renderer treats a source revision bump as "clear the whole
   * per-source cache" (see CanvasTileLayerRenderer.prepareFrame), which would
   * evict every tile currently in view -- not just the handful this batch of
   * chunk.updated events actually touched. Reaching into the renderer's tile
   * cache directly is what makes the invalidation as targeted as the server
   * revision it is responding to.
   */
  private evictRevisedTiles(): void {
    for (const key of this.pendingRevisionKeys) {
      const [mode, zStr, xStr, yStr] = key.split('/');
      const z = Number(zStr);
      const x = Number(xStr);
      const y = Number(yStr);
      if (mode === 'top') {
        this.evictFromLayer(this.layers.top, this.sources.top, z, x, y);
        this.evictFromLayer(this.contourLayers.top, this.contourSources.top, z, x, y);
      } else {
        // Either iso slot -- front, back, or both -- could hold this tile,
        // so both are checked; containsKey makes checking the one that
        // doesn't have it a no-op rather than an error.
        this.evictFromLayer(this.isoLayers[0], this.isoSources[0], z, x, y);
        this.evictFromLayer(this.isoLayers[1], this.isoSources[1], z, x, y);
        this.evictFromLayer(this.contourLayers.iso[0], this.contourSources.iso[0], z, x, y);
        this.evictFromLayer(this.contourLayers.iso[1], this.contourSources.iso[1], z, x, y);
      }
    }
    this.pendingRevisionKeys.clear();
    this.map.render();
  }

  /** Drops one tile from one layer's render cache, if present. Shared body
   * for evictRevisedTiles's top and iso cases. */
  private evictFromLayer(layer: TileLayer, source: XYZ, z: number, x: number, y: number): void {
    const cache = layer.getRenderer()?.getTileCache();
    if (!cache) return;
    const cacheKey = getCacheKey(source, source.getKey(), z, x, y);
    if (cache.containsKey(cacheKey)) cache.remove(cacheKey);
  }

  /** Drops every cached tile so current URLs are requested again, e.g. after
   * a style or dimension change. Unlike a camera or slice change this is not
   * crossfaded -- a style/dimension switch is a deliberate full content
   * change, not a re-render of the same view, so an instant swap reads as
   * correct rather than as the map having briefly broken. */
  refreshTiles(): void {
    this.sources.top.refresh();
    this.isoSources[0].refresh();
    this.isoSources[1].refresh();
    this.contourSources.top.refresh();
    this.contourSources.iso[0].refresh();
    this.contourSources.iso[1].refresh();
  }

  /** Recomputes size after a layout change, e.g. entering fullscreen. */
  updateSize(): void {
    this.map.updateSize();
  }
}

/** Re-exported so overlays can share the engine's projection constants. */
export { ISO_HALF_WIDTH, ISO_HALF_HEIGHT, ISO_BLOCK_HEIGHT, isoToMap, BASE_ZOOM };

/**
 * Quantises a map coordinate into a cache key.
 *
 * Rounding to a whole map unit means sub-pixel jitter from animation does not
 * invalidate a resolved centre, while any real movement does.
 */
function centerKey(c: Coordinate): string {
  return `${Math.round(c[0])}:${Math.round(c[1])}`;
}
