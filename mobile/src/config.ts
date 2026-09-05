/**
 * Runtime configuration.
 *
 * The API host is a real problem in development and worth being explicit about:
 * a phone on the same Wi-Fi cannot reach `localhost` — that resolves to the
 * phone itself. It needs the development machine's LAN address. The Android
 * emulator is different again: `10.0.2.2` is its alias for the host loopback.
 *
 * Set EXPO_PUBLIC_API_URL in mobile/.env. Expo inlines EXPO_PUBLIC_* variables
 * at build time, so this is a compile-time constant, not a runtime lookup —
 * which also means changing it requires restarting the bundler.
 */

import { Platform } from 'react-native';

const FALLBACK = Platform.select({
  // The emulator's alias for the host machine's loopback interface.
  android: 'http://10.0.2.2:8080',
  default: 'http://localhost:8080',
});

export const API_BASE_URL = process.env.EXPO_PUBLIC_API_URL ?? FALLBACK;

/**
 * Whether the fallback is in use, so the debug screen can warn. On a physical
 * device the fallback is always wrong, and the resulting symptom — every drain
 * failing with a network error — looks identical to a server that is down.
 */
export const API_URL_IS_FALLBACK = !process.env.EXPO_PUBLIC_API_URL;

export const DEVICE_ID_SETTING = 'device_id';
