import type {
  Area,
  ChunkHeight,
  ChunkRef,
  ClientConfig,
  DimensionInfo,
  FeatureSet,
  Marker,
  PickResult,
  Player,
  SearchResult,
} from './types';
import type { BlockBounds } from '../coordinates/mc';
import { asArray, isRecord } from './validate';

/** Outcome of a chunk-selection mutation. `conflicts`, when present, names
 * exactly the chunks that blocked a claim so the caller can retry cleanly. */
export type ActionResult<T> =
  | { ok: true; value: T }
  | { ok: false; message: string; conflicts?: ChunkRef[] };

/**
 * HTTP client for the map API.
 *
 * Every request that the map can issue repeatedly while the user is moving --
 * features, picks, searches -- is cancellable and de-duplicated here rather
 * than in the calling layer, so panning never queues an unbounded backlog of
 * stale requests behind the one whose answer actually matters.
 */
export class ApiClient {
  private readonly base: string;
  /** In-flight requests keyed by purpose, so a newer one supersedes an older. */
  private readonly inflight = new Map<string, AbortController>();

  constructor(base = '') {
    this.base = base.replace(/\/$/, '');
  }

  private url(path: string, params?: Record<string, string | number | undefined>): string {
    const u = new URL(this.base + path, window.location.origin);
    if (params) {
      for (const [k, v] of Object.entries(params)) {
        if (v !== undefined && v !== null && v !== '') u.searchParams.set(k, String(v));
      }
    }
    return u.toString();
  }

  /**
   * Performs a GET, cancelling any previous request under the same key.
   *
   * Returns `null` when the request was superseded or aborted, which callers
   * treat as "no news" rather than as an error -- an aborted fetch during a pan
   * is the normal case, not a failure.
   */
  private async get<T>(
    key: string | null,
    path: string,
    params?: Record<string, string | number | undefined>,
  ): Promise<T | null> {
    if (key) {
      this.inflight.get(key)?.abort();
    }
    const controller = new AbortController();
    if (key) this.inflight.set(key, controller);

    try {
      const res = await fetch(this.url(path, params), {
        signal: controller.signal,
        headers: { Accept: 'application/json' },
      });
      if (!res.ok) {
        throw new Error(`${path} responded ${res.status}`);
      }
      return (await res.json()) as T;
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return null;
      throw err;
    } finally {
      if (key && this.inflight.get(key) === controller) {
        this.inflight.delete(key);
      }
    }
  }

  /**
   * Performs a POST with a JSON body.
   *
   * Unlike get(), there is no cancel-by-key here: claim/unclaim/force-load
   * are one-shot mutations where aborting the client's wait would not undo an
   * effect that already reached the server, so callers are expected to guard
   * against duplicate submission themselves (e.g. disabling the button while
   * a request is in flight) rather than relying on cancellation.
   */
  private async post(path: string, body: unknown): Promise<{ status: number; data: unknown }> {
    const res = await fetch(this.url(path), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => null);
    return { status: res.status, data };
  }

  /** Extracts a server error message from a failed response body, if any. */
  private errorMessage(status: number, data: unknown): string {
    if (isRecord(data) && typeof data.error === 'string') return data.error;
    return `request failed (${status})`;
  }

  /**
   * Creates a claim covering exactly the given chunks. Fails with the
   * specific conflicting chunks if any of them are already claimed, so the
   * caller can retry with a clean selection instead of guessing which ones.
   */
  async claimChunks(
    dimension: string,
    chunks: ChunkRef[],
    name?: string,
    owner?: string,
  ): Promise<ActionResult<Area>> {
    const { status, data } = await this.post('/api/chunks/claim', { dimension, chunks, name, owner });
    if (status === 200 && isRecord(data) && typeof data.id === 'string') {
      return { ok: true, value: data as unknown as Area };
    }
    const conflicts = isRecord(data) ? asArray<ChunkRef>(data.conflicts) : undefined;
    return { ok: false, message: this.errorMessage(status, data), conflicts };
  }

  /** Removes the given chunks from whichever claim(s) they belong to. */
  async unclaimChunks(dimension: string, chunks: ChunkRef[]): Promise<ActionResult<void>> {
    const { status, data } = await this.post('/api/chunks/unclaim', { dimension, chunks });
    if (status === 200) return { ok: true, value: undefined };
    return { ok: false, message: this.errorMessage(status, data) };
  }

  /** Marks or clears the force-loaded flag on the given chunks. */
  async setForceLoaded(dimension: string, chunks: ChunkRef[], loaded: boolean): Promise<ActionResult<void>> {
    const { status, data } = await this.post('/api/chunks/forceload', { dimension, chunks, loaded });
    if (status === 200) return { ok: true, value: undefined };
    return { ok: false, message: this.errorMessage(status, data) };
  }

