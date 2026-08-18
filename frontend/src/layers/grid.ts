import Layer from 'ol/layer/Layer';
import type { FrameState } from 'ol/Map';

import { CHUNK_SIZE, floorDiv } from '../coordinates/mc';
import { ISO_HALF_HEIGHT, ISO_HALF_WIDTH, project, rotate, unrotate } from '../coordinates/iso';
import type { MapEngine } from '../map/engine';

/**
 * Chunk and block grid overlay.
 *
 * ## Why this is drawn, not tiled
 *
 * The grid must be exactly aligned to real Minecraft chunks at every zoom and
 * must not drift by a single pixel while panning or zooming. Baking it into
 * terrain tiles would make it uncacheable and un-toggleable; drawing it as
 * vector features would create a feature per line. Drawing directly to a canvas
 * from the frame state derives every line from the view transform itself, so
 * alignment is exact by construction and the cost is proportional to the lines
 * actually on screen.
 *
 * ## Isometric grids
 *
 * In isometric mode a chunk is not a square. Its boundary is the projection of
 * a 16x16 footprint at a reference elevation, which is a parallelogram. The
 * same lattice arithmetic produces it: stepping one chunk along a camera axis
 * moves a fixed offset in projected space, so the grid is drawn as two families
 * of parallel lines rather than as horizontal and vertical ones.
 */
export class GridLayer extends Layer {
  private readonly canvas: HTMLCanvasElement;
  private readonly ctx: CanvasRenderingContext2D;
  private readonly engine: MapEngine;

  private showChunks = false;
  private showBlocks = false;

  /** Maximum lines drawn per axis, a hard stop against pathological zooms. */
  private static readonly MAX_LINES = 400;

  constructor(engine: MapEngine) {
    super({ zIndex: 20 });
    this.engine = engine;
    this.canvas = document.createElement('canvas');
    this.canvas.style.position = 'absolute';
    this.canvas.style.top = '0';
    this.canvas.style.left = '0';
    this.canvas.style.pointerEvents = 'none';
    const ctx = this.canvas.getContext('2d');
    if (!ctx) throw new Error('2D canvas context unavailable');
    this.ctx = ctx;
  }

  /** Enables or disables the chunk grid. */
  setChunkGrid(on: boolean): void {
    this.showChunks = on;
    this.changed();
  }

  /** Enables or disables the per-block grid. */
  setBlockGrid(on: boolean): void {
    this.showBlocks = on;
    this.changed();
  }

  isChunkGridOn(): boolean {
    return this.showChunks;
  }

  isBlockGridOn(): boolean {
    return this.showBlocks;
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

    const cfg = this.engine.config.overlays;
    const zoom = this.engine.zoom();

    // Progressive detail: grids only appear once they can be read. The chunk
    // grid also fades in over its first zoom level so it does not snap on.
    const chunkVisible = this.showChunks && zoom >= cfg.chunkGridMinZoom - 1;
    const blockVisible = this.showBlocks && zoom >= cfg.blockGridMinZoom - 0.5;
    if (!chunkVisible && !blockVisible) return this.canvas;

    const { center, resolution } = frameState.viewState;
    const originX = center[0] - (width / 2) * resolution;
    const originY = center[1] + (height / 2) * resolution;

    // Map coordinate to CSS pixel. Y flips because map Y grows upward.
    const toPx = (mx: number, my: number): [number, number] => [
      (mx - originX) / resolution,
      (originY - my) / resolution,
    ];

    if (blockVisible) {
      const alpha = Math.min(1, Math.max(0, zoom - (cfg.blockGridMinZoom - 0.5)));
      this.drawGrid(1, `rgba(255,255,255,${0.10 * alpha})`, 1, toPx, width, height, resolution);
    }
    if (chunkVisible) {
      const alpha = Math.min(1, Math.max(0, zoom - (cfg.chunkGridMinZoom - 1)));
      this.drawGrid(
        CHUNK_SIZE,
        `rgba(120,190,255,${0.42 * alpha})`,
        1,
        toPx,
        width,
        height,
        resolution,
      );
      // Region boundaries every 32 chunks give a coarse reference at the zooms
      // where the chunk grid alone becomes a dense mesh.
      if (zoom >= cfg.chunkGridMinZoom) {
        this.drawGrid(
          CHUNK_SIZE * 32,
          `rgba(255,190,110,${0.5 * alpha})`,
          1.5,
          toPx,
          width,
          height,
          resolution,
        );
      }
    }
    return this.canvas;
  }

  /**
   * Draws one grid family at a spacing measured in Minecraft blocks.
   *
   * Line positions come from flooring the viewport's world bounds to a multiple
   * of the spacing, so every line sits on a real block boundary. Nothing is
   * accumulated frame to frame, which is what makes drift impossible.
   */
  private drawGrid(
    stepBlocks: number,
    color: string,
    lineWidth: number,
    toPx: (mx: number, my: number) => [number, number],
    width: number,
    height: number,
    resolution: number,
  ): void {
    const ctx = this.ctx;
    ctx.save();
    ctx.strokeStyle = color;
    ctx.lineWidth = lineWidth;
    ctx.beginPath();

    if (this.engine.getMode() === 'iso') {
      this.drawIsoGrid(stepBlocks, toPx, width, height, resolution);
    } else {
      this.drawTopGrid(stepBlocks, toPx, width, height, resolution);
    }

    ctx.stroke();
    ctx.restore();
  }

