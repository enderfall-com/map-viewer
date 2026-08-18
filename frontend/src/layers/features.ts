import VectorLayer from 'ol/layer/Vector';
import VectorSource from 'ol/source/Vector';
import Feature from 'ol/Feature';
import Polygon from 'ol/geom/Polygon';
import MultiPolygon from 'ol/geom/MultiPolygon';
import Point from 'ol/geom/Point';
import { Fill, Stroke, Style, Text, Circle as CircleStyle } from 'ol/style';
import type { Coordinate } from 'ol/coordinate';
import type { FeatureLike } from 'ol/Feature';

import { CHUNK_SIZE, chunkToBlock } from '../coordinates/mc';
import type { Area, Marker } from '../api/types';
import type { MapEngine } from '../map/engine';

/** Default colours per feature kind, overridable per feature by the server. */
const KIND_COLORS: Record<string, { fill: string; stroke: string }> = {
  claim: { fill: '#3ddc97', stroke: '#6ff0bd' },
  region: { fill: '#4c8dff', stroke: '#8ab4ff' },
  protected: { fill: '#e5484d', stroke: '#ff6369' },
  forceload: { fill: '#ff9f4c', stroke: '#ffc27d' },
  default: { fill: '#8a8f98', stroke: '#c0c5cc' },
};

const MARKER_COLORS: Record<string, string> = {
  spawn: '#ffd166',
  warp: '#4cc9f0',
  home: '#3ddc97',
  waypoint: '#c77dff',
  poi: '#f0a35e',
  default: '#c0c5cc',
};

/** Distinct, deterministic colours for claim owners, so the same name always
 * gets the same colour both on the map and in the claims legend without the
 * server having to assign or send one. */
const OWNER_PALETTE = [
  '#7ee787', '#8ea6c8', '#e07b7b', '#e6c07a', '#b48ead',
  '#5fa8ff', '#3ddc97', '#ff9f4c', '#ffd166', '#c77dff',
];

/** A simple, fast string hash (no cryptographic properties needed, just
 * even distribution across the palette). */
function hashOwner(owner: string): number {
  let h = 0;
  for (let i = 0; i < owner.length; i++) h = (h * 31 + owner.charCodeAt(i)) >>> 0;
  return h;
}

export function colorForOwner(owner: string): string {
  return OWNER_PALETTE[hashOwner(owner) % OWNER_PALETTE.length];
}

/** One row of the claims legend. */
export interface ClaimLegendEntry {
  owner: string;
  color: string;
  chunks: number;
}

/**
 * Vector overlay for claims, regions and points of interest.
 *
 * ## Why vectors rather than baked tiles
 *
 * These change independently of terrain and must be toggleable, clickable and
 * restylable without regenerating anything. Keeping them as vector geometry
 * also means a claim's true chunk-aligned outline is drawn exactly, rather than
 * being resampled through a raster pyramid.
 *
 * ## Why the geometry is rebuilt on mode changes
 *
 * A claim is a rectangle in Minecraft space, but in isometric mode it projects
 * to a parallelogram. Rebuilding from the source block coordinates keeps both
 * views describing literally the same region of the world.
 *
 * Isometric overlays are drawn on the dimension's reference elevation, because
 * a claim covers a whole column of the world rather than sitting at one height.
 * Markers that carry a real Y are placed at it.
 */
export class FeatureLayers {
  readonly areas: VectorLayer<VectorSource>;
  readonly markers: VectorLayer<VectorSource>;

  private readonly engine: MapEngine;
  private readonly areaSource = new VectorSource();
  private readonly markerSource = new VectorSource();

  private currentAreas: Area[] = [];
  private currentMarkers: Marker[] = [];

  private showClaims = true;
  private showRegions = true;
  private showMarkers = true;
  private showForceLoaded = true;

  /** The integer zoom level as of the last rebuild, for {@link onZoomChanged}. */
  private lastZoomBucket: number | null = null;

  constructor(engine: MapEngine) {
    this.engine = engine;

    this.areas = new VectorLayer({
      source: this.areaSource,
      zIndex: 10,
      style: (f) => this.areaStyle(f),
      // Areas are numerous and mostly static, so the canvas renderer's
      // per-frame cost matters more than hit-detection precision.
      updateWhileAnimating: false,
      updateWhileInteracting: false,
    });

    this.markers = new VectorLayer({
      source: this.markerSource,
      zIndex: 30,
      style: (f) => this.markerStyle(f),
      updateWhileAnimating: true,
      updateWhileInteracting: true,
    });
  }

  /** Toggles claim visibility. */
  setClaimsVisible(v: boolean): void {
    this.showClaims = v;
    this.rebuildAreas();
  }

