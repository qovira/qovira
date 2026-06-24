// Storybook preview — project-wide parameters, decorators, globals. This file is the source of truth for story
// rendering context. It is imported by the Vitest setup file so story-as-tests share the same decorators,
// parameters, and globals.

import type { Preview } from "@storybook/sveltekit";

// Import @qovira/theme tokens so CSS custom properties (--color-*, --font-*, etc.) are available in every story
// canvas.
import "@qovira/theme";

const preview: Preview = {
  parameters: {
    controls: { matchers: { color: /(background|color)$/i, date: /Date$/i } },
    // a11y: { test: 'error' } enforces axe violations as test failures. House default per writing-storybook §7.
    a11y: { test: "error" },
  },
};

export default preview;
