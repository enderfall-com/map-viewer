import Layer from 'ol/layer/Layer';
import type { FrameState } from 'ol/Map';

import type { ApiClient } from '../api/client';
import { CHUNK_SIZE, blockToMap } from '../coordinates/mc';
import { blockBoxFaces } from '../coordinates/iso';
import type { MapEngine } from '../map/engine';

/** A chunk coordinate pair, matching the backend's ChunkRef shape exactly. */
export interface ChunkKey {
  x: number;
  z: number;
}

function keyOf(c: ChunkKey): string {
  return `${c.x},${c.z}`;
}

/** Minimum depth of the selection volume, for the case where every member
 * sits at the same elevation and the terrain-derived depth would be zero --
 * a box with no height at all reads as a flat decal rather than a volume. */
const MIN_SKIRT_DEPTH = 6;

/** How far below the batch's *lowest* ground the volume's walls extend. The
 * walls have to pass the low end of the terrain they cover, or the slab reads
 * as hovering over it rather than as a box planted in it. */
const SKIRT_UNDERCUT = 3;

const FILL_TOP = 'rgba(255, 209, 102, 0.35)';
const FILL_FRONT = 'rgba(209, 171, 84, 0.35)';
const FILL_SIDE = 'rgba(173, 142, 69, 0.35)';
const STROKE = 'rgba(255, 209, 102, 0.95)';
const HOVER_STROKE = 'rgba(255, 255, 255, 0.85)';
/** Interior chunk boundaries inside one selection -- present but quiet, so
 * the batch reads as a single slab subdivided into chunks rather than as a
 * loud grid competing with its own outline. */
const SEAM_STROKE = 'rgba(255, 209, 102, 0.28)';
/** Faint, dashed, and unfilled-adjacent -- a drag preview is not committed
 * yet, so it must never be mistaken for the solid amber of a real selection. */
const PREVIEW_FILL_TOP = 'rgba(255, 209, 102, 0.16)';
const PREVIEW_FILL_FRONT = 'rgba(209, 171, 84, 0.16)';
const PREVIEW_FILL_SIDE = 'rgba(173, 142, 69, 0.16)';

/** An inclusive chunk-coordinate rectangle. */
interface ChunkRange {
  minX: number;
  minZ: number;
  maxX: number;
  maxZ: number;
}

/**
 * Chunk selection overlay.
 *
 * Owns the selected-chunk set and draws a highlight over each one, projected
 * the same way the chunk grid is: as a true parallelogram in isometric mode,
 * not an approximation. Selection state is deliberately kept here rather than
 * in `main.ts`, so the layer can redraw itself the moment the set changes
 * instead of the caller having to remember to call `changed()`.
 */
export class SelectionLayer extends Layer {
  private readonly canvas: HTMLCanvasElement;
  private readonly ctx: CanvasRenderingContext2D;
  private readonly engine: MapEngine;
  private readonly api: ApiClient;
  private readonly selected = new Map<string, ChunkKey>();
  private changeListener: (() => void) | null = null;
  /** The chunk under the cursor while selection mode is active, drawn as a
   * light preview ring so a click's effect is obvious before it happens. */
  private hoverChunk: ChunkKey | null = null;
  /** The chunk range a drag-box is currently spanning, shown as a live,
   * chunk-aligned preview in place of OpenLayers' own freehand rectangle --
   * see setPreviewRange(). Null when no drag is in progress. */
  private previewRange: ChunkRange | null = null;
  /** Real terrain surface elevation per chunk (block-top convention, i.e.
   * already `height + 1`), fetched lazily so the isometric box sits flush
   * with the ground instead of floating on a flat reference plane. Missing
   * entries fall back to the reference plane until the fetch resolves. */
  private readonly groundY = new Map<string, number>();
  private readonly pendingGroundY = new Set<string>();

  constructor(engine: MapEngine, api: ApiClient) {
    super({ zIndex: 26 });
    this.engine = engine;
    this.api = api;
    this.canvas = document.createElement('canvas');
    this.canvas.style.position = 'absolute';
    this.canvas.style.top = '0';
    this.canvas.style.left = '0';
    this.canvas.style.pointerEvents = 'none';
    const ctx = this.canvas.getContext('2d');
    if (!ctx) throw new Error('2D canvas context unavailable');
    this.ctx = ctx;
  }

  /** Notified whenever the selection set changes, e.g. to update a count. */
  onChange(fn: () => void): void {
    this.changeListener = fn;
  }

  private emitChange(): void {
    this.changed();
    this.changeListener?.();
  }

  count(): number {
    return this.selected.size;
  }

  isEmpty(): boolean {
    return this.selected.size === 0;
  }

