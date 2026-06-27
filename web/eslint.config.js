import js from "@eslint/js";
import ts from "typescript-eslint";
import svelte from "eslint-plugin-svelte";
import globals from "globals";
import svelteConfig from "./svelte.config.js";

export default ts.config(
  js.configs.recommended,
  // Type-checked rules apply only to TypeScript and Svelte files
  {
    files: ["**/*.ts", "**/*.svelte", "**/*.svelte.ts"],
    extends: ts.configs.strictTypeChecked,
  },
  ...svelte.configs["flat/recommended"],
  {
    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.node,
      },
    },
  },
  {
    files: ["**/*.svelte", "**/*.svelte.ts"],
    languageOptions: {
      parserOptions: {
        projectService: true,
        extraFileExtensions: [".svelte"],
        parser: ts.parser,
        svelteConfig,
      },
    },
  },
  {
    files: ["**/*.ts"],
    languageOptions: {
      parserOptions: {
        projectService: true,
      },
    },
  },
  {
    rules: {
      curly: ["error", "all"],
    },
  },
  {
    ignores: [".svelte-kit/**", "coverage/**", "node_modules/**"],
  },
);
