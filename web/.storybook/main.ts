import type { StorybookConfig } from "@storybook/sveltekit";

const config: StorybookConfig = {
  framework: "@storybook/sveltekit",
  stories: ["../src/**/*.stories.@(svelte|ts)", "../src/**/*.mdx"],
  addons: [
    // @storybook/addon-svelte-csf must come first — it registers the Svelte CSF indexer that allows Storybook
    // (and the Vitest plugin) to parse *.stories.svelte.
    "@storybook/addon-svelte-csf",
    "@storybook/addon-docs",
    "@storybook/addon-a11y",
    "@storybook/addon-vitest",
  ],
};

export default config;