  has(c: ChunkKey): boolean {
    return this.selected.has(keyOf(c));
  }

  list(): ChunkKey[] {
    return [...this.selected.values()];
  }

  clear(): void {
    this.groundY.clear();
    this.pendingGroundY.clear();
    if (this.selected.size === 0) return;
    this.selected.clear();
    this.emitChange();
  }

  /** Adds or removes a single chunk from the selection. */
  toggle(c: ChunkKey): void {
    const k = keyOf(c);
    if (this.selected.has(k)) this.selected.delete(k);
    else {
      this.selected.set(k, c);
      this.ensureGroundY(c);
    }
    this.emitChange();
  }

  /** Sets (or clears, via `null`) the chunk shown as a hover preview. A no-op
   * when it names the same chunk already hovered, so a stationary cursor
   * doesn't force a redraw on every pointermove tick. */
  setHover(c: ChunkKey | null): void {
    const nextKey = c ? keyOf(c) : null;
    const prevKey = this.hoverChunk ? keyOf(this.hoverChunk) : null;
    if (nextKey === prevKey) return;
    this.hoverChunk = c;
    if (c) this.ensureGroundY(c);
    this.changed();
  }

  /**
   * Shows (or clears, via `null`) a live preview of the chunk range a drag
   * box currently spans -- exactly the chunks `addRange` would commit if the
   * drag ended right now, not an arbitrary freehand rectangle. Ground
   * elevation for these chunks is whatever's already cached; a drag in
   * progress does not itself trigger new fetches, since the range (and thus
   * which chunks matter) changes on every pointer move.
   */
  setPreviewRange(range: ChunkRange | null): void {
    this.previewRange = range;
    this.changed();
  }

  private previewChunks(): ChunkKey[] {
    if (!this.previewRange) return [];
    const { minX, minZ, maxX, maxZ } = this.previewRange;
    const chunks: ChunkKey[] = [];
    for (let z = minZ; z <= maxZ; z++) {
      for (let x = minX; x <= maxX; x++) chunks.push({ x, z });
    }
    return chunks;
  }

  /** Adds every chunk in an inclusive chunk-coordinate range. */
  addRange(minX: number, minZ: number, maxX: number, maxZ: number): void {
    for (let z = minZ; z <= maxZ; z++) {
      for (let x = minX; x <= maxX; x++) {
        const c = { x, z };
        this.selected.set(keyOf(c), c);
        this.ensureGroundY(c);
      }
    }
    this.emitChange();
  }

  /**
   * Fetches the real terrain surface elevation under one chunk's centre, so
   * the isometric box can sit flush with the ground instead of floating on
   * the flat reference plane. A no-op once cached (or already in flight) --
   * the ground doesn't move, so this only ever needs to happen once per
   * chunk per dimension.
   */
  private ensureGroundY(c: ChunkKey): void {
    const key = keyOf(c);
    if (this.groundY.has(key) || this.pendingGroundY.has(key)) return;
    this.pendingGroundY.add(key);
    const dimension = this.engine.getDimension().id;
    const cx = c.x * CHUNK_SIZE + CHUNK_SIZE / 2;
    const cz = c.z * CHUNK_SIZE + CHUNK_SIZE / 2;
    this.api
      .pickTop(dimension, cx, cz, null)
      .then((res) => {
        this.pendingGroundY.delete(key);
        // The dimension may have changed while this was in flight -- a block
        // Y from the old one would be meaningless (and likely wrong) here.
        if (this.engine.getDimension().id !== dimension) return;
        if (!res) return;
        this.groundY.set(key, (res.found ? res.y : this.engine.referenceY()) + 1);
        this.changed();
      })
      .catch(() => {
        this.pendingGroundY.delete(key);
      });
  }

  /** The map-space ring (closed, 4 points) of one chunk's top face, used in
   * top-down mode where there is no vertical dimension to show a box in. */
  private ringFor(c: ChunkKey): Array<[number, number]> {
    const x0 = c.x * CHUNK_SIZE;
    const z0 = c.z * CHUNK_SIZE;
    return [
      blockToMap(x0, z0),
      blockToMap(x0 + CHUNK_SIZE, z0),
      blockToMap(x0 + CHUNK_SIZE, z0 + CHUNK_SIZE),
      blockToMap(x0, z0 + CHUNK_SIZE),
    ];
  }

  /** The projected top elevation to draw a chunk's box at: the real terrain
   * surface once known, the flat reference plane until then. */
  private topYFor(c: ChunkKey): number {
    return this.groundY.get(keyOf(c)) ?? this.engine.referenceY() + 1;
  }