  /** Axis-aligned grid for top-down mode. */
  private drawTopGrid(
    stepBlocks: number,
    toPx: (mx: number, my: number) => [number, number],
    width: number,
    height: number,
    resolution: number,
  ): void {
    const ctx = this.ctx;
    const stepPx = stepBlocks / resolution;
    if (stepPx < 4) return; // too dense to read

    // Map space: X is Minecraft X, Y is -Minecraft Z. Both grids are therefore
    // still on exact block multiples.
    const [minMapX, maxMapY] = [
      this.pxToMapX(0, toPx, resolution),
      this.pxToMapY(0, toPx, resolution),
    ];
    const maxMapX = minMapX + width * resolution;
    const minMapY = maxMapY - height * resolution;

    const firstX = Math.floor(minMapX / stepBlocks) * stepBlocks;
    const firstY = Math.floor(minMapY / stepBlocks) * stepBlocks;

    let drawn = 0;
    for (let x = firstX; x <= maxMapX && drawn < GridLayer.MAX_LINES; x += stepBlocks, drawn++) {
      const px = Math.round(toPx(x, 0)[0]) + 0.5;
      ctx.moveTo(px, 0);
      ctx.lineTo(px, height);
    }
    drawn = 0;
    for (let y = firstY; y <= maxMapY && drawn < GridLayer.MAX_LINES; y += stepBlocks, drawn++) {
      const py = Math.round(toPx(0, y)[1]) + 0.5;
      ctx.moveTo(0, py);
      ctx.lineTo(width, py);
    }
  }

  /**
   * Projected grid for isometric mode.
   *
   * Chunk boundaries become two families of parallel lines running along the
   * camera's a and b axes. Each line is the projection of a real chunk edge at
   * the reference elevation, so the diamonds it forms correspond exactly to the
   * same 16x16 chunks the top-down grid shows.
   */
  private drawIsoGrid(
    stepBlocks: number,
    toPx: (mx: number, my: number) => [number, number],
    width: number,
    height: number,
    resolution: number,
  ): void {
    const ctx = this.ctx;
    // One step along a camera axis moves this far in iso units.
    const stepU = stepBlocks * ISO_HALF_WIDTH;
    const stepV = stepBlocks * ISO_HALF_HEIGHT;
    const stepPx = Math.hypot(stepU, stepV) / resolution;
    if (stepPx < 6) return;

    const cam = this.engine.camera;
    const y = this.engine.referenceY() + 1;
    const bounds = this.engine.visibleBlockBounds();

    // Work in camera space so the two line families are simple integer sweeps.
    const corners: Array<[number, number]> = [
      [bounds.minX, bounds.minZ],
      [bounds.maxX, bounds.minZ],
      [bounds.minX, bounds.maxZ],
      [bounds.maxX, bounds.maxZ],
    ];
    let aMin = Infinity;
    let aMax = -Infinity;
    let bMin = Infinity;
    let bMax = -Infinity;
    for (const [cx, cz] of corners) {
      const [a, b] = rotate(cam, cx, cz);
      aMin = Math.min(aMin, a);
      aMax = Math.max(aMax, a);
      bMin = Math.min(bMin, b);
      bMax = Math.max(bMax, b);
    }

    const aStart = floorDiv(aMin, stepBlocks) * stepBlocks;
    const bStart = floorDiv(bMin, stepBlocks) * stepBlocks;

    // Lines of constant a, swept across b.
    let drawn = 0;
    for (let a = aStart; a <= aMax && drawn < GridLayer.MAX_LINES; a += stepBlocks, drawn++) {
      const p0 = this.isoPoint(cam, a, bMin, y, toPx);
      const p1 = this.isoPoint(cam, a, bMax, y, toPx);
      ctx.moveTo(p0[0], p0[1]);
      ctx.lineTo(p1[0], p1[1]);
    }
    // Lines of constant b, swept across a.
    drawn = 0;
    for (let b = bStart; b <= bMax && drawn < GridLayer.MAX_LINES; b += stepBlocks, drawn++) {
      const p0 = this.isoPoint(cam, aMin, b, y, toPx);
      const p1 = this.isoPoint(cam, aMax, b, y, toPx);
      ctx.moveTo(p0[0], p0[1]);
      ctx.lineTo(p1[0], p1[1]);
    }
    void width;
    void height;
  }

  /** Projects a camera-space position to CSS pixels. */
  private isoPoint(
    cam: Parameters<typeof project>[0],
    a: number,
    b: number,
    y: number,
    toPx: (mx: number, my: number) => [number, number],
  ): [number, number] {
    const [x, z] = unrotate(cam, a, b);
    const [u, v] = project(cam, x, y, z);
    return toPx(u, -v);
  }

  private pxToMapX(
    px: number,
    toPx: (mx: number, my: number) => [number, number],
    resolution: number,
  ): number {
    // Invert the affine transform for a single axis.
    const originPx = toPx(0, 0)[0];
    return (px - originPx) * resolution;
  }

  private pxToMapY(
    py: number,
    toPx: (mx: number, my: number) => [number, number],
    resolution: number,
  ): number {
    const originPy = toPx(0, 0)[1];
    return (originPy - py) * resolution;
  }
}
