/**
 * Dynamic Expo config.
 *
 * `app.json` stays the static base and this file layers one thing on top: the
 * Google Maps API key, read from the environment so it never reaches a commit.
 * Expo merges the two, passing the parsed `app.json` in as `config`.
 *
 * **What this does and does not protect.** The key is kept out of git, which is
 * what CLAUDE.md's "no secrets in the repo" rule asks for. It is *not* kept out
 * of the APK — a client-side maps SDK needs the key on the device, so anyone
 * with the artifact can extract it. That is unavoidable, not an oversight.
 *
 * The thing that actually makes the key safe is restricting it in Google Cloud
 * to this package name and signing certificate, which turns an extracted key
 * into a useless string. Treat that restriction as mandatory. Note that the
 * release build currently signs with the *debug* keystore, so the SHA-1
 * registered today changes when a real keystore is generated before shipping.
 */

module.exports = ({ config }) => {
  const apiKey = process.env.GOOGLE_MAPS_API_KEY;

  // Fail loudly at build time rather than shipping an app whose map is a grey
  // rectangle with an unhelpful logcat line. A missing key is a setup mistake,
  // and setup mistakes should be noisy — this project has lost two field tests
  // to configuration that failed silently.
  if (!apiKey && process.env['EXPO_SKIP_MAPS_KEY_CHECK'] !== '1') {
    throw new Error(
      'GOOGLE_MAPS_API_KEY is not set. Put it in mobile/.env (gitignored); ' +
        'see .env.example. Set EXPO_SKIP_MAPS_KEY_CHECK=1 to build without a map.',
    );
  }

  return {
    ...config,
    android: {
      ...config.android,
      config: {
        ...config.android?.config,
        googleMaps: { apiKey },
      },
    },
  };
};
