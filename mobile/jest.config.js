// jest-expo handles TypeScript and the React Native module graph.
//
// The queue is deliberately split so most of it never touches that graph: the
// policy layer is pure TypeScript and runs as a plain unit test, while only the
// storage adapter needs SQLite. That split is what makes the hard parts —
// backoff, batching, response interpretation — testable without a device.
module.exports = {
  preset: 'jest-expo',
  testMatch: ['**/*.test.ts', '**/*.test.tsx'],
  collectCoverageFrom: ['src/**/*.{ts,tsx}', '!src/**/*.test.{ts,tsx}'],
};