  /**
   * The elevation band a whole batch is drawn as: one shared top plane, and
   * walls deep enough to reach past the lowest ground it covers.
   *
   * Deliberately one shared plane rather than each chunk following its own
   * ground: terrain varies by tens of blocks between neighbouring chunks, so
   * per-chunk elevations scatter the selection into disconnected plates at
   * unrelated heights -- unreadable as one region, whatever is done about
   * the walls between them.
   *
   * The top sits at the highest member's ground so it is never buried inside
   * a hill (and therefore invisible), and the walls run from there down past
   * the *lowest* member's ground. That makes the whole thing read as a box
   * planted in the terrain: a shallow fixed-depth skirt instead leaves the
   * slab visibly hovering wherever the ground drops away beneath it.
   */
  private batchBand(chunks: ChunkKey[]): { topY: number; bottomY: number } {
    let top = -Infinity;
    let low = Infinity;
    for (const c of chunks) {
      const y = this.topYFor(c);
      top = Math.max(top, y);
      low = Math.min(low, y);
    }
    if (!Number.isFinite(top)) {
      const fallback = this.engine.referenceY() + 1;
      return { topY: fallback, bottomY: fallback - MIN_SKIRT_DEPTH };
    }
    return { topY: top, bottomY: Math.min(low - SKIRT_UNDERCUT, top - MIN_SKIRT_DEPTH) };
  }

  /** Traces a closed ring into the current path without stroking/filling it,
   * so callers can set their own style and call fill()/stroke() themselves. */
  private tracePath(
    ctx: CanvasRenderingContext2D,
    ring: Array<[number, number]>,
    toPx: (mx: number, my: number) => [number, number],
  ): void {
    ctx.beginPath();
    ring.forEach(([mx, my], i) => {
      const [px, py] = toPx(mx, my);
      if (i === 0) ctx.moveTo(px, py);
      else ctx.lineTo(px, py);
    });
    ctx.closePath();
  }

  /** Strokes a single map-space segment, for drawing individual box edges
   * rather than whole rings. */
  private strokeSeg(
    ctx: CanvasRenderingContext2D,
    a: [number, number],
    b: [number, number],
    toPx: (mx: number, my: number) => [number, number],
  ): void {
    const [ax, ay] = toPx(a[0], a[1]);
    const [bx, by] = toPx(b[0], b[1]);
    ctx.beginPath();
    ctx.moveTo(ax, ay);
    ctx.lineTo(bx, by);
    ctx.stroke();
  }

  /** One chunk's projected faces at the batch's shared elevation. The top
   * ring runs (x,z) → (x+16,z) → (x+16,z+16) → (x,z+16), i.e. its edges are
   * north, east, south, west in that order. */
  private chunkFaces(c: ChunkKey, topY: number, bottomY: number) {
    return blockBoxFaces(
      this.engine.camera,
      c.x * CHUNK_SIZE,
      c.z * CHUNK_SIZE,
      CHUNK_SIZE,
      CHUNK_SIZE,
      topY,
      bottomY,
      bottomY,
    );
  }

  /**
   * Draws a batch of chunks as one box: a single coplanar sheet of top faces,
   * walls only where the selection actually ends (running deep enough to sink
   * into the terrain rather than hover over it), faint seams between members
   * so chunk granularity stays readable, and one bright outline around the
   * true perimeter.
   *
   * Committed selection, drag preview and lone hover are each their own
   * batch -- a preview chunk never merges with an already-committed one
   * beside it, since one is still provisional and the other isn't.
   */
  private drawChunkBatch(
    ctx: CanvasRenderingContext2D,
    chunks: ChunkKey[],
    toPx: (mx: number, my: number) => [number, number],
    fills: readonly [top: string, front: string, side: string],
    stroke: string,
    dashed: boolean,
  ): void {
    if (chunks.length === 0) return;
    const members = new Set(chunks.map(keyOf));
    const inBatch = (x: number, z: number): boolean => members.has(`${x},${z}`);
    const { topY, bottomY } = this.batchBand(chunks);

    ctx.save();
    ctx.setLineDash(dashed ? [5, 4] : []);
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = stroke;

    // Walls first, so the slab's own top faces cap them. Only the +X/+Z
    // faces this camera can see, and only on the outer boundary -- with one
    // shared elevation there is nothing to bridge between members.
    for (const c of [...chunks].sort((a, b) => a.x + a.z - (b.x + b.z))) {
      const f = this.chunkFaces(c, topY, bottomY);
      for (const [ring, fill, exposed] of [
        [f.front, fills[1], !inBatch(c.x, c.z + 1)],
        [f.side, fills[2], !inBatch(c.x + 1, c.z)],
      ] as const) {
        if (!exposed) continue;
        this.tracePath(ctx, ring, toPx);
        if (fill) {
          ctx.fillStyle = fill;
          ctx.fill();
        }
        ctx.stroke();
      }
    }

    // The slab itself: filled per chunk but never stroked here, so a seam
    // shared by two members isn't painted twice at double opacity.
    if (fills[0]) {
      ctx.fillStyle = fills[0];
      for (const c of chunks) {
        this.tracePath(ctx, this.chunkFaces(c, topY, bottomY).top, toPx);
        ctx.fill();
      }
    }

    // Interior seams, faint -- enough to read chunk boundaries, not enough to
    // compete with the outline. Each shared edge is drawn once, from the
    // member on the low side of it.
    ctx.lineWidth = 1;
    ctx.strokeStyle = SEAM_STROKE;
    for (const c of chunks) {
      const ring = this.chunkFaces(c, topY, bottomY).top;
      if (inBatch(c.x + 1, c.z)) this.strokeSeg(ctx, ring[1], ring[2], toPx);
      if (inBatch(c.x, c.z + 1)) this.strokeSeg(ctx, ring[2], ring[3], toPx);
    }

    // One bright outline tracing the batch's real silhouette.
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = stroke;
    for (const c of chunks) {
      const ring = this.chunkFaces(c, topY, bottomY).top;
      if (!inBatch(c.x, c.z - 1)) this.strokeSeg(ctx, ring[0], ring[1], toPx);
      if (!inBatch(c.x + 1, c.z)) this.strokeSeg(ctx, ring[1], ring[2], toPx);
      if (!inBatch(c.x, c.z + 1)) this.strokeSeg(ctx, ring[2], ring[3], toPx);
      if (!inBatch(c.x - 1, c.z)) this.strokeSeg(ctx, ring[3], ring[0], toPx);
    }
    ctx.restore();
  }

