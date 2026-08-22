import { describe, expect, it } from 'vitest';
import {
  CAMERA_ORDER,
  DEFAULT_CAMERA,
  isoToMap,
  mapToIso,
  nextCamera,
  northBearingDeg,
  project,
  rotate,
  unproject,
  unrotate,
  type IsoCamera,
} from './iso';

describe('rotate / unrotate', () => {
  it('unrotate exactly inverts rotate for every camera', () => {
    for (const cam of CAMERA_ORDER) {
      for (const [x, z] of [
        [0, 0],
        [5, -3],
        [-100, 240],
      ]) {
        const [a, b] = rotate(cam, x, z);
        const [rx, rz] = unrotate(cam, a, b);
        expect(rx).toBeCloseTo(x, 9);
        expect(rz).toBeCloseTo(z, 9);
      }
    }
  });

  it('the default camera (se) is the identity', () => {
    expect(rotate('se', 7, 11)).toEqual([7, 11]);
    expect(unrotate('se', 7, 11)).toEqual([7, 11]);
  });
});

describe('project / unproject', () => {
  it('unproject exactly inverts project for a known elevation, every camera', () => {
    const points: Array<[number, number, number]> = [
      [0, 64, 0],
      [16, 70, -176],
      [-500, 100, 340],
      [1234, -12, -5678],
    ];
    for (const cam of CAMERA_ORDER) {
      for (const [x, y, z] of points) {
        const [u, v] = project(cam, x, y, z);
        const [rx, rz] = unproject(cam, u, v, y);
        expect(rx).toBeCloseTo(x, 9);
        expect(rz).toBeCloseTo(z, 9);
      }
    }
  });

  it('one Y level shifts the unprojected column by one block in X and Z', () => {
    // PERF_PLAN.md's whole footprint-tightening argument rests on this: in a
    // 2:1 dimetric projection, elevation shifts terrain along the view ray by
    // exactly one block per Y level. If this regresses, the backend's
    // WorldFootprint/SurfaceBounds sizing assumption silently stops matching
    // what the frontend actually shows.
    const cam: IsoCamera = DEFAULT_CAMERA;
    const [u, v] = project(cam, 10, 64, 10);
    const [x0, z0] = unproject(cam, u, v, 64);
    const [x1, z1] = unproject(cam, u, v, 65);
    expect(Math.abs(x1 - x0)).toBeCloseTo(1, 9);
    expect(Math.abs(z1 - z0)).toBeCloseTo(1, 9);
  });
});

describe('isoToMap / mapToIso', () => {
  it('is an exact involution', () => {
    for (const [u, v] of [
      [0, 0],
      [42, -17],
      [-1000.5, 2000.25],
    ]) {
      const [mapX, mapY] = isoToMap(u, v);
      expect(mapX).toBe(u);
      expect(mapY).toBe(-v);
      const [ru, rv] = mapToIso(mapX, mapY);
      expect(ru).toBe(u);
      expect(rv).toBe(v);
    }
  });
});

describe('nextCamera', () => {
  it('cycles through all four cameras and back to the start', () => {
    let cam: IsoCamera = DEFAULT_CAMERA;
    const seen = new Set<IsoCamera>([cam]);
    for (let i = 0; i < CAMERA_ORDER.length - 1; i++) {
      cam = nextCamera(cam);
      seen.add(cam);
    }
    expect(seen.size).toBe(CAMERA_ORDER.length);
    expect(nextCamera(cam)).toBe(DEFAULT_CAMERA);
  });

  it('matches CAMERA_ORDER exactly', () => {
    for (let i = 0; i < CAMERA_ORDER.length; i++) {
      const next = CAMERA_ORDER[(i + 1) % CAMERA_ORDER.length];
      expect(nextCamera(CAMERA_ORDER[i])).toBe(next);
    }
  });
});

describe('northBearingDeg', () => {
  it('is a distinct bearing for every camera (each corner faces a different way)', () => {
    const bearings = CAMERA_ORDER.map(northBearingDeg);
    const unique = new Set(bearings.map((b) => Math.round(b)));
    expect(unique.size).toBe(CAMERA_ORDER.length);
  });
});
