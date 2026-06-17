/**
 * Shared constants for the E2E suite. Kept in a non-test file so journey
 * specs can import from here without Playwright complaining about importing
 * from a test file (*.setup.ts is considered a test file by Playwright).
 */

import path from "path";
import { fileURLToPath } from "url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

/** Path where auth.setup.ts writes the browser storageState. */
export const AUTH_STATE_FILE = path.join(__dirname, ".auth", "session.json");
