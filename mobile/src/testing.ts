/**
 * Test helpers.
 *
 * `noUncheckedIndexedAccess` makes `arr[0]` return `T | undefined`, which is
 * correct and worth keeping — the queue indexes into arrays constantly. In
 * tests it would otherwise mean either a non-null assertion on every access,
 * which defeats the setting, or a soup of optional chaining that turns a
 * genuine "nothing was sent" bug into a silently passing assertion.
 */

/** Returns the element at `index`, failing loudly rather than yielding undefined. */
export function at<T>(items: readonly T[], index: number, label = 'array'): T {
  const value = items[index];
  if (value === undefined) {
    throw new Error(`expected ${label}[${index}] to exist, but length is ${items.length}`);
  }
  return value;
}