  /** OpenLayers calls this each frame; returning the canvas composites it. */
  override render(frameState: FrameState): HTMLElement {
    const [width, height] = frameState.size;
    const ratio = frameState.pixelRatio;

    if (this.canvas.width !== width * ratio || this.canvas.height !== height * ratio) {
      this.canvas.width = width * ratio;
      this.canvas.height = height * ratio;
      this.canvas.style.width = `${width}px`;
      this.canvas.style.height = `${height}px`;
    }
    this.ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    this.ctx.clearRect(0, 0, width, height);
    const preview = this.previewChunks();
    if (this.selected.size === 0 && !this.hoverChunk && preview.length === 0) return this.canvas;

    const { center, resolution } = frameState.viewState;
    const originX = center[0] - (width / 2) * resolution;
    const originY = center[1] + (height / 2) * resolution;
    const toPx = (mx: number, my: number): [number, number] => [
      (mx - originX) / resolution,
      (originY - my) / resolution,
    ];

    const ctx = this.ctx;
    const iso = this.engine.getMode() === 'iso';

    if (iso) {
      this.drawChunkBatch(ctx, [...this.selected.values()], toPx, [FILL_TOP, FILL_FRONT, FILL_SIDE], STROKE, false);
      // Faint and dashed, so a drag preview reads as "this is what would get
      // selected" rather than competing with an already-committed selection.
      this.drawChunkBatch(
        ctx,
        preview,
        toPx,
        [PREVIEW_FILL_TOP, PREVIEW_FILL_FRONT, PREVIEW_FILL_SIDE],
        HOVER_STROKE,
        true,
      );
      if (this.hoverChunk) {
        this.drawChunkBatch(ctx, [this.hoverChunk], toPx, ['', '', ''], HOVER_STROKE, true);
      }
      return this.canvas;
    }

    ctx.save();
    ctx.fillStyle = FILL_TOP;
    ctx.strokeStyle = STROKE;
    ctx.lineWidth = 1.5;
    for (const c of this.selected.values()) {
      this.tracePath(ctx, this.ringFor(c), toPx);
      ctx.fill();
      ctx.stroke();
    }
    ctx.restore();

    if (preview.length > 0) {
      ctx.save();
      ctx.setLineDash([5, 4]);
      ctx.fillStyle = PREVIEW_FILL_TOP;
      ctx.strokeStyle = HOVER_STROKE;
      ctx.lineWidth = 1.5;
      for (const c of preview) {
        this.tracePath(ctx, this.ringFor(c), toPx);
        ctx.fill();
        ctx.stroke();
      }
      ctx.restore();
    }

    if (this.hoverChunk) {
      ctx.save();
      ctx.setLineDash([5, 4]);
      ctx.strokeStyle = HOVER_STROKE;
      ctx.lineWidth = 1.5;
      this.tracePath(ctx, this.ringFor(this.hoverChunk), toPx);
      ctx.stroke();
      ctx.restore();
    }
    return this.canvas;
  }
}
