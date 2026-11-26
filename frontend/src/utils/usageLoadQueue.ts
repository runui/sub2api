/**
 * Usage request scheduler.
 *
 * Requests execute immediately here. The account list batches desktop queries;
 * the backend decides whether to use passive data or query the provider.
 */

import type { Account } from '@/types'

/**
 * Schedule a usage fetch. All requests execute immediately.
 */
export function enqueueUsageRequest<T>(
  _account: Account,
  fn: () => Promise<T>
): Promise<T> {
  return fn()
}
