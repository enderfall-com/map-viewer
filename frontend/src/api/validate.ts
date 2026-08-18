/**
 * Minimal runtime guards for data crossing the network boundary.
 *
 * The backend and frontend are deployed and versioned independently, so a
 * shape mismatch -- a renamed field, a proxy returning a JSON error body with
 * a 200 status, a stale cached response -- is possible even though both ends
 * speak JSON. Without a check here, `res.json() as T` trusts whatever comes
 * back and the wrong shape crashes far from the actual cause, deep inside
 * rendering code that gives no hint a network response was ever involved.
 * These guards are deliberately shallow: enough to catch "this is not the
 * shape we asked for" without becoming a full schema validator.
 */

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** Returns `value` if it is an array, otherwise `fallback`. */
export function asArray<T>(value: unknown, fallback: T[] = []): T[] {
  return Array.isArray(value) ? (value as T[]) : fallback;
}
