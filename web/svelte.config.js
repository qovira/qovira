import adapter from "@sveltejs/adapter-static";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),

  kit: {
    adapter: adapter({
      fallback: "index.html",
    }),
    // Output stays at the default web/build — wiring into the Go embed is out of scope for this
    // issue (handled by Server Foundation).
  },
};

export default config;
