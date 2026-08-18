import type { ChunkUpdatedEvent, Player, RealtimeEvent } from './types';
import { asArray, isRecord } from './validate';

/** `reconnecting` covers the first few retries after a drop; `offline` is
 * shown once retries have gone on long enough that the connection is
 * probably not coming back on its own soon -- reconnect attempts themselves
 * never stop, only the label shown for them changes. */
export type ConnectionState = 'live' | 'reconnecting' | 'offline';

/** Retries beyond this many relabel the banner from "reconnecting" to
 * "offline" (still retrying underneath) rather than showing "reconnecting"
 * indefinitely, which would read as stuck. At the backoff schedule below,
 * this is roughly half a minute of failed attempts. */
const OFFLINE_AFTER_RETRIES = 4;

type Handlers = {
  onPlayers?: (players: Player[], dimension: string) => void;
  onChunkUpdated?: (ev: ChunkUpdatedEvent, dimension: string) => void;
  onFeatureUpdated?: (kind: string, dimension: string, data: unknown) => void;
  onStatus?: (connected: boolean) => void;
  onConnectionState?: (state: ConnectionState) => void;
};

/**
 * Realtime channel.
 *
 * Reconnects with exponential backoff and jitter so that a server restart does
 * not produce a synchronised reconnect storm from every open browser tab. The
 * map degrades gracefully without it: polling still refreshes players, and
 * terrain updates simply arrive on the next tile request instead of instantly.
 */
export class RealtimeSocket {
  private socket: WebSocket | null = null;
  private handlers: Handlers = {};
  private dimension = '';
  private closedByUs = false;
  private retries = 0;
  private retryTimer: number | null = null;

  constructor(private readonly path = '/ws') {}

  /** Registers event handlers. */
  on(handlers: Handlers): void {
    this.handlers = { ...this.handlers, ...handlers };
  }

  /** Opens the connection, subscribing to a dimension's events. */
  connect(dimension: string): void {
    this.dimension = dimension;
    this.closedByUs = false;
    this.open();
  }

  /** Changes which dimension's events are delivered. */
  subscribe(dimension: string): void {
    this.dimension = dimension;
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify({ type: 'subscribe', dimension }));
    }
  }

  /** Closes the connection and stops reconnecting. */
  close(): void {
    this.closedByUs = true;
    if (this.retryTimer !== null) window.clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.socket?.close();
    this.socket = null;
  }

  private open(): void {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${proto}//${window.location.host}${this.path}?dimension=${encodeURIComponent(this.dimension)}`;

    let ws: WebSocket;
    try {
      ws = new WebSocket(url);
    } catch {
      this.scheduleRetry();
      return;
    }
    this.socket = ws;

    ws.onopen = () => {
      this.retries = 0;
      this.handlers.onStatus?.(true);
      this.handlers.onConnectionState?.('live');
      ws.send(JSON.stringify({ type: 'subscribe', dimension: this.dimension }));
    };

    ws.onmessage = (ev) => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(ev.data as string);
      } catch {
        return;
      }
      // A malformed frame -- wrong shape entirely, not just an unrecognised
      // type -- is dropped here rather than cast and handed to a handler
      // that assumes the envelope is well-formed.
      if (!isRecord(parsed) || typeof parsed.type !== 'string') return;
      this.dispatch(parsed as unknown as RealtimeEvent);
    };

    ws.onclose = () => {
      this.handlers.onStatus?.(false);
      this.socket = null;
      if (!this.closedByUs) {
        this.scheduleRetry();
        this.handlers.onConnectionState?.(this.retries > OFFLINE_AFTER_RETRIES ? 'offline' : 'reconnecting');
      }
    };

    ws.onerror = () => {
      // onclose always follows, which is where reconnection is handled.
      ws.close();
    };
  }

  private dispatch(msg: RealtimeEvent): void {
    const dim = msg.dimension ?? '';
    switch (msg.type) {
      case 'player.move':
        this.handlers.onPlayers?.(asArray<Player>(msg.data), dim);
        break;
      case 'chunk.updated': {
        const ev = asChunkUpdatedEvent(msg.data);
        if (ev) this.handlers.onChunkUpdated?.(ev, dim);
        break;
      }
      case 'claim.updated':
      case 'region.updated':
      case 'marker.updated':
      case 'forceload.updated':
        this.handlers.onFeatureUpdated?.(msg.type.split('.')[0], dim, msg.data);
        break;
      default:
        break;
    }
  }

  private scheduleRetry(): void {
    if (this.retryTimer !== null) return;
    this.retries += 1;
    // Exponential backoff to 30s, with jitter so reconnects spread out.
    const base = Math.min(30_000, 500 * Math.pow(2, Math.min(this.retries, 6)));
    const delay = base * (0.7 + Math.random() * 0.6);
    this.retryTimer = window.setTimeout(() => {
      this.retryTimer = null;
      if (!this.closedByUs) this.open();
    }, delay);
  }
}

/** Validates a chunk.updated payload's shape before it reaches a handler
 * that indexes straight into chunkX/chunkZ/tiles. */
function asChunkUpdatedEvent(data: unknown): ChunkUpdatedEvent | null {
  if (
    !isRecord(data) ||
    typeof data.chunkX !== 'number' ||
    typeof data.chunkZ !== 'number' ||
    typeof data.revision !== 'number'
  ) {
    return null;
  }
  return {
    chunkX: data.chunkX,
    chunkZ: data.chunkZ,
    revision: data.revision,
    tiles: asArray(data.tiles),
  };
}
