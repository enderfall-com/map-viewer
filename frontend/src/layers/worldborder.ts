import VectorLayer from 'ol/layer/Vector';
import VectorSource from 'ol/source/Vector';
import Feature from 'ol/Feature';
import Polygon from 'ol/geom/Polygon';
import { Stroke, Style } from 'ol/style';

import type { MapEngine } from '../map/engine';

/**
 * World border overlay.
 *
 * The border is a square in Minecraft block space, so in isometric mode it is
 * drawn as the projected diamond of that same square rather than as a screen-
 * aligned box. Its edges are subdivided so that, at low zoom where a single
 * straight segment would cut a visible chord, the outline still traces the true
 * projected boundary.
 */
export class WorldBorderLayer {
  readonly layer: VectorLayer<VectorSource>;
  private readonly engine: MapEngine;
  private readonly source = new VectorSource();
  private visible = false;

  constructor(engine: MapEngine) {
    this.engine = engine;
    this.layer = new VectorLayer({
      source: this.source,
      zIndex: 15,
      visible: false,
      style: new Style({
        stroke: new Stroke({
          color: 'rgba(255,120,120,0.75)',
          width: 2,
          lineDash: [10, 6],
        }),
      }),
    });
  }

  setVisible(v: boolean): void {
    this.visible = v;
    this.layer.setVisible(v);
    if (v) this.rebuild();
  }

  isVisible(): boolean {
    return this.visible;
  }

  /** Rebuilds the border for the current dimension and projection. */
  rebuild(): void {
    this.source.clear(true);
    const d = this.engine.getDimension();
    if (!d.worldBorder || d.worldBorder <= 0) return;

    const half = d.worldBorder / 2;
    const minX = d.centerX - half;
    const maxX = d.centerX + half;
    const minZ = d.centerZ - half;
    const maxZ = d.centerZ + half;

    // Subdividing each edge keeps the projected outline faithful in isometric
    // mode and costs nothing in top-down, where the extra points are collinear.
    const steps = 24;
    const ring = [
      ...this.edge(minX, minZ, maxX, minZ, steps),
      ...this.edge(maxX, minZ, maxX, maxZ, steps),
      ...this.edge(maxX, maxZ, minX, maxZ, steps),
      ...this.edge(minX, maxZ, minX, minZ, steps),
    ];
    ring.push(ring[0]);

    const f = new Feature({ geometry: new Polygon([ring]) });
    f.set('kind', 'worldborder');
    this.source.addFeature(f);
  }

  private edge(x0: number, z0: number, x1: number, z1: number, steps: number) {
    const pts = [];
    for (let i = 0; i < steps; i++) {
      const t = i / steps;
      pts.push(this.engine.blockToView(x0 + (x1 - x0) * t, z0 + (z1 - z0) * t));
    }
    return pts;
  }
}
