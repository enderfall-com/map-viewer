/** Types mirroring the backend's JSON API. */

export interface DimensionInfo {
  id: string;
  name: string;
  minY: number;
  maxY: number;
  worldBorder?: number;
  centerX: number;
  centerZ: number;
  enabled: boolean;
  spawnX: number;
  spawnZ: number;
  hasCeiling: boolean;
}

export interface OverlayThresholds {
  chunkGridMinZoom: number;
  blockGridMinZoom: number;
  playersMinZoom: number;
  claimsMinZoom: number;
  regionsMinZoom: number;
  markersMinZoom: number;
  labelsMinZoom: number;
}

export interface IsoClientConfig {
  halfWidth: number;
  halfHeight: number;
  blockHeight: number;
  minDirectZoom: number;
}

export interface ClientConfig {
  title: string;
  tileUrlTemplate: string;
  tileSize: number;
  baseZoom: number;
  minZoom: number;
  maxZoom: number;
  /** Raises the effective minimum zoom (furthest zoom-out) in isometric mode
   * only; <= 0 means no restriction beyond minZoom. Enforced by MapEngine,
   * not the shared OpenLayers View -- see engine.ts. */
  isoMinZoom: number;
  /** Deepest zoom for which top-down tiles exist; beyond it the client magnifies. */
  topMaxDataZoom: number;
  /** Deepest zoom for which isometric tiles exist. */
  isoMaxDataZoom: number;
  defaultZoom: number;
  defaultDimension: string;
  defaultMode: string;
  defaultStyle: string;
  modes: string[];
  styles: string[];
  overlays: OverlayThresholds;
  isoCamera: string;
  iso: IsoClientConfig;
  live: boolean;
  constrainToBorder: boolean;
  dimensions: DimensionInfo[];
}

export interface Player {
  uuid: string;
  name: string;
  dimension: string;
  x: number;
  y: number;
  z: number;
  rotation: number;
  health?: number;
  armor?: number;
  updatedAt: string;
}

export type AreaShape = 'rect' | 'polygon' | 'chunks';

export interface Point {
  x: number;
  z: number;
}

export interface ChunkRef {
  x: number;
  z: number;
}

export interface Area {
  id: string;
  kind: string;
  name?: string;
  owner?: string;
  dimension: string;
  shape: AreaShape;
  minX: number;
  minZ: number;
  maxX: number;
  maxZ: number;
  polygon?: Point[];
  chunks?: ChunkRef[];
  fill?: string;
  stroke?: string;
  fillOpacity?: number;
  label?: string;
  minZoom?: number;
  maxZoom?: number;
  properties?: Record<string, unknown>;
}

export interface Marker {
  id: string;
  kind: string;
  name: string;
  dimension: string;
  x: number;
  y?: number;
  z: number;
  icon?: string;
  color?: string;
  minZoom?: number;
  maxZoom?: number;
  properties?: Record<string, unknown>;
}

export interface FeatureSet {
  areas: Area[];
  markers: Marker[];
  players: Player[];
  truncated: boolean;
}

export interface SearchResult {
  type: string;
  id: string;
  name: string;
  dimension: string;
  x: number;
  z: number;
}

export interface PickResult {
  dimension: string;
  x: number;
  y: number;
  z: number;
  chunkX: number;
  chunkZ: number;
  regionX: number;
  regionZ: number;
  block: string;
  biome: string;
  water?: boolean;
  waterY?: number;
  light: number;
  found: boolean;
}

/** A tile whose revision changed, as delivered over the realtime channel. */
export interface TileRevision {
  mode: string;
  z: number;
  x: number;
  y: number;
  revision: number;
}

export interface ChunkUpdatedEvent {
  chunkX: number;
  chunkZ: number;
  revision: number;
  tiles: TileRevision[];
}

export interface RealtimeEvent {
  type: string;
  dimension?: string;
  data?: unknown;
}
