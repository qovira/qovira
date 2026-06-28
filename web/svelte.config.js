import adapter from "@sveltejs/adapter-static";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      pages: "../internal/httpx/webdist",
      assets: "../internal/httpx/webdist",
      fallback: "index.html",
      precompress: false,
      strict: false,
    }),
  },
};

export default config;