  /** Toggles region visibility. */
  setRegionsVisible(v: boolean): void {
    this.showRegions = v;
    this.rebuildAreas();
  }

  /** Toggles force-loaded chunk visibility. */
  setForceLoadedVisible(v: boolean): void {
    this.showForceLoaded = v;
    this.rebuildAreas();
  }

  /** Toggles marker visibility. */
  setMarkersVisible(v: boolean): void {
    this.showMarkers = v;
    this.markers.setVisible(v);
  }

  /** Replaces the area set, e.g. after a viewport query. */
  setAreas(areas: Area[]): void {
    this.currentAreas = areas;
    this.rebuildAreas();
  }

  /** Claim chunk counts grouped by owner, for the claims legend. Areas
   * without an exact chunk list (non chunk-shaped claims) fall back to a
   * bounding-box estimate rather than being dropped from the count. */
  legendEntries(): ClaimLegendEntry[] {
    const byOwner = new Map<string, number>();
    for (const a of this.currentAreas) {
      if (a.kind !== 'claim') continue;
      const owner = a.owner ?? 'Unclaimed';
      const count = a.chunks?.length ?? this.approxChunkCount(a);
      byOwner.set(owner, (byOwner.get(owner) ?? 0) + count);
    }
    return [...byOwner.entries()]
      .map(([owner, chunks]) => ({ owner, chunks, color: colorForOwner(owner) }))
      .sort((a, b) => b.chunks - a.chunks);
  }

  private approxChunkCount(a: Area): number {
    const w = Math.max(1, Math.round((a.maxX - a.minX) / CHUNK_SIZE));
    const h = Math.max(1, Math.round((a.maxZ - a.minZ) / CHUNK_SIZE));
    return w * h;
  }

  /** Replaces the marker set. */
  setMarkers(markers: Marker[]): void {
    this.currentMarkers = markers;
    this.rebuildMarkers();
  }

  /** Re-projects everything, called when the mode or dimension changes. */
  reproject(): void {
    this.lastZoomBucket = Math.floor(this.engine.zoom());
    this.rebuildAreas();
    this.rebuildMarkers();
  }

  /**
   * Rebuilds only if the zoom has crossed an integer boundary since the last
   * rebuild. Meant for the engine's 'zoom' event, which fires on every
   * resolution tick of a zoom animation -- potentially dozens of times per
   * gesture -- yet every threshold that decides which features are actually
   * shown (claimsMinZoom, regionsMinZoom, markersMinZoom, and each feature's
   * own minZoom/maxZoom) is a whole number, so nothing they gate can change
   * within the same integer zoom level. Label visibility inside the style
   * functions already reads zoom live on every render and needs no rebuild.
   */
  onZoomChanged(): void {
    const bucket = Math.floor(this.engine.zoom());
    if (bucket === this.lastZoomBucket) return;
    this.reproject();
  }

  private visibleAtZoom(minZoom?: number, maxZoom?: number): boolean {
    const z = this.engine.zoom();
    if (minZoom !== undefined && z < minZoom) return false;
    if (maxZoom !== undefined && z > maxZoom) return false;
    return true;
  }

  private rebuildAreas(): void {
    this.areaSource.clear(true);
    const cfg = this.engine.config.overlays;
    const zoom = this.engine.zoom();

    const features: Feature[] = [];
    for (const a of this.currentAreas) {
      if (a.kind === 'claim') {
        if (!this.showClaims || zoom < cfg.claimsMinZoom) continue;
      } else if (a.kind === 'forceload') {
        if (!this.showForceLoaded) continue;
      } else {
        if (!this.showRegions || zoom < cfg.regionsMinZoom) continue;
      }
      if (!this.visibleAtZoom(a.minZoom, a.maxZoom)) continue;

      const geom = this.areaGeometry(a);
      if (!geom) continue;
      const f = new Feature({ geometry: geom });
      f.setId(a.id);
      f.set('area', a);
      f.set('kind', a.kind);
      features.push(f);
    }
    this.areaSource.addFeatures(features);
  }

  /** Builds an area's geometry in the active mode's coordinate space. */
  private areaGeometry(a: Area): Polygon | MultiPolygon | null {
    switch (a.shape) {
      case 'polygon': {
        if (!a.polygon || a.polygon.length < 3) return null;
        const ring = a.polygon.map((p) => this.project(p.x, p.z));
        ring.push(ring[0]);
        return new Polygon([ring]);
      }
      case 'chunks': {
        if (!a.chunks || a.chunks.length === 0) return null;
        // One polygon per chunk keeps the outline exactly chunk-aligned, which
        // is how the underlying claim actually works.
        const polys = a.chunks.map((c) => {
          const x0 = chunkToBlock(c.x);
          const z0 = chunkToBlock(c.z);
          return [this.rectRing(x0, z0, x0 + CHUNK_SIZE, z0 + CHUNK_SIZE)];
        });
        return new MultiPolygon(polys);
      }
      default:
        return new Polygon([this.rectRing(a.minX, a.minZ, a.maxX, a.maxZ)]);
    }
  }