  /**
   * Resolves a batch of chunks to one representative ground level each.
   *
   * One request for the whole set rather than a pick per chunk: a full 12x12
   * chunk selection is 144 chunks, and asking individually spends 144 round
   * trips on something the server answers in a single pass. A POST only
   * because the chunk list belongs in a body rather than a query string --
   * this reads world data and changes nothing.
   *
   * Returns an empty array rather than throwing when the request fails, since
   * every caller can fall back to the reference plane and a missing overlay
   * elevation is not worth surfacing as an error.
   */
  async chunkHeights(dimension: string, chunks: ChunkRef[]): Promise<ChunkHeight[]> {
    const { status, data } = await this.post('/api/chunks/heights', { dimension, chunks });
    if (status !== 200 || !isRecord(data)) return [];
    return asArray<ChunkHeight>(data.heights).filter(
      (h) => isRecord(h) && typeof h.x === 'number' && typeof h.z === 'number' && typeof h.y === 'number',
    );
  }

  /** Loads the bootstrap configuration. */
  async config(): Promise<ClientConfig> {
    const cfg = await this.get<ClientConfig>(null, '/api/config');
    if (!cfg) throw new Error('configuration request was aborted');
    // Almost everything the app does reads from this object with no further
    // checking, so a malformed response is worth failing on loudly and early
    // rather than letting it surface as a confusing crash somewhere in the
    // engine or UI construction that follows.
    if (
      !isRecord(cfg) ||
      typeof cfg.tileUrlTemplate !== 'string' ||
      !Array.isArray(cfg.dimensions) ||
      !isRecord(cfg.overlays) ||
      !isRecord(cfg.iso)
    ) {
      throw new Error('server returned a malformed configuration');
    }
    return cfg;
  }

  /** Lists enabled dimensions. */
  async dimensions(): Promise<DimensionInfo[]> {
    return asArray<DimensionInfo>(await this.get<DimensionInfo[]>(null, '/api/dimensions'));
  }

  /** Queries overlay features intersecting a viewport. */
  async features(
    dimension: string,
    bounds: BlockBounds,
    zoom: number,
  ): Promise<FeatureSet | null> {
    const set = await this.get<FeatureSet>('features', '/api/features', {
      dimension,
      minX: Math.floor(bounds.minX),
      minZ: Math.floor(bounds.minZ),
      maxX: Math.ceil(bounds.maxX),
      maxZ: Math.ceil(bounds.maxZ),
      zoom: Math.floor(zoom),
      limit: 4000,
    });
    if (!set || !isRecord(set)) return null;
    // Coerce rather than reject: a single unexpected field should not throw
    // away areas/markers/players that did come back in the expected shape.
    return {
      areas: asArray(set.areas),
      markers: asArray(set.markers),
      players: asArray(set.players),
      truncated: set.truncated === true,
    };
  }

  /** Fetches current players. */
  async players(dimension: string): Promise<Player[]> {
    return asArray<Player>(await this.get<Player[]>('players', '/api/players', { dimension }));
  }

  /** Fetches claims. */
  async claims(dimension: string): Promise<Area[]> {
    return asArray<Area>(await this.get<Area[]>('claims', '/api/claims', { dimension }));
  }

  /** Fetches server regions. */
  async regions(dimension: string): Promise<Area[]> {
    return asArray<Area>(await this.get<Area[]>('regions', '/api/regions', { dimension }));
  }

  /** Fetches markers. */
  async markers(dimension: string): Promise<Marker[]> {
    return asArray<Marker>(await this.get<Marker[]>('markers', '/api/markers', { dimension }));
  }

  /** Runs a search. */
  async search(dimension: string, q: string): Promise<SearchResult[]> {
    return asArray<SearchResult>(
      await this.get<SearchResult[]>('search', '/api/search', { dimension, q }),
    );
  }

  /**
   * Resolves a top-down map position to a Minecraft block.
   *
   * `key` separates the throttled hover probe from a click, so a stream of
   * hover requests can never cancel the click the user actually made.
   */
  async pickTop(
    dimension: string,
    x: number,
    z: number,
    key: string | null = 'pick',
  ): Promise<PickResult | null> {
    return this.validatedPick(
      await this.get<PickResult>(key, '/api/pick', {
        dimension,
        mode: 'top',
        x: Math.floor(x),
        z: Math.floor(z),
      }),
    );
  }

  /**
   * Resolves an isometric map position to the block visible at that pixel.
   *
   * The server ray-marches the terrain height field, so this reports the
   * mountainside the user is pointing at rather than the ground that happens to
   * lie under that pixel on a flat plane.
   */
  async pickIso(
    dimension: string,
    u: number,
    v: number,
    key: string | null = 'pick',
  ): Promise<PickResult | null> {
    return this.validatedPick(
      await this.get<PickResult>(key, '/api/pick', {
        dimension,
        mode: 'iso',
        u: u.toFixed(3),
        v: v.toFixed(3),
      }),
    );
  }

  /**
   * Guards a pick response's shape before it reaches callers that index
   * straight into `.found`/`.x`/`.y`/`.z` -- a malformed response would
   * otherwise crash there instead of at the point the bad data was received.
   */
  private validatedPick(result: PickResult | null): PickResult | null {
    if (!result || !isRecord(result) || typeof result.found !== 'boolean') return null;
    return result;
  }

  /** Cancels every in-flight request, used when the dimension or mode changes. */
  abortAll(): void {
    for (const c of this.inflight.values()) c.abort();
    this.inflight.clear();
  }
}
