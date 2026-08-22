import { describe, expect, it } from 'vitest';
import {
  BASE_ZOOM,
  TILE_SIZE,
  blockToChunk,
  blockToMap,
  blockToRegion,
  blockToTile,
  blocksPerPixel,
  chunkToBlock,
  clamp,
  floorDiv,
  floorMod,
  mapToBlock,
  pixelsPerBlock,
  tileBounds,
  tileParent,
  tileSpanBlocks,
} from './mc';

// This file's own doc comment promises a mirrored backend/internal/mcmath
// test for every constant and formula here; these tests are that promise's
// frontend half, which did not previously exist.

describe('floorDiv / floorMod', () => {
  it('floors toward negative infinity, unlike JS division', () => {
    expect(floorDiv(-1, 16)).toBe(-1);
    expect(floorDiv(-16, 16)).toBe(-1);
    expect(floorDiv(-17, 16)).toBe(-2);
    expect(floorDiv(15, 16)).toBe(0);
    expect(floorDiv(16, 16)).toBe(1);
  });

  it('is always non-negative for a positive divisor', () => {
    for (let a = -20; a <= 20; a++) {
      expect(floorMod(a, 16)).toBeGreaterThanOrEqual(0);
      expect(floorMod(a, 16)).toBeLessThan(16);
    }
  });
});

describe('blockToChunk', () => {
  it('is correct across zero', () => {
    expect(blockToChunk(-1)).toBe(-1);
    expect(blockToChunk(-16)).toBe(-1);
    expect(blockToChunk(-17)).toBe(-2);
    expect(blockToChunk(0)).toBe(0);
    expect(blockToChunk(15)).toBe(0);
    expect(blockToChunk(16)).toBe(1);
  });

  it('round-trips through chunkToBlock at chunk boundaries', () => {
    for (let c = -5; c <= 5; c++) {
      expect(blockToChunk(chunkToBlock(c))).toBe(c);
    }
  });
});

describe('blockToRegion', () => {
  it('spans exactly 32 chunks per region, across zero', () => {
    expect(blockToRegion(0)).toBe(0);
    expect(blockToRegion(511)).toBe(0); // 31 chunks in, still region 0
    expect(blockToRegion(512)).toBe(1); // chunk 32, region 1
    expect(blockToRegion(-1)).toBe(-1);
    expect(blockToRegion(-512)).toBe(-1);
    expect(blockToRegion(-513)).toBe(-2);
  });
});

describe('blockToMap / mapToBlock', () => {
  it('is an exact involution (Z negates, X passes through)', () => {
    for (const [x, z] of [
      [0, 0],
      [16, 176],
      [-1024, 3000],
      [7, -7],
    ]) {
      const [mapX, mapY] = blockToMap(x, z);
      expect(mapX).toBe(x);
      expect(mapY).toBe(-z);
      const [bx, bz] = mapToBlock(mapX, mapY);
      expect(bx).toBe(x);
      expect(bz).toBe(z);
    }
  });
});

describe('blocksPerPixel / pixelsPerBlock', () => {
  it('are exact reciprocals at every zoom', () => {
    for (let z = 0; z <= 12; z++) {
      expect(blocksPerPixel(z) * pixelsPerBlock(z)).toBeCloseTo(1, 12);
    }
  });

  it('is 1 block per pixel at BASE_ZOOM', () => {
    expect(blocksPerPixel(BASE_ZOOM)).toBe(1);
    expect(pixelsPerBlock(BASE_ZOOM)).toBe(1);
  });

  it('halves/doubles per zoom level, matching the documented table', () => {
    expect(blocksPerPixel(0)).toBe(64);
    expect(blocksPerPixel(6)).toBe(1);
    expect(blocksPerPixel(10)).toBeCloseTo(1 / 16, 12);
  });
});

describe('tileSpanBlocks / blockToTile / tileBounds', () => {
  it('matches the documented span table', () => {
    expect(tileSpanBlocks(0)).toBe(32768);
    expect(tileSpanBlocks(6)).toBe(TILE_SIZE);
    expect(tileSpanBlocks(10)).toBe(32);
  });

  it('always returns a tile whose bounds actually contain the source block', () => {
    const zooms = [0, 4, 6, 8, 10];
    const points: Array<[number, number]> = [
      [0, 0],
      [16, 176],
      [-1, -1],
      [-8200, 4300],
      [8191, -8191],
    ];
    for (const zoom of zooms) {
      for (const [x, z] of points) {
        const t = blockToTile(x, z, zoom);
        const b = tileBounds(t);
        expect(x).toBeGreaterThanOrEqual(b.minX);
        expect(x).toBeLessThan(b.maxX);
        expect(z).toBeGreaterThanOrEqual(b.minZ);
        expect(z).toBeLessThan(b.maxZ);
      }
    }
  });

  it('is continuous across the X=0/Z=0 origin, no off-by-one gap or overlap', () => {
    const zoom = 6;
    const span = tileSpanBlocks(zoom);
    const west = blockToTile(-1, 0, zoom);
    const east = blockToTile(0, 0, zoom);
    expect(east.x - west.x).toBe(1);
    expect(tileBounds(west).maxX).toBe(tileBounds(east).minX);
    expect(tileBounds(east).minX).toBe(0);
    void span;
  });
});

describe('tileParent', () => {
  it("covers the child tile's own origin one level out", () => {
    for (let zoom = 3; zoom <= 8; zoom++) {
      const child = blockToTile(1234, -5678, zoom);
      const parent = tileParent(child);
      expect(parent.z).toBe(zoom - 1);
      const wantParent = blockToTile(1234, -5678, zoom - 1);
      expect(parent).toEqual(wantParent);
    }
  });
});

describe('clamp', () => {
  it('bounds a value into [lo, hi]', () => {
    expect(clamp(5, 0, 10)).toBe(5);
    expect(clamp(-5, 0, 10)).toBe(0);
    expect(clamp(15, 0, 10)).toBe(10);
  });
});
