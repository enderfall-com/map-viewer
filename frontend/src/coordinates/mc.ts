/**
 * Minecraft coordinate mathematics.
 *
 * This is the browser-side counterpart of the backend's mcmath package and must
 * agree with it exactly. Every constant and formula here has a mirrored test in
 * `backend/internal/mcmath`; if one side changes, both must.
 *
 * There is no latitude or longitude anywhere in this application. The map view
 * works directly in Minecraft coordinates.
 *
 * ## Coordinate spaces
 *
 * ```
 * block  (mcX, mcY, mcZ)   Minecraft blocks. Y up, Z south.
 * map    (mapX, mapY)      mapX = mcX, mapY = -mcZ. What OpenLayers sees.
 * tile   (z, x, y)         x indexes mcX, y indexes mcZ, both floor-divided.
 * ```
 *
 * Map space negates Z so that "up on screen" is north. Tile space does *not*
 * use map space: tile rows conventionally grow downward, which is exactly what
 * mcZ already does, so tile indices come straight from mcX and mcZ with no
 * negation and no special case at the origin.
 */

/** Edge length of a Minecraft chunk, in blocks. */
export const CHUNK_SIZE = 16;

/** Edge length of an Anvil region file, in chunks. */
export const REGION_CHUNKS = 32;

/** Pixel edge length of every tile in the pyramid. */
export const TILE_SIZE = 512;

/** The pyramid level at which one pixel is exactly one Minecraft block. */
export const BASE_ZOOM = 6;

/**
 * Euclidean floor division, rounding toward negative infinity.
 *
 * JavaScript's `/` with `Math.trunc` semantics is wrong for world coordinates:
 * block -1 belongs to chunk -1, not chunk 0. `Math.floor` on the quotient gives
 * the correct answer for every sign.
 */
export function floorDiv(a: number, b: number): number {
  return Math.floor(a / b);
}

/** Non-negative remainder consistent with {@link floorDiv}. */
export function floorMod(a: number, b: number): number {
  return a - floorDiv(a, b) * b;
}

/**
 * Converts a block coordinate on one axis to its chunk coordinate.
 *
 * Correct across zero: -1 → -1, -16 → -1, -17 → -2.
 */
export function blockToChunk(block: number): number {
  return floorDiv(Math.floor(block), CHUNK_SIZE);
}

/** Converts a chunk coordinate to its minimum block coordinate. */
export function chunkToBlock(chunk: number): number {
  return chunk * CHUNK_SIZE;
}

/** Converts a block coordinate to its region-file coordinate. */
export function blockToRegion(block: number): number {
  return floorDiv(blockToChunk(block), REGION_CHUNKS);
}

/** A position in Minecraft block space. */
export interface BlockPos {
  x: number;
  z: number;
  y?: number;
}

/** A rectangle in Minecraft block space, half-open on the max edges. */
export interface BlockBounds {
  minX: number;
  minZ: number;
  maxX: number;
  maxZ: number;
}

/**
 * Converts Minecraft X/Z into the map-space coordinate pair OpenLayers uses.
 * This is exactly reversible by {@link mapToBlock}.
 */
export function blockToMap(mcX: number, mcZ: number): [number, number] {
  return [mcX, -mcZ];
}

/** Converts map-space coordinates back to Minecraft X/Z. */
export function mapToBlock(mapX: number, mapY: number): [number, number] {
  return [mapX, -mapY];
}

/**
 * Blocks covered by one tile pixel at a pyramid level.
 * Zoom 0 = 64, zoom 6 = 1, zoom 10 = 1/16.
 *
 * This doubles as the OpenLayers resolution, because map units are Minecraft
 * blocks and resolution is map units per pixel.
 */
export function blocksPerPixel(zoom: number): number {
  return Math.pow(2, BASE_ZOOM - zoom);
}

/** Pixels covering one block at a pyramid level. */
export function pixelsPerBlock(zoom: number): number {
  return Math.pow(2, zoom - BASE_ZOOM);
}

/**
 * Edge length in blocks of one tile at a pyramid level:
 * 32768 at zoom 0, 512 at zoom 6, 32 at zoom 10.
 */
export function tileSpanBlocks(zoom: number): number {
  return TILE_SIZE * blocksPerPixel(zoom);
}

/** A tile address in the pyramid. */
export interface TilePos {
  z: number;
  x: number;
  y: number;
}

/**
 * Returns the tile containing a block position.
 *
 * Both axes come straight from Minecraft coordinates via floor division, so the
 * grid stays continuous across X=0 and Z=0.
 */
export function blockToTile(mcX: number, mcZ: number, zoom: number): TilePos {
  const span = tileSpanBlocks(zoom);
  return { z: zoom, x: floorDiv(mcX, span), y: floorDiv(mcZ, span) };
}

/** Returns the block bounds a tile covers. */
export function tileBounds(t: TilePos): BlockBounds {
  const span = tileSpanBlocks(t.z);
  return {
    minX: t.x * span,
    minZ: t.y * span,
    maxX: t.x * span + span,
    maxZ: t.y * span + span,
  };
}

/** Returns the tile one level out that contains this tile. */
export function tileParent(t: TilePos): TilePos {
  return { z: t.z - 1, x: floorDiv(t.x, 2), y: floorDiv(t.y, 2) };
}

/** Formats a coordinate for display, rounding toward the containing block. */
export function formatBlockCoord(v: number): string {
  return String(Math.floor(v));
}

/**
 * Builds a human-readable coordinate label.
 * Example: `X: 1254  Z: -8462`
 */
export function formatPosition(x: number, z: number, y?: number): string {
  const parts = [`X: ${Math.floor(x)}`];
  if (y !== undefined && Number.isFinite(y)) parts.push(`Y: ${Math.floor(y)}`);
  parts.push(`Z: ${Math.floor(z)}`);
  return parts.join('   ');
}

/** Clamps a value to a range. */
export function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}