  /**
   * Builds a rectangle ring in map space.
   *
   * In isometric mode the four corners project individually, turning the
   * rectangle into the parallelogram that actually corresponds to that region
   * of the world.
   */
  private rectRing(minX: number, minZ: number, maxX: number, maxZ: number): Coordinate[] {
    const pts: Coordinate[] = [
      this.project(minX, minZ),
      this.project(maxX, minZ),
      this.project(maxX, maxZ),
      this.project(minX, maxZ),
    ];
    pts.push(pts[0]);
    return pts;
  }

  private project(x: number, z: number, y?: number): Coordinate {
    return this.engine.blockToView(x, z, y);
  }

  private rebuildMarkers(): void {
    this.markerSource.clear(true);
    const cfg = this.engine.config.overlays;
    const zoom = this.engine.zoom();
    if (!this.showMarkers || zoom < cfg.markersMinZoom) return;

    const features: Feature[] = [];
    for (const m of this.currentMarkers) {
      if (!this.visibleAtZoom(m.minZoom, m.maxZoom)) continue;
      // Spawn is important enough to show at every zoom the layer is on.
      const geom = new Point(this.project(m.x, m.z, m.y));
      const f = new Feature({ geometry: geom });
      f.setId(m.id);
      f.set('marker', m);
      features.push(f);
    }
    this.markerSource.addFeatures(features);
  }

  private areaStyle(f: FeatureLike): Style {
    const a = f.get('area') as Area;
    const palette = KIND_COLORS[a.kind] ?? KIND_COLORS.default;
    // Claims are coloured per owner (matching the claims legend) rather than
    // a single flat green, so adjacent claims by different owners are
    // visually distinguishable on the map itself.
    const ownerColor = a.kind === 'claim' && a.owner ? colorForOwner(a.owner) : undefined;
    const fill = a.fill ?? ownerColor ?? palette.fill;
    const stroke = a.stroke ?? ownerColor ?? palette.stroke;
    const opacity = a.fillOpacity ?? (a.kind === 'claim' ? 0.16 : 0.12);

    const zoom = this.engine.zoom();
    const showLabel =
      zoom >= this.engine.config.overlays.labelsMinZoom && (a.label || a.name || a.owner);

    return new Style({
      fill: new Fill({ color: hexToRgba(fill, opacity) }),
      stroke: new Stroke({ color: hexToRgba(stroke, 0.85), width: a.kind === 'claim' ? 1 : 1.5 }),
      text: showLabel
        ? new Text({
            text: a.label ?? a.name ?? a.owner ?? '',
            font: '500 12px Inter, system-ui, sans-serif',
            fill: new Fill({ color: '#e8ecf1' }),
            stroke: new Stroke({ color: 'rgba(6,8,12,0.85)', width: 3 }),
            overflow: false,
          })
        : undefined,
    });
  }

  private markerStyle(f: FeatureLike): Style {
    const m = f.get('marker') as Marker;
    const color = m.color ?? MARKER_COLORS[m.kind] ?? MARKER_COLORS.default;
    const zoom = this.engine.zoom();
    const showLabel = zoom >= this.engine.config.overlays.labelsMinZoom;
    const radius = m.kind === 'spawn' ? 7 : 5;

    return new Style({
      image: new CircleStyle({
        radius,
        fill: new Fill({ color: hexToRgba(color, 0.95) }),
        stroke: new Stroke({ color: 'rgba(10,12,16,0.9)', width: 2 }),
      }),
      text: showLabel
        ? new Text({
            text: m.name,
            offsetY: -(radius + 10),
            font: '500 12px Inter, system-ui, sans-serif',
            fill: new Fill({ color: '#e8ecf1' }),
            stroke: new Stroke({ color: 'rgba(6,8,12,0.9)', width: 3 }),
          })
        : undefined,
    });
  }
}

/** Converts #rrggbb plus an alpha into a CSS rgba string. */
export function hexToRgba(hex: string, alpha: number): string {
  const h = hex.replace('#', '');
  const full = h.length === 3 ? h.split('').map((c) => c + c).join('') : h;
  const n = parseInt(full.slice(0, 6), 16);
  const r = (n >> 16) & 255;
  const g = (n >> 8) & 255;
  const b = n & 255;
  return `rgba(${r},${g},${b},${alpha})`;
}
